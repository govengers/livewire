package livewiretest_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/livewiretest"
	"github.com/govengers/livewire/query"
)

// The application the gates run against in these tests and examples: notes (a filterable
// resource), tags (a typed list) and a summary (an object counting both), served on gin in
// front of an engine on in-memory adapters, one tenant per user. Its options break it the
// ways the gates must catch.

// userHeader names the user a request acts as; the test application trusts it.
const userHeader = "X-Test-User"

// tenantOf is the tenant of each user.
var tenantOf = map[string]string{"alice": "north", "bob": "south", "dave": "west"}

type (
	note struct {
		ID    uuid.UUID `json:"id"`
		Title string    `json:"title"`
	}
	tag struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	summary struct {
		Notes int `json:"notes"`
		Tags  int `json:"tags"`
	}
)

// brokenBy names the ways the application is broken; the zero value is a sound application.
type brokenBy struct {
	leakingTenants bool     // the notes loaders ignore the tenant
	unstableNotes  bool     // every notes list read answers another title
	summaryTables  []string // the summary's Tables instead of notes and tags
}

// notesApplication is the running application and its data.
type notesApplication struct {
	livewiretest.Application
	feed   *feed
	mutex  sync.Mutex
	notes  map[string][]note // by tenant
	tags   map[string][]tag  // by tenant
	reads  atomic.Int64
	broken brokenBy
	stop   func()
}

// startNotes starts the application with one note and one tag per tenant.
func startNotes(broken brokenBy) (*notesApplication, error) {
	application := &notesApplication{feed: &feed{connected: make(chan struct{})}, notes: map[string][]note{}, tags: map[string][]tag{}, broken: broken}
	for _, tenant := range []string{"north", "south"} {
		application.notes[tenant] = []note{{ID: uuid.New(), Title: "first of " + tenant}}
		application.tags[tenant] = []tag{{ID: uuid.New(), Name: "urgent"}}
	}
	registry, err := application.registry()
	if err != nil {
		return nil, err
	}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, ChangeFeed: application.feed, IdentitySource: headerIdentity{}, Transactor: transactor{},
		SameHostOnly: true, DeliveryBatchWait: time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(func(ginContext *gin.Context) {
		if identity := identityOf(ginContext.Request); identity != nil {
			ginContext.Request = ginContext.Request.WithContext(auth.WithIdentity(ginContext.Request.Context(), identity))
		}
	})
	if err := ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine: engine}); err != nil {
		return nil, err
	}
	router.GET("/api/socket", ginadapter.SocketHandler(engine))
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	go func() {
		defer close(running)
		_ = engine.Run(ctx) // returns nil after Close
	}()
	application.Application = livewiretest.Application{Engine: engine, Handler: router, SocketPath: "/api/socket"}
	application.stop = func() {
		closing, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = engine.Close(closing) // the application is done; a slow close only delays the test's end
		cancel()
		<-running
	}
	application.feed.waitConnected()
	return application, nil
}

func (a *notesApplication) close() { a.stop() }

