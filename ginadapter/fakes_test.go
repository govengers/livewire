package ginadapter_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/govengers/auth"

	"github.com/govengers/livewire"
)

// The in-memory adapters of the engine behind the routes: what a test needs of them and no
// more.

const subjectHeader = "X-Subject"

// identities answers the identity registered for a request's X-Subject.
type identities struct {
	mutex  sync.Mutex
	people map[string]*auth.Identity
}

func (i *identities) get(subject string) *auth.Identity {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	if identity, ok := i.people[subject]; ok {
		return identity.Clone()
	}
	return nil
}

func (i *identities) Identify(_ context.Context, request *http.Request) (*auth.Identity, error) {
	if identity := i.get(request.Header.Get(subjectHeader)); identity != nil {
		return identity, nil
	}
	return nil, auth.ErrUnauthenticated
}

// transactor begins transactions whose commit runs their hooks.
type transactor struct {
	mutex   sync.Mutex
	details []livewire.TransactionDetails
}

func (t *transactor) Begin(_ context.Context, details livewire.TransactionDetails) (livewire.Transaction, error) {
	t.mutex.Lock()
	t.details = append(t.details, details)
	t.mutex.Unlock()
	return &transaction{}, nil
}

type transaction struct {
	mutex      sync.Mutex
	onCommit   []func()
	onRollback []func()
}

func (t *transaction) Handle() any { return t }

func (t *transaction) hooks(onCommit, onRollback func()) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.onCommit = append(t.onCommit, onCommit)
	t.onRollback = append(t.onRollback, onRollback)
}

func (t *transaction) Commit(context.Context) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, hook := range t.onCommit {
		hook()
	}
	return nil
}

func (t *transaction) Rollback(context.Context) error {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, hook := range t.onRollback {
		hook()
	}
	return nil
}

func transactionOf(ctx context.Context) *transaction {
	current, _ := livewire.TransactionFrom(ctx).(*transaction)
	return current
}

// idempotency keeps a record per owner and key; a rollback frees it.
type idempotency struct {
	mutex   sync.Mutex
	records map[string]*livewire.IdempotencyRecord
}

func (i *idempotency) Reserve(ctx context.Context, owner livewire.Owner, key, mutation string) (*livewire.IdempotencyRecord, bool, error) {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	id := owner.Key + "|" + owner.Tenant + "|" + key
	if record, ok := i.records[id]; ok {
		if record.Mutation != mutation {
			return nil, false, livewire.Conflict()
		}
		replayed := *record
		return &replayed, true, nil
	}
	transactionOf(ctx).hooks(func() {}, func() {
		i.mutex.Lock()
		delete(i.records, id)
		i.mutex.Unlock()
	})
	i.records[id] = &livewire.IdempotencyRecord{Owner: owner, Key: key, Mutation: mutation}
	return nil, false, nil
}

func (i *idempotency) Record(_ context.Context, record *livewire.IdempotencyRecord) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	stored := i.records[record.Owner.Key+"|"+record.Owner.Tenant+"|"+record.Key]
	stored.Status, stored.Response = record.Status, record.Response
	return nil
}

// filters keeps stored filters by id.
type filters struct {
	mutex   sync.Mutex
	stored  map[string]livewire.StoredFilter
	counter int
}

func (f *filters) Save(_ context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.counter++
	saved := *filter
	saved.ID = "filter-" + strconv.Itoa(f.counter)
	saved.CreatedAt, saved.LastUsedAt = time.Unix(1, 0).UTC(), time.Unix(1, 0).UTC()
	f.stored[saved.ID] = saved
	return &saved, nil
}

func (f *filters) Get(_ context.Context, owner livewire.Owner, id string) (*livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	stored, ok := f.stored[id]
	if !ok || stored.Owner != owner {
		return nil, livewire.NotFound()
	}
	return &stored, nil
}

func (f *filters) List(_ context.Context, owner livewire.Owner, resource string) ([]livewire.StoredFilter, error) {
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

func (f *filters) Delete(_ context.Context, owner livewire.Owner, id string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if stored, ok := f.stored[id]; ok && stored.Owner == owner {
		delete(f.stored, id)
	}
	return nil
}

// presences keeps the presences in memory.
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

var errDatabaseGone = errors.New("the database went away")
