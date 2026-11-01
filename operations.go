package livewire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/govengers/auth"
)

// handle dispatches one request frame. Ping and unsubscribe answer at once (D51); every
// other op runs on its own goroutine, at most InFlightPerSocket at a time (D10), under a
// fresh identity (D6). Every frame reports one outcome (D30).
func (s *socket) handle(frame *RequestFrame) {
	switch frame.Operation {
	case OperationPing:
		s.observe(startUnit(OutcomeRequest, OperationPing, ""), nil)
		s.respond(frame.ID, http.StatusOK, nil)
	case OperationUnsubscribe:
		s.unsubscribe(frame)
	case OperationQuery, OperationSubscribe, OperationCall, OperationBatch, OperationJoin, OperationLeave:
		s.start(frame)
	default:
		refusal := invalidParameter("op", "is not an operation of the socket")
		s.observe(startUnit(OutcomeRequest, frame.Operation, frame.Target), refusal)
		s.respondError(frame.ID, refusal)
	}
}

// start runs an op on its own goroutine within the in-flight limit. The op's slot is free
// before its response is queued, so a client that sends its next request on reading the
// answer is never refused for the one it answered. A panic of the op is answered as an
// internal error; the socket stays open.
func (s *socket) start(frame *RequestFrame) {
	limit := s.engine.options.InFlightPerSocket
	s.mutex.Lock()
	admitted := !s.closed && s.inFlight < limit
	if admitted {
		s.inFlight++
	}
	s.mutex.Unlock()
	if !admitted {
		refusal := limitExceeded("requests", "at most "+strconv.Itoa(limit)+" in flight per socket")
		s.observe(startUnit(OutcomeRequest, frame.Operation, frame.Target), refusal)
		s.respondError(frame.ID, refusal)
		return
	}
	release := sync.OnceFunc(func() {
		s.mutex.Lock()
		s.inFlight--
		s.mutex.Unlock()
	})
	s.spawn(func() {
		defer release()
		unit := startUnit(OutcomeRequest, frame.Operation, frame.Target)
		if err := recovering(func() error { s.run(frame, release); return nil }); err != nil {
			s.observe(unit, err)
			release()
			s.respondError(frame.ID, err)
		}
	})
}

// run re-validates the identity, runs the op under it, gives the op's in-flight slot back
// and answers. A subscribe answers itself, together with its first push, and releases the
// slot before it queues them.
func (s *socket) run(frame *RequestFrame, release func()) {
	unit := startUnit(OutcomeRequest, frame.Operation, frame.Target)
	metered := withQueryWait(s.ctx)
	identity, err := s.freshIdentity(metered)
	if err != nil {
		s.observe(unit, err)
		release()
		s.respondError(frame.ID, err)
		return
	}
	ctx := auth.WithIdentity(metered, identity)
	status, payload := http.StatusOK, json.RawMessage(nil)
	switch frame.Operation {
	case OperationSubscribe:
		s.subscribe(ctx, frame, release)
		return
	case OperationQuery:
		payload, err = s.query(ctx, frame)
	case OperationCall:
		status, payload, err = s.call(ctx, frame)
	case OperationBatch:
		payload, err = s.batch(ctx, frame)
	case OperationJoin:
		err = s.join(ctx, frame)
	case OperationLeave:
		err = s.leave(ctx, frame)
	case OperationPing, OperationUnsubscribe:
	}
	release()
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	s.respond(frame.ID, status, payload)
}

func (s *socket) query(ctx context.Context, frame *RequestFrame) (json.RawMessage, error) {
	unit := startUnit(OutcomeRequest, OperationQuery, frame.Target)
	var resolved *resolvedRead
	var result *readResult
	err := s.engine.inSlot(ctx, func() error {
		var err error
		resolved, result, err = s.engine.read(ctx, readRequestOf(frame))
		return err
	})
	s.engine.observe(ctx, unit.readOutcome(ctx, resolved, err))
	if err != nil {
		return nil, err
	}
	return result.payload, nil
}

func (s *socket) unsubscribe(frame *RequestFrame) {
	unit := startUnit(OutcomeRequest, OperationUnsubscribe, frame.Target)
	unit.subscription = frame.Target
	liveSubscription := s.removeSubscription(frame.Target)
	if liveSubscription == nil {
		s.observe(unit, NotFound())
		s.respondError(frame.ID, NotFound())
		return
	}
	liveSubscription.end()
	s.observe(unit, nil)
	s.respond(frame.ID, http.StatusOK, nil)
}