func (a *notesApplication) registry() (*livewire.Registry, error) {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{
		BasePath: "/api",
		Tenant: func(ctx context.Context, _ any) (string, error) {
			return tenantOf[auth.IdentityFrom(ctx).Subject], nil
		},
	})
	if err != nil {
		return nil, err
	}
	summaryTables := a.broken.summaryTables
	if summaryTables == nil {
		summaryTables = []string{"notes", "tags"}
	}
	return registry, registry.Add(
		livewire.NewResource("notes", "/notes", &livewire.ResourceOptions[note]{
			Tables: []string{"notes"},
			Read:   authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeUUID, SQL: "notes.id", Filter: true, Sort: true},
				{Name: "title", Type: query.TypeText, SQL: "notes.title", Filter: true, Sort: true},
			},
			DefaultSort: []query.SortTerm{{Field: "title"}},
			Item:        &livewire.ItemKey{Parameter: "id", Field: "id"},
			List:        a.listNotes,
			Get:         a.getNote,
			Distinct:    a.distinctNotes,
			Export:      a.exportNotes,
		}),
		livewire.NewList("tags", "/tags", &livewire.ListOptions[tag, livewire.NoParameters]{
			Tables: []string{"tags"}, Read: authorization.IsUser(),
			Load: func(ctx context.Context, _ *livewire.NoParameters) ([]tag, error) {
				a.mutex.Lock()
				defer a.mutex.Unlock()
				return slices.Clone(a.tags[livewire.TenantFrom(ctx)]), nil
			},
		}),
		livewire.NewObject("summary", "/summary", &livewire.ObjectOptions[summary, livewire.NoParameters]{
			Tables: summaryTables, Read: authorization.IsUser(),
			Load: func(ctx context.Context, _ *livewire.NoParameters) (*summary, error) {
				a.mutex.Lock()
				defer a.mutex.Unlock()
				tenant := livewire.TenantFrom(ctx)
				return &summary{Notes: len(a.notes[tenant]), Tags: len(a.tags[tenant])}, nil
			},
		}),
	)
}

// visibleNotes are the notes the request's tenant reads, every tenant's when they leak.
func (a *notesApplication) visibleNotes(ctx context.Context) []note {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	var visible []note
	for tenant, notes := range a.notes {
		if a.broken.leakingTenants || tenant == livewire.TenantFrom(ctx) {
			visible = append(visible, notes...)
		}
	}
	slices.SortFunc(visible, func(left, right note) int {
		return strings.Compare(left.Title+left.ID.String(), right.Title+right.ID.String())
	})
	return visible
}

func (a *notesApplication) listNotes(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[note], error) {
	visible := a.visibleNotes(ctx)
	if a.broken.unstableNotes {
		for index := range visible {
			visible[index].Title += " #" + strconv.FormatInt(a.reads.Add(1), 10)
		}
	}
	start := min(listQuery.Offset, len(visible))
	return &livewire.Page[note]{Items: visible[start:min(len(visible), start+listQuery.Limit)], Total: len(visible)}, nil
}

func (a *notesApplication) getNote(ctx context.Context, listQuery *query.ListQuery) (*note, error) {
	_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "notes", IDColumn: "notes.id"})
	for _, visible := range a.visibleNotes(ctx) {
		if slices.ContainsFunc(arguments, func(argument any) bool { return fmt.Sprint(argument) == visible.ID.String() }) {
			return &visible, nil
		}
	}
	return nil, livewire.NotFound()
}

func (a *notesApplication) distinctNotes(ctx context.Context, _ string, _ *query.ListQuery) ([]livewire.DistinctValue, error) {
	counts := map[string]int{}
	for _, visible := range a.visibleNotes(ctx) {
		counts[visible.Title]++
	}
	values := make([]livewire.DistinctValue, 0, len(counts))
	for value, count := range counts {
		values = append(values, livewire.DistinctValue{Value: value, Count: count})
	}
	slices.SortFunc(values, func(left, right livewire.DistinctValue) int {
		return strings.Compare(fmt.Sprint(left.Value), fmt.Sprint(right.Value))
	})
	return values, nil
}

func (a *notesApplication) exportNotes(ctx context.Context, _ *query.ListQuery, _ query.Fields, writer io.Writer) (int, error) {
	output := csv.NewWriter(writer)
	visible := a.visibleNotes(ctx)
	if err := output.Write([]string{"id", "title"}); err != nil {
		return 0, err
	}
	for _, exported := range visible {
		if err := output.Write([]string{exported.ID.String(), exported.Title}); err != nil {
			return 0, err
		}
	}
	output.Flush()
	return len(visible), output.Error()
}

// firstNote is the id of the tenant's first note.
func (a *notesApplication) firstNote(tenant string) string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.notes[tenant][0].ID.String()
}

