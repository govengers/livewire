package livewire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"sync"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/query"
)

// subscription is one live read of one socket (section 5). It is indexed by its tables, each
// in its partition, before its first load, so a change committed during that load at least
// marks it dirty. A presence subscription is indexed by the presence table and by its
// declaration's tables: a presence change re-lists the presences only, a change of the
// record's tables, an identity change and a resync also run the record read again. Its goroutine waits on dirty, a one-slot channel: the changes that arrive while a
// re-run is in flight collapse into one follow-up, and an idle subscription re-runs at once
// (the leading edge). current is what the last re-run read, delivered what the client was
// last sent; the socket's writer turns the difference into the next push.
type subscription struct {
	id     string
	socket *socket
	// request is what every re-run reads: a stored or inline filter resolved at subscribe
	// travels inline, so a re-run never asks the FilterStore.
	request       ReadRequest
	tenant        string // resolved at subscribe; a re-run under another tenant ends it
	list          bool
	presence      bool
	partitions    map[string]string // the partition of each table (D25)
	timeDependent bool
	//nolint:containedctx // the CON-002 exception of the repository: the subscription's lifetime, cancelled by end
	ctx    context.Context
	cancel context.CancelFunc
	dirty  chan struct{}

	mutex         sync.Mutex
	ended         bool
	resynchronize bool // a resync asked for a snapshot; the next re-run takes it
	snapshotDue   bool // the next push is a snapshot (lists) or a replace (objects)
	current       *view
	delivered     *view
	sequence      int64
	failures      int
	backingOff    bool        // a retry is pending: dirty marks wait for it
	stopRetry     func() bool // the pending retry's timer
	recordDue     bool        // a presence re-run runs the record read again
	filterHash    string      // the inline filter this subscription counts against FiltersPerSocket
}

// rerunMarks are what a re-run owes beyond reading again: a snapshot push, a record read.
type rerunMarks struct {
	snapshot bool
	record   bool
}

// view is one read as a subscription keeps it: the REST body and, for a list, its state.
type view struct {
	payload json.RawMessage
	state   *ListState
}

// errTenantChanged ends a subscription whose re-run resolves another tenant than its
// subscribe did: its partition no longer names what it reads.
var errTenantChanged = &auth.Error{Kind: auth.ErrForbidden, Reason: "the tenant of the subscription changed"}

// subscribe opens a subscription: the response and the first push are queued together, so
// the wire order is response, first push, then every later change.
func (s *socket) subscribe(ctx context.Context, frame *RequestFrame) {
	unit := startUnit(OutcomeSubscribe, OperationSubscribe, frame.Target)
	opening := &installation{socket: s, frameID: frame.ID, request: readRequestOf(frame)}
	err := s.engine.inSlot(ctx, func() error { return opening.install(ctx) })
	if opening.subscription != nil {
		unit.subscription = opening.subscription.id
	}
	s.engine.observe(ctx, unit.readOutcome(ctx, opening.resolved, err))
	if err != nil {
		s.respondError(frame.ID, err)
	}
}

// installation carries a subscribe from its frame to the published subscription; resolved
// and subscription are set as far as it got.
type installation struct {
	socket       *socket
	frameID      string
	request      ReadRequest
	resolved     *resolvedRead
	subscription *subscription
}

// install registers, indexes, loads and publishes the subscription; on any exit that did
// not publish, a panic included, it takes the subscription off the socket and the index.
func (i *installation) install(ctx context.Context) (err error) {
	if i.resolved, err = i.socket.engine.resolve(ctx, i.request); err != nil {
		return err
	}
	liveSubscription, err := i.socket.newSubscription(ctx, i.resolved)
	if err != nil {
		return err
	}
	if err := i.socket.addSubscription(liveSubscription); err != nil {
		return err
	}
	i.subscription = liveSubscription
	published := false
	defer func() {
		if published {
			return
		}
		if err != nil && liveSubscription.isEnded() {
			err = NotFound() // unsubscribed before it started
		}
		i.socket.removeSubscription(liveSubscription.id)
		liveSubscription.end()
	}()
	i.socket.engine.index(liveSubscription)
	frames, err := i.firstFrames(ctx)
	if err != nil {
		return err
	}
	if err := i.publish(frames); err != nil {
		return err
	}
	published = true
	return nil
}