func (s *socket) call(ctx context.Context, frame *RequestFrame) (status int, payload json.RawMessage, err error) {
	err = s.engine.inSlot(ctx, func() error {
		var callError error
		request := CallRequest{Target: frame.Target, Source: MapSource(frame.Parameters), Body: frame.Body, IdempotencyKey: frame.IdempotencyKey}
		status, payload, callError = s.engine.call(ctx, OperationCall, request)
		return callError
	})
	return status, payload, err
}

func (s *socket) batch(ctx context.Context, frame *RequestFrame) (payload json.RawMessage, err error) {
	err = s.engine.inSlot(ctx, func() error {
		var batchError error
		payload, batchError = s.engine.batch(ctx, OperationBatch, BatchRequest{Body: frame.Body, IdempotencyKey: frame.IdempotencyKey})
		return batchError
	})
	return payload, err
}

// presenceRecord names the record a socket joined: the tenant, the declaration and the key.
type presenceRecord struct {
	tenant string
	target string
	key    string
}

// join announces the socket on a record (D37); the frame's body is the client's state. The
// record is resolved like a presence read (the declaration's static Check, its parameters,
// the tenant) and its own read runs under the caller first, so a record the caller may not
// read cannot be joined. A socket joins at most PresencesPerSocket records; joining one
// again updates its state.
func (s *socket) join(ctx context.Context, frame *RequestFrame) error {
	unit := startUnit(OutcomeRequest, OperationJoin, frame.Target)
	var resolved *resolvedRead
	s.presenceMutex.Lock() // before the slot: a join waiting for another holds none
	defer s.presenceMutex.Unlock()
	err := s.engine.inSlot(ctx, func() error {
		var err error
		if resolved, err = s.engine.resolve(ctx, ReadRequest{Target: frame.Target, Kind: ReadPresence, Source: MapSource(frame.Parameters)}); err != nil {
			return err
		}
		if err := s.engine.checkRecord(ctx, resolved); err != nil {
			return err
		}
		record := presenceRecord{tenant: resolved.tenant, target: resolved.request.Target, key: resolved.presenceKey}
		return s.joinRecord(WithTenant(ctx, resolved.tenant), record, frame.Body)
	})
	s.engine.observe(ctx, unit.readOutcome(ctx, resolved, err))
	return err
}

// joinRecord counts and stores the join; the caller holds presenceMutex, so a leave of the
// same record waits until the store answered and the accounting matches it.
func (s *socket) joinRecord(ctx context.Context, record presenceRecord, state json.RawMessage) error {
	fresh, err := s.reservePresence(record)
	if err != nil {
		return err
	}
	owner, _ := OwnerOf(ctx) // a socket always has an identity
	identity := auth.IdentityFrom(ctx)
	display := identity.Name
	if display == "" {
		display = identity.Username
	}
	if len(state) == 0 {
		state = json.RawMessage("null")
	}
	presence := &Presence{Socket: s.id, Owner: owner, Display: display, Target: record.target, Key: record.key, State: state, UpdatedAt: time.Now()}
	err = recovering(func() error { return s.engine.options.PresenceStore.Join(ctx, presence) })
	if err != nil {
		if fresh {
			s.forgetPresence(record)
		}
		return fmt.Errorf("livewire: join %s %q: %w", record.target, record.key, err)
	}
	return nil
}

// reservePresence counts a record against PresencesPerSocket; fresh is false for a record
// the socket joined already, which is an update.
func (s *socket) reservePresence(record presenceRecord) (fresh bool, err error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, joined := s.presences[record]; joined {
		return false, nil
	}
	limit := s.engine.options.PresencesPerSocket
	if len(s.presences) >= limit {
		return false, limitExceeded("presences", "at most "+strconv.Itoa(limit)+" per socket")
	}
	s.presences[record] = struct{}{}
	return true, nil
}

func (s *socket) forgetPresence(record presenceRecord) {
	s.mutex.Lock()
	delete(s.presences, record)
	s.mutex.Unlock()
}

