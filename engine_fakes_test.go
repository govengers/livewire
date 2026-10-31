package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/govengers/auth"

	"github.com/govengers/livewire"
)

// The in-memory fakes of every contract the engine consumes. The world is the application's
// database: gadgets written through the fake transaction, published to the fake feed after
// the commit like the NOTIFY trigger does.

var errTestConnectionLost = errors.New("connection lost")

type gadget struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Group  string `json:"group"`
	Secret string `json:"secret"`
}

type world struct {
	mutex   sync.Mutex
	gadgets map[string]gadget
	feed    *fakeFeed
	// failures makes the next loads fail with a 5xx; panics makes them panic.
	failures atomic.Int64
	panics   atomic.Int64
	loads    atomic.Int64
	// block, when set, holds every list load until it is closed.
	block atomic.Pointer[chan struct{}]
	// stubborn makes a blocked load ignore its context, like a query that is not cancelled.
	stubborn atomic.Bool
}

func newWorld(feed *fakeFeed) *world {
	return &world{gadgets: map[string]gadget{}, feed: feed}
}

func (w *world) put(item gadget) {
	w.mutex.Lock()
	w.gadgets[item.ID] = item
	w.mutex.Unlock()
}

func (w *world) remove(id string) {
	w.mutex.Lock()
	delete(w.gadgets, id)
	w.mutex.Unlock()
}

