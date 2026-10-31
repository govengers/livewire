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
	"time"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/query"
)

// subscription is one live read of one socket (section 5). It is indexed by its tables and
// partition before its first load, so a change committed during that load at least marks it
// dirty. Its goroutine waits on dirty, a one-slot channel: the changes that arrive while a
// re-run is in flight collapse into one follow-up, and an idle subscription re-runs at once
// (the leading edge). current is what the last re-run read, delivered what the client was
// last sent; the socket's writer turns the difference into the next push.
type subscription struct {
	id            string
	socket        *socket
	request       ReadRequest // what every re-run reads; an inline filter stays inline
	list          bool
	tables        []string
	partition     string
	timeDependent bool
	//nolint:containedctx // the subscription's lifetime: cancelled by an unsubscribe or the socket's close
	ctx    context.Context
	cancel context.CancelFunc
	dirty  chan struct{}

	mutex       sync.Mutex
	ended       bool
	resync      bool // a resync asked for a snapshot; the next re-run takes it
	snapshotDue bool // the next push is a snapshot (lists) or a replace (objects)
	current     *view
	delivered   *view
	sequence    int64
	failures    int
	retry       *time.Timer
}

// view is one read as a subscription keeps it: the REST body and, for a list, its state.
type view struct {
	payload json.RawMessage
	state   *ListState
}

// subscribe opens a subscription: the response and the first push are queued together, so
// the wire order is response, first push, then every later change.
func (s *socket) subscribe(ctx context.Context, frame *RequestFrame) {
	unit := startUnit(OutcomeSubscribe, OperationSubscribe, frame.Target)
	opening := &installation{socket: s, frameID: frame.ID, request: readRequestOf(frame)}
	err := opening.install(ctx)
	if opening.subscription != nil {
		unit.subscription = opening.subscription.id
	}
	s.engine.registry.Observe(WithTenant(ctx, opening.tenant()), unit.readOutcome(ctx, opening.resolved, err))
	if err != nil {
		s.respondError(frame.ID, err)
	}
}

// installation is one subscribe on its way.
type installation struct {
	socket       *socket
	frameID      string
	request      ReadRequest
	resolved     *resolvedRead
	subscription *subscription
}

func (i *installation) tenant() string {
	if i.resolved == nil {
		return ""
	}
	return i.resolved.tenant
}

