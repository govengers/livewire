package generate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
)

// The fixture registry served for real: a live engine on in-memory adapters behind the gin
// routes and the socket, so the generated Go client and CLI run against what an
// application serves (the gate of WP7 until examples/todo exists, WP8).

const (
	userKey  = "key-user"
	adminKey = "key-admin"
)

// apiKeys resolves the bearer API key of a request, as an API-key resolver does (D20).
type apiKeys struct {
	mutex      sync.Mutex
	identities map[string]*auth.Identity
}

func (k *apiKeys) identityOf(request *http.Request) *auth.Identity {
	key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	k.mutex.Lock()
	defer k.mutex.Unlock()
	if identity, ok := k.identities[key]; ok {
		return identity.Clone()
	}
	return nil
}

func (k *apiKeys) Identify(_ context.Context, request *http.Request) (*auth.Identity, error) {
	if identity := k.identityOf(request); identity != nil {
		return identity, nil
	}
	return nil, auth.ErrUnauthenticated
}

func (k *apiKeys) revoke(key string) {
	k.mutex.Lock()
	defer k.mutex.Unlock()
	delete(k.identities, key)
}

// transactor begins transactions whose rollback runs their hooks.
type transactor struct{}

func (transactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return &transaction{}, nil
}

type transaction struct {
	mutex      sync.Mutex
	onRollback []func()
}

func (t *transaction) Handle() any { return t }

func (t *transaction) Commit(ctx context.Context) error { return ctx.Err() }

func (t *transaction) Rollback(context.Context) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, hook := range t.onRollback {
		hook()
	}
	return nil
}

// idempotency keeps a record per owner and key; a rollback frees it.
type idempotency struct {
	mutex   sync.Mutex
	records map[string]*livewire.IdempotencyRecord
}

func (i *idempotency) Reserve(ctx context.Context, owner livewire.Owner, key, mutation string) (*livewire.IdempotencyRecord, bool, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	id := owner.Key + "|" + key
	if record, ok := i.records[id]; ok {
		if record.Mutation != mutation {
			return nil, false, livewire.Conflict()
		}
		if record.Status == 0 {
			return nil, false, livewire.InProgress()
		}
		replayed := *record
		return &replayed, true, nil
	}
	if current, ok := livewire.TransactionFrom(ctx).(*transaction); ok {
		current.mutex.Lock()
		current.onRollback = append(current.onRollback, func() {
			i.mutex.Lock()
			delete(i.records, id)
			i.mutex.Unlock()
		})
		current.mutex.Unlock()
	}
	i.records[id] = &livewire.IdempotencyRecord{Owner: owner, Key: key, Mutation: mutation}
	return nil, false, nil
}

func (i *idempotency) Record(_ context.Context, record *livewire.IdempotencyRecord) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	stored := i.records[record.Owner.Key+"|"+record.Key]
	stored.Status, stored.Response = record.Status, record.Response
	return nil
}

// filterStore keeps stored filters by id; evict drops one as the store's cap would (D54).
type filterStore struct {
	mutex   sync.Mutex
	stored  map[string]livewire.StoredFilter
	counter int
}

func (f *filterStore) Save(_ context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	for _, stored := range f.stored {
		if filter.Name == "" && stored.Name == "" && stored.Owner == filter.Owner && stored.Resource == filter.Resource && stored.Hash == filter.Hash {
			return &stored, nil
		}
	}
	f.counter++
	saved := *filter
	saved.ID = "filter-" + strconv.Itoa(f.counter)
	saved.CreatedAt, saved.LastUsedAt = time.Unix(1, 0).UTC(), time.Unix(1, 0).UTC()
	f.stored[saved.ID] = saved
	return &saved, nil
}

func (f *filterStore) Get(_ context.Context, owner livewire.Owner, id string) (*livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	stored, ok := f.stored[id]
	if !ok || stored.Owner != owner {
		return nil, livewire.NotFound()
	}
	return &stored, nil
}

func (f *filterStore) List(_ context.Context, owner livewire.Owner, resource string) ([]livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var listed []livewire.StoredFilter
	for _, stored := range f.stored {
		if stored.Owner == owner && stored.Name != "" && (resource == "" || stored.Resource == resource) {
			listed = append(listed, stored)
		}
	}
	return listed, nil
}