// firstFrames loads the first read under the subscription's lifetime, registers an inline
// filter (D13) and encodes the response and the first push.
func (i *installation) firstFrames(ctx context.Context) ([][]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(i.subscription.ctx, cancel)()
	result, err := i.socket.engine.load(ctx, i.resolved)
	if err != nil {
		return nil, err
	}
	filterID, err := i.socket.registerFilter(ctx, i.resolved, i.subscription)
	if err != nil {
		return nil, err
	}
	push, err := i.subscription.firstPush(result)
	if err != nil {
		return nil, err
	}
	response, err := json.Marshal(ResponseFrame{ID: i.frameID, OK: true, Status: http.StatusOK, Subscription: i.subscription.id, FilterID: filterID})
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the subscribe response: %w", err)
	}
	return [][]byte{response, push}, nil
}

// publish queues the response and the first push unless the subscription ended meanwhile
// (an unsubscribe ends it under the same lock before it answers, so its answer and these
// frames never cross), then starts the re-runs.
func (i *installation) publish(frames [][]byte) error {
	liveSubscription := i.subscription
	liveSubscription.mutex.Lock()
	if liveSubscription.ended {
		liveSubscription.mutex.Unlock()
		return NotFound()
	}
	withinLimit := i.socket.outbox.enqueue(frames...)
	liveSubscription.mutex.Unlock()
	if !withinLimit {
		i.socket.close(CloseTooSlow, "the socket does not read its responses")
	}
	i.socket.spawn(liveSubscription.run)
	return nil
}

// readRequestOf is the read a subscribe or query frame names: the target's suffix after the
// dot is the ReadKind (D46), the parameters are the frame's, copied.
func readRequestOf(frame *RequestFrame) ReadRequest {
	name, kind := parseTarget(frame.Target)
	return ReadRequest{Target: name, Kind: kind, Source: MapSource(maps.Clone(frame.Parameters))}
}

type partitioned interface {
	Partition() PartitionResolver
}

// newSubscription builds the subscription of a resolved read: only a live declaration (one
// with Tables) or a presence read can be subscribed. The declaration's tables are in the
// partition of its resolver, or the tenant (D25); the presence table in the tenant.
func (s *socket) newSubscription(ctx context.Context, resolved *resolvedRead) (*subscription, error) {
	plan := resolved.plan
	presence := plan.kind == ReadPresence
	if len(plan.declaration.Tables()) == 0 && !presence {
		return nil, invalidParameter("target", "is not live")
	}
	timeDependent, err := isTimeDependent(resolved)
	if err != nil {
		return nil, err
	}
	partition := resolved.tenant
	if declaration, ok := plan.declaration.(partitioned); ok && declaration.Partition() != nil {
		partition = declaration.Partition()(WithTenant(ctx, resolved.tenant), resolved.parameters)
	}
	partitions := map[string]string{}
	for _, table := range plan.declaration.Tables() {
		partitions[table] = partition
	}
	if presence {
		partitions[PresenceTable] = resolved.tenant
	}
	request := resolved.request
	request.Source = rerunSource(resolved)
	return &subscription{
		socket: s, request: request, tenant: resolved.tenant, partitions: partitions, presence: presence,
		timeDependent: timeDependent, list: plan.answersList, dirty: make(chan struct{}, 1),
	}, nil
}

// rerunSource is the subscription's parameters with a stored filter replaced by its document
// (D13: resolved once, at subscribe).
func rerunSource(resolved *resolvedRead) ParameterSource {
	source, isMap := resolved.request.Source.(MapSource)
	stored := resolved.readParameters != nil && resolved.readParameters.FilterID != ""
	if !isMap || !stored {
		return resolved.request.Source
	}
	inline := maps.Clone(source)
	delete(inline, ParameterFilterID)
	inline[ParameterFilter] = string(resolved.readParameters.Filter)
	return inline
}

