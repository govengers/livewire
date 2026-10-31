package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/govengers/auth"
)

const (
	pingInterval      = 30 * time.Second
	pongWait          = pingInterval*2 + pingInterval/2
	closeFrameTimeout = time.Second
	// leaveTimeout bounds the presence leaves of a closed socket; the sweep removes what
	// they miss.
	leaveTimeout = 5 * time.Second
	// closeInternalError is the close code of a socket whose own goroutine panicked.
	closeInternalError = websocket.CloseInternalServerErr
)

// errIdentityGone is the identity check's answer when it closed the socket 4401, and
// errSocketClosed the answer of work that found its socket closed; nothing is answered on a
// closed socket.
var (
	errIdentityGone = errors.New("livewire: the socket's identity is gone")
	errSocketClosed = errors.New("livewire: the socket is closed")
)

// Handler is the socket endpoint: it refuses an Origin that is neither allowed nor the
// request's own host with 403 (D9; a request without Origin is a browser's only when it
// carries cookies, so it is refused then and admitted otherwise), a request without an
// identity in its context (auth.IdentityFrom; put the handler behind the same
// authentication as the REST routes) with 401, an owner past SocketsPerOwner with 429 and a
// closing engine with 503, each with the one error body; then it upgrades and serves the
// socket until it ends.
func (e *Engine) Handler() http.Handler {
	return http.HandlerFunc(e.serveSocket)
}

func (e *Engine) serveSocket(writer http.ResponseWriter, request *http.Request) {
	if !e.originAllowed(request) {
		writeError(writer, &auth.Error{Kind: auth.ErrForbidden, Reason: "the origin may not open a socket"})
		return
	}
	identity := auth.IdentityFrom(request.Context())
	if identity == nil {
		writeError(writer, auth.ErrUnauthenticated)
		return
	}
	clientSocket, err := e.admit(request, identity)
	if err != nil {
		writeError(writer, err)
		return
	}
	connection, err := e.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		clientSocket.cancel()
		e.release(clientSocket) // the upgrader answered the handshake failure
		return
	}
	clientSocket.serve(connection)
}

// originAllowed applies D9: an allowed origin or the request's own host; no Origin only for
// a request without cookies, which no browser's socket is.
func (e *Engine) originAllowed(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return len(request.Cookies()) == 0
	}
	if slices.Contains(e.origins, strings.ToLower(origin)) {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}

// admit registers a socket for the upgrade, counted against SocketsPerOwner.
func (e *Engine) admit(request *http.Request, identity *auth.Identity) (*socket, error) {
	ownerKey := OwnerKey(identity)
	e.mutex.Lock()
	defer e.mutex.Unlock()
	switch {
	case e.closing:
		return nil, errUnavailable
	case e.options.SocketsPerOwner > 0 && e.perOwner[ownerKey] >= e.options.SocketsPerOwner:
		return nil, limitExceeded("sockets", "at most "+strconv.Itoa(e.options.SocketsPerOwner)+" per owner")
	}
	clientSocket := newSocket(e, request, identity, ownerKey)
	e.perOwner[ownerKey]++
	e.sockets[clientSocket] = struct{}{}
	e.lifetimes.Add(1)
	return clientSocket, nil
}

// release gives a socket's slot back once its goroutines ended.
func (e *Engine) release(clientSocket *socket) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if _, ok := e.sockets[clientSocket]; !ok {
		return
	}
	delete(e.sockets, clientSocket)
	e.uncountOwner(clientSocket.ownerKey)
	e.lifetimes.Done()
}

// migrateOwner moves a socket's SocketsPerOwner count to the owner the identity of Identify
// call sequence has, unless a later call moved it already; the socket counts there from now
// on, past the limit too, and stays open.
func (e *Engine) migrateOwner(clientSocket *socket, ownerKey string, sequence uint64) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	_, open := e.sockets[clientSocket]
	if !open || sequence < clientSocket.ownerSequence {
		return
	}
	clientSocket.ownerSequence = sequence
	if clientSocket.ownerKey == ownerKey {
		return
	}
	e.uncountOwner(clientSocket.ownerKey)
	clientSocket.ownerKey = ownerKey
	e.perOwner[ownerKey]++
}

// uncountOwner drops one socket of the owner; e.mutex is held.
func (e *Engine) uncountOwner(ownerKey string) {
	if e.perOwner[ownerKey]--; e.perOwner[ownerKey] <= 0 {
		delete(e.perOwner, ownerKey)
	}
}