// sorted lists the gadgets of a group ("" is every group) by id.
func (w *world) sorted(group string) []gadget {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	items := make([]gadget, 0, len(w.gadgets))
	for _, item := range w.gadgets {
		if group == "" || item.Group == group {
			items = append(items, item)
		}
	}
	slices.SortFunc(items, func(left, right gadget) int {
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	return items
}

func (w *world) lookup(id string) (gadget, bool) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	item, ok := w.gadgets[id]
	return item, ok
}

// load runs before every loader: it counts, fails, panics or blocks as the test asked.
func (w *world) load(ctx context.Context) error {
	w.loads.Add(1)
	if w.panics.Add(-1) >= 0 {
		panic("the loader panicked")
	}
	w.panics.Store(0)
	if w.failures.Add(-1) >= 0 {
		return errors.New("the database is down")
	}
	w.failures.Store(0)
	if block := w.block.Load(); block != nil && w.stubborn.Load() {
		<-*block
		return nil
	}
	if block := w.block.Load(); block != nil {
		select {
		case <-*block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// change publishes a row change of the gadgets table as the trigger would.
func (w *world) change(id, group string) {
	w.feed.publish(livewire.Change{Table: "gadgets", Operation: livewire.ChangeUpdate, ID: id, Partition: group})
}

// fakeFeed is a ChangeFeed whose connects the test drives: failFirst refuses that many
// connects, drop ends the current connection with an error, cleanup (when set) holds Run
// after its context ended until it is closed. publish routes a change synchronously while
// the feed is connected, so a test knows every dirty mark landed when it returns; a change
// published while disconnected is lost, like a NOTIFY without a listener.
type fakeFeed struct {
	failFirst atomic.Int64
	runs      atomic.Int64
	connects  atomic.Int64
	drop      chan error
	connected chan struct{}
	cleanup   atomic.Pointer[chan struct{}]
	mutex     sync.Mutex
	onChange  func(livewire.Change)
}

func newFakeFeed() *fakeFeed {
	return &fakeFeed{drop: make(chan error, 1), connected: make(chan struct{}, 16)}
}

func (f *fakeFeed) Run(ctx context.Context, onChange func(livewire.Change), onResync func()) error {
	f.runs.Add(1)
	if f.failFirst.Add(-1) >= 0 {
		return errors.New("connection refused")
	}
	f.failFirst.Store(0)
	f.connects.Add(1)
	f.mutex.Lock()
	f.onChange = onChange
	f.mutex.Unlock()
	defer func() {
		f.mutex.Lock()
		f.onChange = nil
		f.mutex.Unlock()
	}()
	onResync()
	select {
	case f.connected <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		if cleanup := f.cleanup.Load(); cleanup != nil {
			<-*cleanup
		}
		return ctx.Err()
	case err := <-f.drop:
		return err
	}
}

func (f *fakeFeed) publish(change livewire.Change) {
	f.mutex.Lock()
	onChange := f.onChange
	f.mutex.Unlock()
	if onChange != nil {
		onChange(change)
	}
}

// fakeIdentities is the IdentitySource: the identity of a socket is the one registered for
// the X-Subject of its upgrade request; a subject without one is gone.
type fakeIdentities struct {
	mutex      sync.Mutex
	identities map[string]*auth.Identity
	failure    error
	panics     atomic.Bool
	calls      atomic.Int64
	// hold, when set, holds the next Identify after it read its answer, until it is closed;
	// held reports that one is held.
	hold atomic.Pointer[chan struct{}]
	held chan struct{}
}

func newFakeIdentities() *fakeIdentities {
	return &fakeIdentities{identities: map[string]*auth.Identity{}, held: make(chan struct{}, 1)}
}

func (f *fakeIdentities) set(identity *auth.Identity) {
	f.mutex.Lock()
	f.identities[identity.Subject] = identity
	f.mutex.Unlock()
}

// setFor makes the identity the answer for the X-Subject subject, whoever the identity is.
func (f *fakeIdentities) setFor(subject string, identity *auth.Identity) {
	f.mutex.Lock()
	f.identities[subject] = identity
	f.mutex.Unlock()
}

func (f *fakeIdentities) revoke(subject string) {
	f.mutex.Lock()
	delete(f.identities, subject)
	f.mutex.Unlock()
}

func (f *fakeIdentities) fail(err error) {
	f.mutex.Lock()
	f.failure = err
	f.mutex.Unlock()
}

func (f *fakeIdentities) get(subject string) *auth.Identity {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.identities[subject]
}

func (f *fakeIdentities) Identify(_ context.Context, request *http.Request) (*auth.Identity, error) {
	f.calls.Add(1)
	if f.panics.Load() {
		panic("the identity source panicked")
	}
	identity, err := f.answer(request.Header.Get(subjectHeader))
	if hold := f.hold.Swap(nil); hold != nil {
		f.held <- struct{}{}
		<-*hold
	}
	return identity, err
}

func (f *fakeIdentities) answer(subject string) (*auth.Identity, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.failure != nil {
		return nil, f.failure
	}
	identity, ok := f.identities[subject]
	if !ok {
		return nil, &auth.Error{Kind: auth.ErrUnauthenticated, Reason: "session revoked"}
	}
	return identity.Clone(), nil
}

const subjectHeader = "X-Subject"

// identified puts the identity of the request's X-Subject into its context, as the
// authentication middleware in front of the socket does.
func identified(identities *fakeIdentities, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if identity := identities.get(request.Header.Get(subjectHeader)); identity != nil {
			request = request.WithContext(auth.WithIdentity(request.Context(), identity.Clone()))
		}
		next.ServeHTTP(writer, request)
	})
}

// fakeTransactor begins fakeTransactions; a write is staged and applied on commit.
type fakeTransactor struct {
	world     *world
	mutex     sync.Mutex
	details   []livewire.TransactionDetails
	commits   atomic.Int64
	rollbacks atomic.Int64
	failBegin atomic.Bool
	// panicBegin makes the next Begin panic.
	panicBegin atomic.Bool
}

func (f *fakeTransactor) Begin(_ context.Context, details livewire.TransactionDetails) (livewire.Transaction, error) {
	if f.failBegin.Load() {
		return nil, errors.New("the pool is exhausted")
	}
	if f.panicBegin.Swap(false) {
		panic("the transactor panicked")
	}
	f.mutex.Lock()
	f.details = append(f.details, details)
	f.mutex.Unlock()
	return &fakeTransaction{transactor: f}, nil
}

func (f *fakeTransactor) writes() []livewire.TransactionDetails {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var writes []livewire.TransactionDetails
	for _, details := range f.details {
		if !details.ReadOnly {
			writes = append(writes, details)
		}
	}
	return writes
}

type fakeTransaction struct {
	transactor *fakeTransactor
	mutex      sync.Mutex
	staged     []func()
	onCommit   []func()
	onRollback []func()
	published  []livewire.Change
}

func (t *fakeTransaction) Handle() any { return t }

// stage records a write and the change its commit publishes.
func (t *fakeTransaction) stage(write func(), change livewire.Change) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.staged = append(t.staged, write)
	t.published = append(t.published, change)
}

func (t *fakeTransaction) hooks(onCommit, onRollback func()) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.onCommit = append(t.onCommit, onCommit)
	t.onRollback = append(t.onRollback, onRollback)
}