// leave removes the socket's own presence on a record it joined: the record is named like a
// join (the declaration, its parameters, the tenant they resolve, the key), but no check or
// record read runs, so a caller who lost access to the record still leaves it. A record the
// socket did not join is 404.
func (s *socket) leave(ctx context.Context, frame *RequestFrame) error {
	unit := startUnit(OutcomeRequest, OperationLeave, frame.Target)
	s.presenceMutex.Lock() // before the slot, like join
	defer s.presenceMutex.Unlock()
	resolved, err := s.leaveJoined(ctx, frame)
	s.engine.observe(ctx, unit.readOutcome(ctx, resolved, err))
	return err
}

// leaveJoined answers 404 before anything is decoded or resolved when the socket joined no
// record of the declaration under the key; otherwise it resolves the tenant from the
// declaration's parameters to leave exactly that record. The caller holds presenceMutex.
func (s *socket) leaveJoined(ctx context.Context, frame *RequestFrame) (*resolvedRead, error) {
	if !s.joinedAny(frame.Target, frame.Parameters[ParameterPresenceKey]) {
		return nil, NotFound()
	}
	var resolved *resolvedRead
	err := s.engine.inSlot(ctx, func() error {
		request := ReadRequest{Target: frame.Target, Kind: ReadPresence, Source: MapSource(frame.Parameters)}
		plan, err := s.engine.planRead(request)
		if err != nil {
			return err
		}
		if resolved, err = s.engine.decodeRead(ctx, plan, request); err != nil {
			return err
		}
		record := presenceRecord{tenant: resolved.tenant, target: resolved.request.Target, key: resolved.presenceKey}
		if !s.hasJoined(record) {
			return NotFound()
		}
		return s.leaveRecord(WithTenant(ctx, record.tenant), record)
	})
	return resolved, err
}

// joinedAny reports whether the socket joined a record of target under key, in any tenant.
func (s *socket) joinedAny(target, key string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for record := range s.presences {
		if record.target == target && record.key == key {
			return true
		}
	}
	return false
}

func (s *socket) hasJoined(record presenceRecord) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_, joined := s.presences[record]
	return joined
}

// leaveRecord removes one joined record from the store and the accounting; the caller holds
// presenceMutex.
func (s *socket) leaveRecord(ctx context.Context, record presenceRecord) error {
	err := recovering(func() error {
		return s.engine.options.PresenceStore.Leave(ctx, record.tenant, s.id, record.target, record.key)
	})
	if err != nil {
		return fmt.Errorf("livewire: leave %s %q: %w", record.target, record.key, err)
	}
	s.forgetPresence(record)
	return nil
}

// leavePresences removes the presences of an ended socket; a failed leave is reported and
// the sweep removes the row after PresenceExpiry.
func (s *socket) leavePresences() {
	s.presenceMutex.Lock()
	defer s.presenceMutex.Unlock()
	s.mutex.Lock()
	records := make([]presenceRecord, 0, len(s.presences))
	for record := range s.presences {
		records = append(records, record)
	}
	s.mutex.Unlock()
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), leaveTimeout)
	defer cancel()
	for _, record := range records {
		unit := startUnit(OutcomeRequest, OperationLeave, record.target)
		if err := s.leaveRecord(ctx, record); err != nil {
			s.observe(unit, err)
		}
	}
}

func (s *socket) hasPresences() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.presences) > 0
}

// refreshPresences is the presence tick (D37): a heartbeat for every socket with a presence,
// then the sweep of the presences older than PresenceExpiry; the first failure is reported.
func (e *Engine) refreshPresences(ctx context.Context) {
	store := e.options.PresenceStore
	if store == nil {
		return
	}
	unit := startUnit(OutcomeHeartbeat, "", "")
	e.mutex.Lock()
	open := e.openSockets()
	e.mutex.Unlock()
	var failure error
	for _, clientSocket := range open {
		if !clientSocket.hasPresences() {
			continue
		}
		err := recovering(func() error { return store.Heartbeat(ctx, clientSocket.id) })
		if err != nil && failure == nil {
			failure = fmt.Errorf("livewire: the presence heartbeat of a socket: %w", err)
		}
	}
	err := recovering(func() error {
		_, sweepError := store.Sweep(ctx, e.options.PresenceExpiry)
		return sweepError
	})
	if err != nil && failure == nil {
		failure = fmt.Errorf("livewire: the presence sweep: %w", err)
	}
	e.observe(ctx, unit.outcome(ctx, failure))
}