// socket is one WebSocket connection. Its reader runs on the handler's goroutine, its writer,
// its re-validation, every request and every subscription on goroutines of its own that end
// with the socket. One writer writes every frame, so the order frames are queued in is the
// order on the wire.
type socket struct {
	engine   *Engine
	id       string
	request  *http.Request // the upgrade request the IdentitySource re-validates
	ownerKey string        // guarded by engine.mutex: admit, migrateOwner, release
	// ownerSequence is the Identify call ownerKey came from, guarded by engine.mutex: an
	// older adopt never moves the count back.
	ownerSequence uint64
	connection    *websocket.Conn
	//nolint:containedctx // the CON-002 exception of the repository: the socket's lifetime, cancelled by close
	ctx        context.Context
	cancel     context.CancelFunc
	outbox     *outbox
	goroutines sync.WaitGroup
	closeOnce  sync.Once
	// revalidation has one slot: a tick or Revalidate call asked for a fresh identity.
	revalidation chan struct{}
	// presenceMutex serializes the joins and leaves of the socket, store calls and
	// accounting included, so a leave never overtakes the join it undoes.
	presenceMutex sync.Mutex

	mutex            sync.Mutex
	closed           bool
	identity         *auth.Identity
	identifiedAt     time.Time
	identityIssued   uint64 // the number of the last Identify call started
	identityAdopted  uint64 // the number of the Identify call the identity came from
	subscriptions    map[string]*subscription
	lastSubscription int
	inFlight         int
	presences        map[presenceRecord]struct{}
	filterHashes     map[string]int // live subscriptions per inline filter hash (FiltersPerSocket)
	failure          error          // the panic that closed the socket 1011, for its OutcomeClose
}

func newSocket(engine *Engine, request *http.Request, identity *auth.Identity, ownerKey string) *socket {
	ctx, cancel := context.WithCancel(context.WithoutCancel(request.Context()))
	return &socket{
		engine: engine, id: uuid.NewString(), request: request, ownerKey: ownerKey,
		ctx: ctx, cancel: cancel,
		outbox:       newOutbox(engine.options.QueuedBytesPerSocket, engine.options.DeliveryBatchWait, engine.clock),
		revalidation: make(chan struct{}, 1),
		identity:     identity, identifiedAt: time.Now(),
		subscriptions: map[string]*subscription{}, presences: map[presenceRecord]struct{}{}, filterHashes: map[string]int{},
	}
}

// serve runs the socket until it ends; whatever happens, it then waits for the socket's
// goroutines, leaves its presences and gives its slot back.
func (s *socket) serve(connection *websocket.Conn) {
	defer s.engine.release(s)
	defer s.guarded(s.leavePresences)
	defer s.goroutines.Wait()
	s.mutex.Lock()
	s.connection = connection
	closed := s.closed
	s.mutex.Unlock()
	if closed { // Close ran between the admission and here and found no connection to end
		s.writeClose(CloseGoingAway, "the server is shutting down", time.Now().Add(closeFrameTimeout))
		return
	}
	s.observe(startUnit(OutcomeOpen, "", ""), nil)
	s.spawn(s.write)
	s.spawn(s.revalidate)
	code := closeInternalError
	s.guarded(func() { code = s.read() })
	s.guarded(func() { s.close(code, "") })
}

// spawn runs work on a goroutine the socket waits for; a panic closes the socket 1011.
func (s *socket) spawn(work func()) {
	s.goroutines.Go(func() { s.guarded(work) })
}

func (s *socket) guarded(work func()) {
	err := recovering(func() error {
		work()
		return nil
	})
	if err == nil {
		return
	}
	s.mutex.Lock()
	s.failure = err
	s.mutex.Unlock()
	_ = recovering(func() error { // a close that panics again leaves the cleanup of serve to finish the socket
		s.close(closeInternalError, "internal error")
		return nil
	})
}

// read reads frames until the connection ends and returns the close code it ended with: the
// peer's own, 1009 for a frame past FrameBytes (gorilla answered it), 1006 for a connection
// that broke or timed out without a close frame.
func (s *socket) read() int {
	s.connection.SetReadLimit(s.engine.options.FrameBytes)
	_ = s.connection.SetReadDeadline(time.Now().Add(pongWait)) // a deadline on a live connection cannot fail
	s.connection.SetPongHandler(func(string) error { return s.connection.SetReadDeadline(time.Now().Add(pongWait)) })
	for {
		_, data, err := s.connection.ReadMessage()
		if err != nil {
			return closeCodeOf(err)
		}
		_ = s.connection.SetReadDeadline(time.Now().Add(pongWait))
		var frame RequestFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			refusal := Invalid(map[string]string{"frame": "is not a JSON request frame"})
			s.observe(startUnit(OutcomeRequest, "", ""), refusal)
			s.respondError("", refusal)
			continue
		}
		s.handle(&frame)
	}
}