// addNote and addTag are the committed writes of the fixture: the row, then its change.
func (a *notesApplication) addNote(tenant, title string) livewiretest.Write {
	return livewiretest.Write{Table: "notes", Commit: func(context.Context) error {
		added := note{ID: uuid.New(), Title: title}
		a.mutex.Lock()
		a.notes[tenant] = append(a.notes[tenant], added)
		a.mutex.Unlock()
		a.feed.publish(livewire.Change{Table: "notes", Operation: livewire.ChangeInsert, ID: added.ID.String()})
		return nil
	}}
}

func (a *notesApplication) addTag(tenant, name string) livewiretest.Write {
	return livewiretest.Write{Table: "tags", Commit: func(context.Context) error {
		added := tag{ID: uuid.New(), Name: name}
		a.mutex.Lock()
		a.tags[tenant] = append(a.tags[tenant], added)
		a.mutex.Unlock()
		a.feed.publish(livewire.Change{Table: "tags", Operation: livewire.ChangeInsert, ID: added.ID.String()})
		return nil
	}}
}

// as authenticates a request as the user.
func as(user string) func(request *http.Request) {
	return func(request *http.Request) { request.Header.Set(userHeader, user) }
}

func identityOf(request *http.Request) *auth.Identity {
	user := request.Header.Get(userHeader)
	if _, known := tenantOf[user]; !known {
		return nil
	}
	return &auth.Identity{Provider: "test", Subject: user, Active: true}
}

// headerIdentity re-validates a socket by the user of its upgrade request.
type headerIdentity struct{}

func (headerIdentity) Identify(_ context.Context, request *http.Request) (*auth.Identity, error) {
	if identity := identityOf(request); identity != nil {
		return identity, nil
	}
	return nil, auth.ErrUnauthenticated
}

// transactor begins transactions that hold nothing: the data lives in memory.
type transactor struct{}

func (transactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return transaction{}, nil
}

type transaction struct{}

func (transaction) Commit(context.Context) error   { return nil }
func (transaction) Rollback(context.Context) error { return nil }
func (transaction) Handle() any                    { return struct{}{} }

// feed is the change feed the writes publish on.
type feed struct {
	mutex     sync.Mutex
	onChange  func(livewire.Change)
	connected chan struct{}
	once      sync.Once
}

func (f *feed) Run(ctx context.Context, onChange func(livewire.Change), onResync func()) error {
	f.mutex.Lock()
	f.onChange = onChange
	f.mutex.Unlock()
	onResync()
	f.once.Do(func() { close(f.connected) })
	<-ctx.Done()
	return ctx.Err()
}

func (f *feed) waitConnected() { <-f.connected }

func (f *feed) publish(change livewire.Change) {
	f.mutex.Lock()
	onChange := f.onChange
	f.mutex.Unlock()
	onChange(change)
}

// recordingT stands in for the application test's testing.TB: it records the gate's failures
// instead of failing, so a test can require that a gate fails a broken application.
type recordingT struct {
	testing.TB // never called: the gates call only the methods below
	mutex      sync.Mutex
	failures   []string
}

func (r *recordingT) Helper() {}

func (r *recordingT) Errorf(format string, arguments ...any) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, arguments...))
}

func (r *recordingT) Fatalf(format string, arguments ...any) {
	r.Errorf(format, arguments...)
	runtime.Goexit()
}

// gateFailures runs a gate on a goroutine of its own, as the testing package runs a test, so a
// Fatalf ends that goroutine only, and answers what the gate failed with.
func gateFailures(gate func(t testing.TB)) []string {
	recorder := &recordingT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		gate(recorder)
	}()
	<-done
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return recorder.failures
}

// verdict is what an example prints: passed, or the failures.
func verdict(failures []string) string {
	if len(failures) == 0 {
		return "passed"
	}
	return strings.Join(failures, "\n")
}