func (i *installation) install(ctx context.Context) error {
	engine := i.socket.engine
	release, err := engine.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if i.resolved, err = engine.resolve(ctx, i.request); err != nil {
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
	engine.index(liveSubscription)
	frames, err := i.firstFrames(ctx)
	if err != nil {
		i.socket.removeSubscription(liveSubscription.id)
		liveSubscription.end()
		return err
	}
	i.socket.enqueue(frames...)
	i.socket.spawn(liveSubscription.run)
	return nil
}

// firstFrames loads the first read, registers an inline filter (D13) and encodes the
// response and the first push.
func (i *installation) firstFrames(ctx context.Context) ([][]byte, error) {
	result, err := i.socket.engine.load(ctx, i.resolved)
	if err != nil {
		return nil, err
	}
	filterID, err := i.socket.engine.registerFilter(ctx, i.resolved)
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

// readRequestOf turns a frame into the read it names.
func readRequestOf(frame *RequestFrame) ReadRequest {
	name, kind := parseTarget(frame.Target)
	return ReadRequest{Target: name, Kind: kind, Source: MapSource(maps.Clone(frame.Parameters))}
}

type partitioned interface {
	Partition() PartitionResolver
}

// newSubscription builds the subscription of a resolved read: only a live declaration (one
// with Tables) or a presence read can be subscribed. Its partition is the declaration's
// resolver's, or the tenant (D25).
func (s *socket) newSubscription(ctx context.Context, resolved *resolvedRead) (*subscription, error) {
	tables := resolved.declaration.Tables()
	if resolved.request.Kind == ReadPresence {
		tables = []string{PresenceTable}
	}
	if len(tables) == 0 {
		return nil, invalidParameter("target", "is not live")
	}
	timeDependent, err := isTimeDependent(resolved)
	if err != nil {
		return nil, err
	}
	partition := resolved.tenant
	if declaration, ok := resolved.declaration.(partitioned); ok && declaration.Partition() != nil && resolved.request.Kind != ReadPresence {
		partition = declaration.Partition()(WithTenant(ctx, resolved.tenant), resolved.parameters)
	}
	subscriptionContext, cancel := context.WithCancel(s.ctx)
	return &subscription{
		socket: s, request: resolved.request, tables: tables, partition: partition, timeDependent: timeDependent,
		list: isListRead(resolved),
		ctx:  subscriptionContext, cancel: cancel, dirty: make(chan struct{}, 1),
	}, nil
}

// isListRead reports whether a read answers a ListResponse, which is diffed by id.
func isListRead(resolved *resolvedRead) bool {
	switch resolved.request.Kind {
	case ReadPresence:
		return true
	case ReadItem, ReadDistinct:
		return false
	case ReadMain:
		return resolved.declaration.Kind() != KindObject
	}
	return false
}

// isTimeDependent reports whether a resource read's filter carries a relative time, so the
// safety tick re-runs it.
func isTimeDependent(resolved *resolvedRead) (bool, error) {
	resource, ok := resolved.declaration.(*Resource)
	if !ok || resolved.readParameters == nil || len(resolved.readParameters.Filter) == 0 {
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
func (e *Engine) registerFilter(ctx context.Context, resolved *resolvedRead) (string, error) {
	parameters := resolved.readParameters
	inline := parameters != nil && parameters.FilterID == "" && len(parameters.Filter) > 0
	ctx = WithTenant(ctx, resolved.tenant)
	owner, hasOwner := OwnerOf(ctx)
	if !inline || !hasOwner || e.options.FilterStore == nil {
		return "", nil
	}
	resource, _ := resolved.declaration.(*Resource)
	document, err := query.Parse(resource.Fields(), parameters.Filter)
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
	stored, err := e.options.FilterStore.Save(ctx, &StoredFilter{Owner: owner, Resource: resource.Name(), Document: canonical, Hash: hash})
	if err != nil {
		return "", fmt.Errorf("livewire: store the filter of a subscription: %w", err)
	}
	return stored.ID, nil
}

// addSubscription registers a subscription with the socket, refusing one past
// SubscriptionsPerSocket.
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
func (sub *subscription) firstPush(result *readResult) ([]byte, error) {
	first, err := sub.viewOf(result)
	if err != nil {
		return nil, err
	}
	sub.mutex.Lock()
	defer sub.mutex.Unlock()
	sub.current, sub.delivered, sub.sequence = first, first, 1
	kind := PushReplace
	if sub.list {
		kind = PushSnapshot
	}
	return encodePush(sub.id, 1, kind, first.payload)
}

func (sub *subscription) viewOf(result *readResult) (*view, error) {
	if !sub.list {
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

// markDirty asks for a re-run; it never blocks.
func (sub *subscription) markDirty() {
	select {
	case sub.dirty <- struct{}{}:
	default:
	}
}

// markResync asks for a re-run whose push is a snapshot.
func (sub *subscription) markResync() {
	sub.mutex.Lock()
	sub.resync = true
	sub.mutex.Unlock()
	sub.markDirty()
}

// run re-runs the subscription whenever it is dirty, until it ends.
func (sub *subscription) run() {
	for {
		select {
		case <-sub.ctx.Done():
			return
		case <-sub.dirty:
			sub.rerun()
		}
	}
}

// rerun reads again under the socket's current identity: a success becomes the next push, a
// refusal (4xx) ends the subscription with an error push, a failure (5xx, a panicking
// loader included) is retried with a backoff (section 5).
func (sub *subscription) rerun() {
	unit := startUnit(OutcomeRerun, "", sub.request.Target)
	unit.subscription = sub.id
	snapshot := sub.takeResync()
	ctx := sub.ctx
	identity, err := sub.socket.rerunIdentity(ctx)
	if errors.Is(err, errIdentityGone) {
		return
	}
	var resolved *resolvedRead
	var next *view
	if err == nil {
		ctx = auth.WithIdentity(ctx, identity)
		resolved, next, err = sub.read(ctx)
	}
	sub.socket.engine.registry.Observe(ctx, unit.readOutcome(ctx, resolved, err))
	if sub.ctx.Err() != nil {
		return
	}
	status, _ := ResponseFor(err)
	switch {
	case err == nil:
		sub.settle(next, snapshot)
	case status < http.StatusInternalServerError:
		sub.endWithError(err)
	default:
		sub.retryLater(snapshot)
	}
}

func (sub *subscription) read(ctx context.Context) (*resolvedRead, *view, error) {
	release, err := sub.socket.engine.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	resolved, result, err := sub.socket.engine.read(ctx, sub.request)
	if err != nil {
		return resolved, nil, err
	}
	next, err := sub.viewOf(result)
	return resolved, next, err
}

func (sub *subscription) takeResync() bool {
	sub.mutex.Lock()
	defer sub.mutex.Unlock()
	resync := sub.resync
	sub.resync = false
	return resync
}

// settle takes a re-run's read as current and asks the writer for a push when it differs
// from what was delivered.
func (sub *subscription) settle(next *view, snapshot bool) {
	sub.mutex.Lock()
	if sub.ended {
		sub.mutex.Unlock()
		return
	}
	sub.failures = 0
	sub.current = next
	sub.snapshotDue = sub.snapshotDue || snapshot
	changed := sub.snapshotDue || !bytes.Equal(next.payload, sub.delivered.payload)
	sub.mutex.Unlock()
	if changed {
		sub.socket.outbox.markPending(sub)
	}
}

// nextPush turns the difference between delivered and current into the next push, or nil
// when nothing changed. The socket's writer calls it when the delivery is due, so every
// change since the last push leaves as one.
func (sub *subscription) nextPush() []byte {
	sub.mutex.Lock()
	defer sub.mutex.Unlock()
	current, delivered := sub.current, sub.delivered
	unchanged := !sub.snapshotDue && bytes.Equal(current.payload, delivered.payload)
	if sub.ended || unchanged {
		return nil
	}
	kind, payload := sub.pushContent(current, delivered)
	sub.delivered, sub.snapshotDue = current, false
	if kind == "" {
		return nil
	}
	sub.sequence++
	encoded, err := encodePush(sub.id, sub.sequence, kind, payload)
	if err != nil {
		return nil // a payload json.Marshal wrote marshals again; a push it cannot encode is not sent
	}
	return encoded
}

// pushContent is the kind and payload of the push from delivered to current; sub.mutex is
// held. A list pushes a snapshot when one is due, else the diff; an object its replace.
func (sub *subscription) pushContent(current, delivered *view) (PushKind, json.RawMessage) {
	switch {
	case !sub.list:
		return PushReplace, current.payload
	case sub.snapshotDue:
		return PushSnapshot, current.payload
	}
	change := current.state.ChangesSince(delivered.state)
	if change == nil {
		return "", nil
	}
	payload, err := json.Marshal(change)
	if err != nil {
		return PushSnapshot, current.payload // a diff that does not encode is replaced by the whole list
	}
	return PushListChange, payload
}

// endWithError ends the subscription with an error push carrying the refusal's body.
func (sub *subscription) endWithError(err error) {
	_, body := ResponseFor(err)
	payload, _ := json.Marshal(body) //nolint:errchkjson // an ErrorBody always marshals
	sub.mutex.Lock()
	if sub.ended {
		sub.mutex.Unlock()
		return
	}
	sub.sequence++
	encoded, encodeError := encodePush(sub.id, sub.sequence, PushError, payload)
	sub.mutex.Unlock()
	sub.socket.removeSubscription(sub.id)
	sub.end()
	if encodeError == nil {
		sub.socket.enqueue(encoded)
	}
}

// retryLater re-runs a failed subscription after a backoff; a snapshot it owed stays owed.
func (sub *subscription) retryLater(snapshot bool) {
	sub.mutex.Lock()
	defer sub.mutex.Unlock()
	if sub.ended {
		return
	}
	sub.resync = sub.resync || snapshot
	delay := backoff(sub.socket.engine.options.RetryInterval, sub.failures)
	sub.failures++
	sub.retry = time.AfterFunc(delay, sub.markDirty)
}

// end stops the subscription for good: no change reaches it, its goroutine returns.
func (sub *subscription) end() {
	sub.mutex.Lock()
	sub.ended = true
	if sub.retry != nil {
		sub.retry.Stop()
	}
	sub.mutex.Unlock()
	sub.cancel()
	sub.socket.engine.unindex(sub)
}