// isTimeDependent reports whether a resource read's filter carries a relative time, so the
// safety tick re-runs it.
func isTimeDependent(resolved *resolvedRead) (bool, error) {
	resource := resolved.plan.resource
	if resource == nil || resolved.readParameters == nil || len(resolved.readParameters.Filter) == 0 {
		return false, nil
	}
	document, err := query.Parse(resource.Fields(), resolved.readParameters.Filter)
	if err != nil {
		return false, err
	}
	return document.IsTimeDependent(), nil
}

// registerFilter stores the inline filter of a subscribed resource read for its owner and
// returns its id (D13); without a FilterStore, an owner or an inline filter there is none.
// The live subscriptions of a socket hold at most FiltersPerSocket distinct inline filters;
// one more is 429, and a subscription's end releases its filter.
func (s *socket) registerFilter(ctx context.Context, resolved *resolvedRead, liveSubscription *subscription) (string, error) {
	parameters := resolved.readParameters
	inline := parameters != nil && parameters.FilterID == "" && len(parameters.Filter) > 0
	ctx = WithTenant(ctx, resolved.tenant)
	owner, hasOwner := OwnerOf(ctx)
	store := s.engine.options.FilterStore
	if !inline || !hasOwner || store == nil {
		return "", nil
	}
	document, err := query.Parse(resolved.plan.resource.Fields(), parameters.Filter)
	if err != nil {
		return "", err
	}
	canonical, err := query.Canonical(document)
	if err != nil {
		return "", fmt.Errorf("livewire: the canonical form of a filter: %w", err)
	}
	hash, err := query.Hash(document)
	if err != nil {
		return "", fmt.Errorf("livewire: the hash of a filter: %w", err)
	}
	if err := s.reserveFilter(hash); err != nil {
		return "", err
	}
	liveSubscription.mutex.Lock()
	liveSubscription.filterHash = hash // released by end, whatever happens next
	liveSubscription.mutex.Unlock()
	var stored *StoredFilter
	err = recovering(func() error {
		var saveError error
		stored, saveError = store.Save(ctx, &StoredFilter{Owner: owner, Resource: resolved.request.Target, Document: canonical, Hash: hash})
		return saveError
	})
	if err != nil {
		return "", fmt.Errorf("livewire: store the filter of a subscription to %s: %w", resolved.request.Target, err)
	}
	return stored.ID, nil
}

// reserveFilter counts a filter hash for one more live subscription; a hash a live
// subscription holds already is free.
func (s *socket) reserveFilter(hash string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	limit := s.engine.options.FiltersPerSocket
	if s.filterHashes[hash] == 0 && len(s.filterHashes) >= limit {
		return limitExceeded("filters", "at most "+strconv.Itoa(limit)+" inline filters per socket at once")
	}
	s.filterHashes[hash]++
	return nil
}

func (s *socket) releaseFilter(hash string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.filterHashes[hash]--; s.filterHashes[hash] <= 0 {
		delete(s.filterHashes, hash)
	}
}

// addSubscription registers a subscription with the socket, refusing one past
// SubscriptionsPerSocket; an accepted one gets its id and its lifetime.
func (s *socket) addSubscription(liveSubscription *subscription) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	limit := s.engine.options.SubscriptionsPerSocket
	switch {
	case s.closed:
		return errSocketClosed
	case len(s.subscriptions) >= limit:
		return limitExceeded("subscriptions", "at most "+strconv.Itoa(limit)+" per socket")
	}
	s.lastSubscription++
	liveSubscription.id = strconv.Itoa(s.lastSubscription)
	liveSubscription.ctx, liveSubscription.cancel = context.WithCancel(s.ctx)
	s.subscriptions[liveSubscription.id] = liveSubscription
	return nil
}

// removeSubscription takes a subscription off the socket; nil when there is none.
func (s *socket) removeSubscription(id string) *subscription {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	liveSubscription := s.subscriptions[id]
	delete(s.subscriptions, id)
	return liveSubscription
}