func (t *fakeTransaction) Commit(context.Context) error {
	t.transactor.commits.Add(1)
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, write := range t.staged {
		write()
	}
	for _, hook := range t.onCommit {
		hook()
	}
	for _, change := range t.published {
		t.transactor.world.feed.publish(change)
	}
	return nil
}

func (t *fakeTransaction) Rollback(context.Context) error {
	t.transactor.rollbacks.Add(1)
	t.mutex.Lock()
	defer t.mutex.Unlock()
	for _, hook := range t.onRollback {
		hook()
	}
	return nil
}

func transactionOf(ctx context.Context) *fakeTransaction {
	transaction, _ := livewire.TransactionFrom(ctx).(*fakeTransaction)
	return transaction
}

// fakeFilters is the FilterStore.
type fakeFilters struct {
	mutex   sync.Mutex
	filters map[string]livewire.StoredFilter
	gets    atomic.Int64
	// panicGet makes the next Get panic.
	panicGet atomic.Bool
}

func newFakeFilters() *fakeFilters {
	return &fakeFilters{filters: map[string]livewire.StoredFilter{}}
}

func (f *fakeFilters) Save(_ context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	for _, stored := range f.filters {
		if stored.Owner == filter.Owner && stored.Resource == filter.Resource && stored.Context == filter.Context && stored.Hash == filter.Hash && filter.Name == "" {
			return &stored, nil
		}
	}
	stored := *filter
	stored.ID = "filter-" + strconv.Itoa(len(f.filters)+1)
	f.filters[stored.ID] = stored
	return &stored, nil
}

func (f *fakeFilters) Get(_ context.Context, owner livewire.Owner, id string) (*livewire.StoredFilter, error) {
	f.gets.Add(1)
	if f.panicGet.Swap(false) {
		panic("the filter store panicked")
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	stored, ok := f.filters[id]
	if !ok || stored.Owner != owner {
		return nil, livewire.NotFound()
	}
	return &stored, nil
}

func (f *fakeFilters) List(_ context.Context, owner livewire.Owner, resource string) ([]livewire.StoredFilter, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var filters []livewire.StoredFilter
	for _, stored := range f.filters {
		if stored.Owner == owner && (resource == "" || stored.Resource == resource) {
			filters = append(filters, stored)
		}
	}
	return filters, nil
}

func (f *fakeFilters) Delete(_ context.Context, owner livewire.Owner, id string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if stored, ok := f.filters[id]; ok && stored.Owner == owner {
		delete(f.filters, id)
	}
	return nil
}

// fakeIdempotency is the IdempotencyStore; a reservation lives in the transaction that made
// it: a rollback frees it, a commit finishes it.
type fakeIdempotency struct {
	mutex   sync.Mutex
	records map[livewire.Owner]map[string]*idempotencyEntry
}

type idempotencyEntry struct {
	record   livewire.IdempotencyRecord
	finished bool
}

func newFakeIdempotency() *fakeIdempotency {
	return &fakeIdempotency{records: map[livewire.Owner]map[string]*idempotencyEntry{}}
}

func (f *fakeIdempotency) Reserve(ctx context.Context, owner livewire.Owner, key, mutation string) (*livewire.IdempotencyRecord, bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.records[owner] == nil {
		f.records[owner] = map[string]*idempotencyEntry{}
	}
	if entry, ok := f.records[owner][key]; ok {
		switch {
		case entry.record.Mutation != mutation:
			return nil, false, livewire.Conflict()
		case !entry.finished:
			return nil, false, livewire.InProgress()
		}
		record := entry.record
		return &record, true, nil
	}
	entry := &idempotencyEntry{record: livewire.IdempotencyRecord{Owner: owner, Key: key, Mutation: mutation}}
	f.records[owner][key] = entry
	transactionOf(ctx).hooks(func() {
		f.mutex.Lock()
		entry.finished = true
		f.mutex.Unlock()
	}, func() {
		f.mutex.Lock()
		delete(f.records[owner], key)
		f.mutex.Unlock()
	})
	return nil, false, nil
}

func (f *fakeIdempotency) Record(_ context.Context, record *livewire.IdempotencyRecord) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	entry := f.records[record.Owner][record.Key]
	entry.record.Status, entry.record.Response = record.Status, record.Response
	return nil
}