func (f *filterStore) Delete(_ context.Context, owner livewire.Owner, id string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if stored, ok := f.stored[id]; ok && stored.Owner == owner {
		delete(f.stored, id)
	}
	return nil
}

func (f *filterStore) evict(id string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	delete(f.stored, id)
}

// presences holds one presence per record a test placed.
type presences struct {
	mutex  sync.Mutex
	joined []livewire.Presence
}

func (p *presences) Join(_ context.Context, presence *livewire.Presence) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.joined = append(p.joined, *presence)
	return nil
}

func (p *presences) Leave(context.Context, string, string, string, string) error { return nil }

func (p *presences) Heartbeat(context.Context, string) error { return nil }

func (p *presences) List(_ context.Context, tenant, target, key string) ([]livewire.Presence, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	var listed []livewire.Presence
	for _, presence := range p.joined {
		if presence.Owner.Tenant == tenant && presence.Target == target && presence.Key == key {
			listed = append(listed, presence)
		}
	}
	return listed, nil
}

func (p *presences) Sweep(context.Context, time.Duration) (int, error) { return 0, nil }

// changeFeed hands the backend's writes to the engine.
type changeFeed struct {
	changes chan livewire.Change
}

func (f *changeFeed) Run(ctx context.Context, onChange func(livewire.Change), onResync func()) error {
	onResync()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case change := <-f.changes:
			onChange(change)
		}
	}
}

// fixtureServer is the fixture registry behind a real HTTP server.
type fixtureServer struct {
	backend   *fixtureBackend
	filters   *filterStore
	presences *presences
	keys      *apiKeys
	engine    *livewire.Engine
	baseURL   string
}

// startFixtureServer serves the fixture registry; the test fails when a goroutine outlives it.
func startFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	backend := newFixtureBackend()
	feed := &changeFeed{changes: make(chan livewire.Change, 256)}
	backend.changed = func(table string) { feed.changes <- livewire.Change{Table: table, Operation: livewire.ChangeUpdate} }
	keys := &apiKeys{identities: map[string]*auth.Identity{
		userKey:  {Provider: "apikey", Subject: "user", Active: true},
		adminKey: {Provider: "apikey", Subject: "admin", Active: true, Roles: []string{auth.RoleAdmin}},
	}}
	filters := &filterStore{stored: map[string]livewire.StoredFilter{}}
	served := &fixtureServer{backend: backend, filters: filters, presences: &presences{}, keys: keys}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: backend.registry(t), ChangeFeed: feed, IdentitySource: keys, Transactor: transactor{},
		FilterStore: filters, IdempotencyStore: &idempotency{records: map[string]*livewire.IdempotencyRecord{}}, PresenceStore: served.presences,
		SameHostOnly: true, DeliveryBatchWait: 5 * time.Millisecond,
	})
	require.NoError(t, err)
	served.engine = engine
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan error, 1)
	go func() { running <- engine.Run(ctx) }()
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(ginContext *gin.Context) { // stands in for the application's API-key resolver
		if identity := keys.identityOf(ginContext.Request); identity != nil {
			ginContext.Request = ginContext.Request.WithContext(auth.WithIdentity(ginContext.Request.Context(), identity))
		}
	})
	require.NoError(t, ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine: engine}))
	router.GET("/api/v1/socket", ginadapter.SocketHandler(engine))
	server := httptest.NewServer(router)
	served.baseURL = server.URL + "/api/v1"
	t.Cleanup(func() {
		closing, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		require.NoError(t, engine.Close(closing))
		cancel()
		require.NoError(t, <-running)
		server.Close()
	})
	return served
}

// joinPresence places a presence on a record, as a socket's join would.
func (s *fixtureServer) joinPresence(t *testing.T, target, key string) {
	t.Helper()
	require.NoError(t, s.presences.Join(t.Context(), &livewire.Presence{Socket: "socket-1", Owner: livewire.Owner{Key: "apikey:user"}, Display: "user", Target: target, Key: key, State: []byte(`{"typing":true}`), UpdatedAt: time.Unix(1, 0).UTC()}))
}

func newPartID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}