// firstPush takes the first read as delivered and encodes its push: a list's snapshot or an
// object's replace.
func (s *subscription) firstPush(result *readResult) ([]byte, error) {
	first, err := s.viewOf(result)
	if err != nil {
		return nil, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.current, s.delivered, s.sequence = first, first, 1
	kind := PushReplace
	if s.list {
		kind = PushSnapshot
	}
	return encodePush(s.id, 1, kind, first.payload)
}

func (s *subscription) viewOf(result *readResult) (*view, error) {
	if !s.list {
		return &view{payload: result.payload}, nil
	}
	state, err := NewListState(result.items, result.total)
	if err != nil {
		return nil, err
	}
	return &view{payload: result.payload, state: state}, nil
}

func encodePush(id string, sequence int64, kind PushKind, payload json.RawMessage) ([]byte, error) {
	encoded, err := json.Marshal(PushFrame{Subscription: id, Sequence: sequence, Kind: kind, Payload: payload})
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal a push: %w", err)
	}
	return encoded, nil
}

func (s *subscription) isEnded() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.ended
}

// markDirty asks for a re-run; it never blocks. While a retry is pending the mark waits for
// it: the retry reads every change since (RetryInterval is enforced). The signal is sent
// under the lock, so it cannot land after retryLater drained the channel.
func (s *subscription) markDirty() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if !s.backingOff {
		s.signal()
	}
}

// markChanged is markDirty for a change of table; for a presence subscription a change of
// the record's tables also runs the record read again.
func (s *subscription) markChanged(table string) {
	if s.presence && table != PresenceTable {
		s.mutex.Lock()
		s.recordDue = true
		s.mutex.Unlock()
	}
	s.markDirty()
}

// markIdentityChanged re-runs the subscription under a new identity, its record read
// included.
func (s *subscription) markIdentityChanged() {
	s.mutex.Lock()
	s.recordDue = true
	s.mutex.Unlock()
	s.markDirty()
}

func (s *subscription) signal() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// markResynchronize asks for a re-run whose push is a snapshot.
func (s *subscription) markResynchronize() {
	s.mutex.Lock()
	s.resynchronize, s.recordDue = true, true
	s.mutex.Unlock()
	s.markDirty()
}

// run re-runs the subscription whenever it is dirty, until it ends.
func (s *subscription) run() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.dirty:
			s.rerun()
		}
	}
}

// rerun reads again under the socket's current identity: a success becomes the next push, a
// refusal (4xx, a changed tenant included) ends the subscription with an error push, a
// failure (5xx, a panicking loader included) is retried with a backoff (section 5).
func (s *subscription) rerun() {
	unit := startUnit(OutcomeRerun, "", s.request.Target)
	unit.subscription = s.id
	marks := s.takeMarks()
	ctx := s.ctx
	identity, err := s.socket.rerunIdentity(ctx)
	if errors.Is(err, errIdentityGone) {
		return
	}
	var resolved *resolvedRead
	var next *view
	if err == nil {
		ctx = auth.WithIdentity(ctx, identity)
		resolved, next, err = s.read(ctx, marks.record)
	}
	if s.ctx.Err() != nil {
		return
	}
	switch {
	case err == nil:
		s.settle(next, marks.snapshot)
	case statusOf(err) < http.StatusInternalServerError:
		s.endWithError(err)
	default:
		s.retryLater(marks)
	}
	s.socket.engine.observe(ctx, unit.readOutcome(ctx, resolved, err))
}

// read resolves and loads the subscription's read; a presence re-run without a record mark
// lists the presences without the record read.
func (s *subscription) read(ctx context.Context, recordDue bool) (*resolvedRead, *view, error) {
	var resolved *resolvedRead
	var next *view
	err := s.socket.engine.inSlot(ctx, func() error {
		var err error
		if resolved, err = s.socket.engine.resolve(ctx, s.request); err != nil {
			return err
		}
		if resolved.tenant != s.tenant {
			return errTenantChanged
		}
		load := s.socket.engine.load
		if s.presence && !recordDue {
			load = s.socket.engine.loadPresenceOnly
		}
		result, err := load(ctx, resolved)
		if err != nil {
			return err
		}
		next, err = s.viewOf(result)
		return err
	})
	return resolved, next, err
}

func (s *subscription) takeMarks() rerunMarks {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	marks := rerunMarks{snapshot: s.resynchronize, record: s.recordDue}
	s.resynchronize, s.recordDue = false, false
	return marks
}