// fakePresences is the PresenceStore; every change is published on the feed like the
// presence trigger does.
type fakePresences struct {
	mutex      sync.Mutex
	presences  map[string]livewire.Presence
	feed       *fakeFeed
	heartbeats atomic.Int64
	failLeave  atomic.Bool
	failJoin   atomic.Bool
	// holdJoin, when set, holds the next Join before it stores, until it is closed; joinHeld
	// reports that one is held.
	holdJoin atomic.Pointer[chan struct{}]
	joinHeld chan struct{}
	sweeps   atomic.Int64
}

func newFakePresences(feed *fakeFeed) *fakePresences {
	return &fakePresences{presences: map[string]livewire.Presence{}, feed: feed, joinHeld: make(chan struct{}, 1)}
}

func presenceID(tenant, socket, target, key string) string {
	return tenant + "|" + socket + "|" + target + "|" + key
}

func (f *fakePresences) Join(_ context.Context, presence *livewire.Presence) error {
	if f.failJoin.Load() {
		return errTestConnectionLost
	}
	if hold := f.holdJoin.Swap(nil); hold != nil {
		f.joinHeld <- struct{}{}
		<-*hold
	}
	f.mutex.Lock()
	f.presences[presenceID(presence.Owner.Tenant, presence.Socket, presence.Target, presence.Key)] = *presence
	f.mutex.Unlock()
	f.feed.publish(livewire.Change{Table: livewire.PresenceTable, Operation: livewire.ChangeInsert, Partition: presence.Owner.Tenant})
	return nil
}

func (f *fakePresences) Leave(_ context.Context, tenant, socket, target, key string) error {
	if f.failLeave.Load() {
		return errTestConnectionLost
	}
	f.mutex.Lock()
	delete(f.presences, presenceID(tenant, socket, target, key))
	f.mutex.Unlock()
	f.feed.publish(livewire.Change{Table: livewire.PresenceTable, Operation: livewire.ChangeDelete, Partition: tenant})
	return nil
}

func (f *fakePresences) Heartbeat(_ context.Context, socket string) error {
	f.heartbeats.Add(1)
	f.mutex.Lock()
	defer f.mutex.Unlock()
	for id, presence := range f.presences {
		if presence.Socket == socket {
			presence.UpdatedAt = time.Now()
			f.presences[id] = presence
		}
	}
	return nil
}

func (f *fakePresences) List(_ context.Context, tenant, target, key string) ([]livewire.Presence, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var presences []livewire.Presence
	for _, presence := range f.presences {
		if presence.Owner.Tenant == tenant && presence.Target == target && presence.Key == key {
			presences = append(presences, presence)
		}
	}
	slices.SortFunc(presences, func(left, right livewire.Presence) int {
		return compareText(left.Display+left.Socket, right.Display+right.Socket)
	})
	return presences, nil
}

func (f *fakePresences) Sweep(_ context.Context, maximumAge time.Duration) (int, error) {
	f.sweeps.Add(1)
	f.mutex.Lock()
	swept := 0
	for id, presence := range f.presences {
		if time.Since(presence.UpdatedAt) > maximumAge {
			delete(f.presences, id)
			swept++
		}
	}
	f.mutex.Unlock()
	if swept > 0 {
		f.feed.publish(livewire.Change{Table: livewire.PresenceTable, Operation: livewire.ChangeDelete})
	}
	return swept, nil
}

// age makes every presence older by the duration, as if the heartbeats had stopped.
func (f *fakePresences) age(duration time.Duration) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	for id, presence := range f.presences {
		presence.UpdatedAt = presence.UpdatedAt.Add(-duration)
		f.presences[id] = presence
	}
}

func compareText(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// outcomes collects what the Observer receives.
type outcomes struct {
	mutex    sync.Mutex
	received []livewire.Outcome
	// panics makes the Observer panic after it recorded the outcome.
	panics atomic.Bool
}

func (o *outcomes) observe(_ context.Context, outcome livewire.Outcome) {
	o.mutex.Lock()
	o.received = append(o.received, outcome)
	o.mutex.Unlock()
	if o.panics.Load() {
		panic("the observer panicked")
	}
}

func (o *outcomes) of(kind livewire.OutcomeKind) []livewire.Outcome {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	var matching []livewire.Outcome
	for _, outcome := range o.received {
		if outcome.Kind == kind {
			matching = append(matching, outcome)
		}
	}
	return matching
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