func closeCodeOf(err error) int {
	var closeError *websocket.CloseError
	switch {
	case errors.As(err, &closeError):
		return closeError.Code
	case errors.Is(err, websocket.ErrReadLimit):
		return websocket.CloseMessageTooBig
	default:
		return websocket.CloseAbnormalClosure
	}
}

// write is the socket's only writer: the queued frames in order, then each due
// subscription's push, computed from what it delivered last.
func (s *socket) write() {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ping.C:
			if err := s.connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(s.engine.options.WriteTimeout)); err != nil {
				s.closeAfterWriteFailure(err)
				return
			}
		case <-s.outbox.wake:
			frames, due := s.outbox.take()
			for _, liveSubscription := range due {
				if push := liveSubscription.nextPush(); push != nil {
					frames = append(frames, push)
				}
			}
			if !s.writeFrames(frames) {
				return
			}
		}
	}
}

func (s *socket) writeFrames(frames [][]byte) bool {
	for _, frame := range frames {
		if s.ctx.Err() != nil {
			return false
		}
		_ = s.connection.SetWriteDeadline(time.Now().Add(s.engine.options.WriteTimeout))
		if err := s.connection.WriteMessage(websocket.TextMessage, frame); err != nil {
			s.closeAfterWriteFailure(err)
			return false
		}
	}
	return true
}

// closeAfterWriteFailure closes a socket that did not take a write within WriteTimeout 1013,
// a slow consumer; any other failure means the connection broke (1006).
func (s *socket) closeAfterWriteFailure(err error) {
	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		s.close(CloseTooSlow, "the socket did not take a write in time")
		return
	}
	s.close(websocket.CloseAbnormalClosure, "")
}

// enqueue hands frames to the writer; a queue past QueuedBytesPerSocket closes the socket
// 1013 (D10).
func (s *socket) enqueue(frames ...[]byte) {
	if !s.outbox.enqueue(frames...) {
		s.close(CloseTooSlow, "the socket does not read its responses")
	}
}

// close ends the socket once: its subscriptions leave the index, its goroutines are
// cancelled, the close frame is sent when the connection still takes one, and an
// OutcomeClose reports the code.
func (s *socket) close(code int, reason string) {
	s.closeWithin(code, reason, time.Now().Add(closeFrameTimeout))
}

// closeWithin is close with the close frame given up at deadline.
func (s *socket) closeWithin(code int, reason string, deadline time.Time) {
	s.closeOnce.Do(func() {
		s.mutex.Lock()
		s.closed = true
		subscriptions := make([]*subscription, 0, len(s.subscriptions))
		for _, liveSubscription := range s.subscriptions {
			subscriptions = append(subscriptions, liveSubscription)
		}
		clear(s.subscriptions)
		connection, failure := s.connection, s.failure
		s.mutex.Unlock()
		s.outbox.shut()
		s.cancel()
		for _, liveSubscription := range subscriptions {
			liveSubscription.end()
		}
		if connection != nil {
			s.writeClose(code, reason, deadline)
		}
		outcome := startUnit(OutcomeClose, "", "").outcome(auth.WithIdentity(s.ctx, s.lastIdentity()), failure)
		outcome.Status = code
		s.engine.observe(s.ctx, outcome)
	})
}

// writeClose sends the close frame and closes the connection; 1006 is a code no frame may
// carry (the connection broke), so then it only closes.
func (s *socket) writeClose(code int, reason string, deadline time.Time) {
	if code != websocket.CloseAbnormalClosure {
		_ = s.connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), deadline) // a peer that does not take it is gone
	}
	_ = s.connection.Close() // the close of a hijacked connection reports nothing to act on
}

// requestRevalidation asks the socket's re-validation goroutine for a fresh identity.
func (s *socket) requestRevalidation() {
	select {
	case s.revalidation <- struct{}{}:
	default:
	}
}

// revalidate re-validates the identity whenever asked, until the socket ends.
func (s *socket) revalidate() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.revalidation:
			unit := startUnit(OutcomeRevalidate, "", "")
			identity, err := s.freshIdentity(s.ctx)
			s.engine.observe(s.ctx, unit.outcome(auth.WithIdentity(s.ctx, identity), err))
		}
	}
}

// rerunIdentity is the identity a re-run acts as: the one validated within
// RevalidateInterval, else a fresh one (section 5).
func (s *socket) rerunIdentity(ctx context.Context) (*auth.Identity, error) {
	s.mutex.Lock()
	identity, identifiedAt := s.identity, s.identifiedAt
	s.mutex.Unlock()
	if time.Since(identifiedAt) < s.engine.options.RevalidateInterval {
		return identity, nil
	}
	return s.freshIdentity(ctx)
}