// settle takes a re-run's read as current and asks the writer for a push when it differs
// from what was delivered; a success ends any pending retry.
func (s *subscription) settle(next *view, snapshot bool) {
	s.mutex.Lock()
	if s.ended {
		s.mutex.Unlock()
		return
	}
	s.failures, s.backingOff = 0, false
	if s.stopRetry != nil {
		s.stopRetry()
		s.stopRetry = nil
	}
	s.current = next
	s.snapshotDue = s.snapshotDue || snapshot
	changed := s.snapshotDue || !bytes.Equal(next.payload, s.delivered.payload)
	s.mutex.Unlock()
	if changed {
		s.socket.outbox.markPending(s)
	}
}

// nextPush turns the difference between delivered and current into the next push, or nil
// when nothing changed. The socket's writer calls it when the delivery is due, so every
// change since the last push leaves as one.
func (s *subscription) nextPush() []byte {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	current, delivered := s.current, s.delivered
	unchanged := !s.snapshotDue && bytes.Equal(current.payload, delivered.payload)
	if s.ended || unchanged {
		return nil
	}
	kind, payload := s.pushContent(current, delivered)
	s.delivered, s.snapshotDue = current, false
	if kind == "" {
		return nil
	}
	s.sequence++
	encoded, err := encodePush(s.id, s.sequence, kind, payload)
	if err != nil {
		return nil // a payload json.Marshal wrote marshals again; a push it cannot encode is not sent
	}
	return encoded
}

// pushContent is the kind and payload of the push from delivered to current; s.mutex is
// held. A list pushes a snapshot when one is due, else the diff; an object its replace.
func (s *subscription) pushContent(current, delivered *view) (PushKind, json.RawMessage) {
	switch {
	case !s.list:
		return PushReplace, current.payload
	case s.snapshotDue:
		return PushSnapshot, current.payload
	}
	changes := current.state.ChangesSince(delivered.state)
	if changes == nil {
		return "", nil
	}
	payload, err := json.Marshal(changes)
	if err != nil {
		return PushSnapshot, current.payload // a diff that does not encode is replaced by the whole list
	}
	return PushListChange, payload
}

// endWithError ends the subscription with an error push carrying the refusal's body.
func (s *subscription) endWithError(err error) {
	_, body := ResponseFor(err)
	payload, _ := json.Marshal(body) //nolint:errchkjson // an ErrorBody always marshals
	s.mutex.Lock()
	if s.ended {
		s.mutex.Unlock()
		return
	}
	s.sequence++
	encoded, encodeError := encodePush(s.id, s.sequence, PushError, payload)
	s.mutex.Unlock()
	s.socket.removeSubscription(s.id)
	s.end()
	if encodeError == nil {
		s.socket.enqueue(encoded)
	}
}

// retryLater re-runs a failed subscription after a backoff with its one timer; a snapshot
// or record read it owed stays owed, and dirty marks wait for the retry.
func (s *subscription) retryLater(marks rerunMarks) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.ended {
		return
	}
	s.resynchronize = s.resynchronize || marks.snapshot
	s.recordDue = s.recordDue || marks.record
	if s.stopRetry != nil {
		s.stopRetry()
	}
	s.backingOff = true
	select {
	case <-s.dirty: // a mark that arrived during the failed re-run waits for the retry too
	default:
	}
	delay := backoff(s.socket.engine.options.RetryInterval, s.failures)
	s.failures++
	s.stopRetry = s.socket.engine.clock.AfterFunc(delay, s.retryNow)
}

func (s *subscription) retryNow() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.backingOff, s.stopRetry = false, nil
	s.signal()
}

// end stops the subscription for good, once: no change reaches it, its goroutine returns,
// its inline filter is released.
func (s *subscription) end() {
	s.mutex.Lock()
	if s.ended {
		s.mutex.Unlock()
		return
	}
	s.ended = true
	if s.stopRetry != nil {
		s.stopRetry()
		s.stopRetry = nil
	}
	filterHash := s.filterHash
	s.mutex.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	s.socket.engine.unindex(s)
	if filterHash != "" {
		s.socket.releaseFilter(filterHash)
	}
}
