package livewire

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/govengers/auth"
)

// handle dispatches one request frame. Ping and unsubscribe answer at once; every other op
// runs on its own goroutine, at most InFlightPerSocket at a time (D10), under a fresh
// identity (D6).
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
		s.respondError(frame.ID, invalidParameter("op", "is not an operation of the socket"))
	}
}

func (s *socket) observe(unit unitOfWork, err error) {
	s.engine.registry.Observe(s.ctx, unit.outcome(auth.WithIdentity(s.ctx, s.lastIdentity()), err))
}

// start runs an op on its own goroutine within the in-flight limit.
func (s *socket) start(frame *RequestFrame) {
	limit := s.engine.options.InFlightPerSocket
	s.mutex.Lock()
	admitted := !s.closed && s.inFlight < limit
	if admitted {
		s.inFlight++
	}
	s.mutex.Unlock()
	if !admitted {
		s.respondError(frame.ID, limitExceeded("requests", "at most "+strconv.Itoa(limit)+" in flight per socket"))
		return
	}
	s.spawn(func() {
		defer func() {
			s.mutex.Lock()
			s.inFlight--
			s.mutex.Unlock()
		}()
		s.run(frame)
	})
}

// run re-validates the identity and runs the op under it.
func (s *socket) run(frame *RequestFrame) {
	identity, err := s.freshIdentity(s.ctx)
	if err != nil {
		s.observe(startUnit(OutcomeRequest, frame.Operation, frame.Target), err)
		s.respondError(frame.ID, err)
		return
	}
	ctx := auth.WithIdentity(s.ctx, identity)
	switch frame.Operation {
	case OperationSubscribe:
		s.subscribe(ctx, frame)
	case OperationQuery:
		s.query(ctx, frame)
	case OperationCall:
		s.call(ctx, frame)
	case OperationBatch:
		s.batch(ctx, frame)
	case OperationJoin:
		s.join(ctx, frame)
	case OperationLeave:
		s.leave(ctx, frame)
	case OperationPing, OperationUnsubscribe:
	}
}

func (s *socket) query(ctx context.Context, frame *RequestFrame) {
	request := readRequestOf(frame)
	unit := startUnit(OutcomeRequest, OperationQuery, frame.Target)
	var resolved *resolvedRead
	var result *readResult
	release, err := s.engine.acquire(ctx)
	if err == nil {
		resolved, result, err = s.engine.read(ctx, request)
		release()
	}
	s.engine.registry.Observe(ctx, unit.readOutcome(ctx, resolved, err))
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	s.respond(frame.ID, http.StatusOK, result.payload)
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

func (s *socket) call(ctx context.Context, frame *RequestFrame) {
	release, err := s.engine.acquire(ctx)
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	defer release()
	request := CallRequest{Target: frame.Target, Source: MapSource(frame.Parameters), Body: frame.Body, IdempotencyKey: frame.IdempotencyKey}
	status, payload, err := s.engine.call(ctx, OperationCall, request)
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	s.respond(frame.ID, status, payload)
}

func (s *socket) batch(ctx context.Context, frame *RequestFrame) {
	release, err := s.engine.acquire(ctx)
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	defer release()
	payload, err := s.engine.batch(ctx, OperationBatch, BatchRequest{Body: frame.Body, IdempotencyKey: frame.IdempotencyKey})
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	s.respond(frame.ID, http.StatusOK, payload)
}

// presenceRecord names the record a socket joined: the tenant, the declaration and the key.
type presenceRecord struct {
	tenant string
	target string
	key    string
}

// join announces the socket on a record of a readable declaration (D37); the frame's body is
// the client's state. The declaration's static Check gates it like a read.
func (s *socket) join(ctx context.Context, frame *RequestFrame) {
	unit := startUnit(OutcomeRequest, OperationJoin, frame.Target)
	record, err := s.presenceRecordOf(ctx, frame)
	if err == nil {
		ctx = WithTenant(ctx, record.tenant)
		err = s.joinRecord(ctx, record, frame.Body)
	}
	s.engine.registry.Observe(ctx, unit.outcome(ctx, err))
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	s.respond(frame.ID, http.StatusOK, nil)
}

func (s *socket) joinRecord(ctx context.Context, record presenceRecord, state json.RawMessage) error {
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
	release, err := s.engine.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := s.engine.options.PresenceStore.Join(ctx, presence); err != nil {
		return err
	}
	s.mutex.Lock()
	s.presences[record] = struct{}{}
	s.mutex.Unlock()
	return nil
}

func (s *socket) leave(ctx context.Context, frame *RequestFrame) {
	unit := startUnit(OutcomeRequest, OperationLeave, frame.Target)
	record, err := s.presenceRecordOf(ctx, frame)
	if err == nil {
		ctx = WithTenant(ctx, record.tenant)
		err = s.leaveRecord(ctx, record)
	}
	s.engine.registry.Observe(ctx, unit.outcome(ctx, err))
	if err != nil {
		s.respondError(frame.ID, err)
		return
	}
	s.respond(frame.ID, http.StatusOK, nil)
}

func (s *socket) leaveRecord(ctx context.Context, record presenceRecord) error {
	release, err := s.engine.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := s.engine.options.PresenceStore.Leave(ctx, record.tenant, s.id, record.target, record.key); err != nil {
		return err
	}
	s.mutex.Lock()
	delete(s.presences, record)
	s.mutex.Unlock()
	return nil
}

// presenceRecordOf resolves the record a join or leave names: a readable declaration whose
// Check passes, the key parameter and the tenant.
func (s *socket) presenceRecordOf(ctx context.Context, frame *RequestFrame) (presenceRecord, error) {
	request := ReadRequest{Target: frame.Target, Kind: ReadPresence, Source: MapSource(frame.Parameters)}
	declaration, err := s.engine.readDeclaration(request)
	if err != nil {
		return presenceRecord{}, err
	}
	if err := declaration.Check()(ctx); err != nil {
		return presenceRecord{}, err
	}
	key, err := presenceKeyFrom(request.Source)
	if err != nil {
		return presenceRecord{}, err
	}
	tenant, err := s.engine.registry.Tenant(ctx, nil)
	if err != nil {
		return presenceRecord{}, err
	}
	return presenceRecord{tenant: tenant, target: declaration.Name(), key: key}, nil
}

// leavePresences removes the presences of an ended socket; what fails is swept after
// PresenceExpiry.
func (s *socket) leavePresences() {
	s.mutex.Lock()
	records := make([]presenceRecord, 0, len(s.presences))
	for record := range s.presences {
		records = append(records, record)
	}
	clear(s.presences)
	s.mutex.Unlock()
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), leaveTimeout)
	defer cancel()
	for _, record := range records {
		_ = s.engine.options.PresenceStore.Leave(ctx, record.tenant, s.id, record.target, record.key) // the sweep removes a presence a failed leave left behind
	}
}

// hasPresences reports whether the socket joined a record.
func (s *socket) hasPresences() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.presences) > 0
}

// refreshPresences is the presence tick (D37): a heartbeat for every socket with a presence,
// then the sweep of the presences older than PresenceExpiry.
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
		if err := store.Heartbeat(ctx, clientSocket.id); err != nil && failure == nil {
			failure = err
		}
	}
	if _, err := store.Sweep(ctx, e.options.PresenceExpiry); err != nil && failure == nil {
		failure = err
	}
	e.registry.Observe(ctx, unit.outcome(ctx, failure))
}