// freshIdentity asks the IdentitySource inside a query slot (D6). A gone identity closes the
// socket 4401 and returns errIdentityGone (D7); a changed one re-runs every subscription
// under the new rights; a failure or panic of the source keeps the last identity, and the
// next op, re-run or tick asks again. Every call is numbered when it starts: an answer older
// than the adopted one is dropped and the newer identity returned; a gone answer closes the
// socket whatever its number.
func (s *socket) freshIdentity(ctx context.Context) (*auth.Identity, error) {
	s.mutex.Lock()
	s.identityIssued++
	sequence := s.identityIssued
	s.mutex.Unlock()
	var identity *auth.Identity
	err := s.engine.inSlot(ctx, func() error {
		return recovering(func() error {
			var identifyError error
			identity, identifyError = s.engine.options.IdentitySource.Identify(ctx, s.request)
			return identifyError
		})
	})
	gone := (identity == nil && err == nil) || errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrForbidden)
	switch {
	case gone:
		s.close(CloseIdentityGone, "the identity is gone")
		return nil, errIdentityGone
	case err != nil:
		return nil, fmt.Errorf("livewire: re-validate the socket's identity: %w", err)
	}
	return s.adopt(identity, sequence), nil
}

// adopt takes the identity of Identify call sequence unless a later call's was adopted
// already, and returns the identity the socket acts as. A changed identity marks every
// subscription dirty and moves the socket's owner count; an equal one keeps the adopted
// pointer, so a pointer names one generation of the identity.
func (s *socket) adopt(identity *auth.Identity, sequence uint64) *auth.Identity {
	s.mutex.Lock()
	if sequence < s.identityAdopted {
		current := s.identity
		s.mutex.Unlock()
		return current
	}
	changed := !reflect.DeepEqual(s.identity, identity)
	if changed {
		s.identity = identity // an unchanged identity keeps its pointer: the pointer is its generation
	}
	s.identifiedAt, s.identityAdopted = time.Now(), sequence
	current := s.identity
	var subscriptions []*subscription
	if changed {
		for _, liveSubscription := range s.subscriptions {
			subscriptions = append(subscriptions, liveSubscription)
		}
	}
	s.mutex.Unlock()
	if changed {
		s.engine.migrateOwner(s, OwnerKey(identity), sequence)
	}
	for _, liveSubscription := range subscriptions {
		liveSubscription.markIdentityChanged()
	}
	return current
}

func (s *socket) lastIdentity() *auth.Identity {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.identity
}

// concerns reports whether the socket's effective or real identity has the owner key.
func (s *socket) concerns(key string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return OwnerKey(s.identity) == key || OwnerKey(s.identity.Real()) == key
}

// respond answers a request with success, its status and payload.
func (s *socket) respond(id string, status int, payload json.RawMessage) {
	s.send(ResponseFrame{ID: id, OK: true, Status: status, Payload: payload})
}

func (s *socket) respondError(id string, err error) {
	if errors.Is(err, errIdentityGone) || errors.Is(err, errSocketClosed) {
		return
	}
	status, body := ResponseFor(err)
	s.send(ResponseFrame{ID: id, Status: status, Error: &body})
}

func (s *socket) send(frame ResponseFrame) {
	encoded, err := json.Marshal(frame)
	if err != nil { // a payload that is no JSON; it never reaches the client
		status, body := ResponseFor(err)
		encoded, _ = json.Marshal(ResponseFrame{ID: frame.ID, Status: status, Error: &body}) //nolint:errchkjson // an ErrorBody always marshals
	}
	s.enqueue(encoded)
}

func (s *socket) observe(unit unitOfWork, err error) {
	s.engine.observe(s.ctx, unit.outcome(auth.WithIdentity(s.ctx, s.lastIdentity()), err))
}

// writeError answers a refused upgrade with the one error body.
func writeError(writer http.ResponseWriter, err error) {
	status, body := ResponseFor(err)
	if errors.Is(err, errUnavailable) {
		status = http.StatusServiceUnavailable
	}
	writeErrorBody(writer, status, body)
}

func writeErrorBody(writer http.ResponseWriter, status int, body ErrorBody) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body) // the client went away; nothing is left to answer
}

// writeHandshakeError answers a request that is no WebSocket handshake.
func writeHandshakeError(writer http.ResponseWriter, _ *http.Request, status int, _ error) {
	writeErrorBody(writer, status, Invalid(map[string]string{"upgrade": "is not a WebSocket handshake"}).Body())
}

// errUnavailable is a closing engine's refusal of a new socket: 503 with the internal body.
var errUnavailable = errors.New("livewire: the engine is closing")

// limitExceeded is the 429 of a per-socket or per-owner limit (D10), with the closed code
// invalid_request naming the limit.
func limitExceeded(name, message string) *Error {
	return newError(http.StatusTooManyRequests, CodeInvalidRequest, map[string]string{name: message})
}
