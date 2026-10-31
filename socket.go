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
	writeTimeout      = 10 * time.Second
	closeFrameTimeout = time.Second
	// leaveTimeout bounds the presence leaves of a closed socket; the sweep removes what
	// they miss.
	leaveTimeout = 5 * time.Second
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
	ownerKey := ownerKeyOf(identity)
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
	if e.perOwner[clientSocket.ownerKey]--; e.perOwner[clientSocket.ownerKey] <= 0 {
		delete(e.perOwner, clientSocket.ownerKey)
	}
	e.lifetimes.Done()
}

func ownerKeyOf(identity *auth.Identity) string {
	return identity.Provider + ":" + identity.Subject
}

// socket is one WebSocket connection. Its reader runs on the handler's goroutine, its writer,
// its re-validation, every request and every subscription on goroutines of its own that end
// with the socket. One writer writes every frame, so the order frames are queued in is the
// order on the wire.
type socket struct {
	engine     *Engine
	id         string
	request    *http.Request // the upgrade request the IdentitySource re-validates
	ownerKey   string
	connection *websocket.Conn
	//nolint:containedctx // the socket's lifetime: cancelled by close, the parent of every unit of work on the socket
	ctx        context.Context
	cancel     context.CancelFunc
	outbox     *outbox
	goroutines sync.WaitGroup
	closeOnce  sync.Once
	// revalidation has one slot: a tick or Revalidate call asked for a fresh identity.
	revalidation chan struct{}

	mutex            sync.Mutex
	closed           bool
	identity         *auth.Identity
	identifiedAt     time.Time
	subscriptions    map[string]*subscription
	lastSubscription int
	inFlight         int
	presences        map[presenceRecord]struct{}
	failure          error // the panic that closed the socket 1011, for its OutcomeClose
}

func newSocket(engine *Engine, request *http.Request, identity *auth.Identity, ownerKey string) *socket {
	ctx, cancel := context.WithCancel(context.WithoutCancel(request.Context()))
	return &socket{
		engine: engine, id: uuid.NewString(), request: request, ownerKey: ownerKey,
		ctx: ctx, cancel: cancel,
		outbox:       newOutbox(engine.options.QueuedBytesPerSocket, engine.options.DeliveryBatchWait),
		revalidation: make(chan struct{}, 1),
		identity:     identity, identifiedAt: time.Now(),
		subscriptions: map[string]*subscription{}, presences: map[presenceRecord]struct{}{},
	}
}

// serve runs the socket until it ends, then waits for its goroutines, leaves its presences
// and gives its slot back.
func (s *socket) serve(connection *websocket.Conn) {
	s.mutex.Lock()
	s.connection = connection
	closed := s.closed
	s.mutex.Unlock()
	if closed { // Close ran between the admission and here and found no connection to end
		s.writeClose(CloseGoingAway, "the server is shutting down")
	} else {
		s.observe(startUnit(OutcomeOpen, "", ""), nil)
		s.spawn(s.write)
		s.spawn(s.revalidate)
		s.guarded(s.read)
		s.close(CloseNormal, "")
	}
	s.goroutines.Wait()
	s.leavePresences()
	s.engine.release(s)
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
	if err != nil {
		s.mutex.Lock()
		s.failure = err
		s.mutex.Unlock()
		s.close(websocket.CloseInternalServerErr, "internal error")
	}
}

func (s *socket) read() {
	s.connection.SetReadLimit(s.engine.options.FrameBytes)
	_ = s.connection.SetReadDeadline(time.Now().Add(pongWait)) // a deadline on a live connection cannot fail
	s.connection.SetPongHandler(func(string) error { return s.connection.SetReadDeadline(time.Now().Add(pongWait)) })
	for {
		_, data, err := s.connection.ReadMessage()
		if err != nil {
			return // the peer closed, the frame was too large (gorilla answered 1009) or the socket was closed
		}
		_ = s.connection.SetReadDeadline(time.Now().Add(pongWait))
		var frame RequestFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			s.respondError("", Invalid(map[string]string{"frame": "is not a JSON request frame"}))
			continue
		}
		s.handle(&frame)
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
			if err := s.connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
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
		_ = s.connection.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := s.connection.WriteMessage(websocket.TextMessage, frame); err != nil {
			s.closeAfterWriteFailure(err)
			return false
		}
	}
	return true
}

// closeAfterWriteFailure closes a socket that did not take a write in time 1013, a slow
// consumer; any other failure means the peer is gone.
func (s *socket) closeAfterWriteFailure(err error) {
	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		s.close(CloseTooSlow, "the socket did not take a write in time")
		return
	}
	s.close(CloseGoingAway, "")
}

// enqueue hands frames to the writer; a queue past QueuedBytesPerSocket closes the socket
// 1013 (D10).
func (s *socket) enqueue(frames ...[]byte) {
	if !s.outbox.enqueue(frames...) {
		s.close(CloseTooSlow, "the socket does not read its responses")
	}
}

// close ends the socket once: its subscriptions leave the index, its goroutines are
// cancelled, the close frame is sent when the connection still takes one.
func (s *socket) close(code int, reason string) {
	s.closeOnce.Do(func() {
		s.mutex.Lock()
		s.closed = true
		subscriptions := make([]*subscription, 0, len(s.subscriptions))
		for _, liveSubscription := range s.subscriptions {
			subscriptions = append(subscriptions, liveSubscription)
		}
		clear(s.subscriptions)
		connection := s.connection
		s.mutex.Unlock()
		s.outbox.shut()
		s.cancel()
		for _, liveSubscription := range subscriptions {
			liveSubscription.end()
		}
		if connection != nil {
			s.writeClose(code, reason)
		}
		s.mutex.Lock()
		failure := s.failure
		s.mutex.Unlock()
		closed := startUnit(OutcomeClose, "", "")
		outcome := closed.outcome(auth.WithIdentity(s.ctx, s.lastIdentity()), failure)
		outcome.Status = code
		s.engine.registry.Observe(s.ctx, outcome)
	})
}

func (s *socket) writeClose(code int, reason string) {
	_ = s.connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(closeFrameTimeout)) // a peer that does not take it is gone
	_ = s.connection.Close()                                                                                                             // the close of a hijacked connection reports nothing to act on
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
			started := time.Now()
			identity, err := s.freshIdentity(s.ctx)
			status, _ := ResponseFor(err)
			switch {
			case err == nil:
				status = http.StatusOK
			case errors.Is(err, errIdentityGone):
				status = http.StatusUnauthorized
			}
			s.engine.registry.Observe(s.ctx, Outcome{Kind: OutcomeRevalidate, Identity: identity, Status: status, Err: err, Duration: time.Since(started)})
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
// under the new rights; a failure of the source keeps the last identity, and the next op,
// re-run or tick asks again.
func (s *socket) freshIdentity(ctx context.Context) (*auth.Identity, error) {
	release, err := s.engine.acquire(ctx)
	if err != nil {
		return nil, err
	}
	identity, err := s.engine.options.IdentitySource.Identify(ctx, s.request)
	release()
	gone := (identity == nil && err == nil) || errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrForbidden)
	switch {
	case gone:
		s.close(CloseIdentityGone, "the identity is gone")
		return nil, errIdentityGone
	case err != nil:
		return nil, fmt.Errorf("livewire: re-validate the socket's identity: %w", err)
	}
	s.adopt(identity)
	return identity, nil
}

// adopt takes a validated identity; a changed one marks every subscription dirty.
func (s *socket) adopt(identity *auth.Identity) {
	s.mutex.Lock()
	changed := !reflect.DeepEqual(s.identity, identity)
	s.identity, s.identifiedAt = identity, time.Now()
	var subscriptions []*subscription
	if changed {
		for _, liveSubscription := range s.subscriptions {
			subscriptions = append(subscriptions, liveSubscription)
		}
	}
	s.mutex.Unlock()
	for _, liveSubscription := range subscriptions {
		liveSubscription.markDirty()
	}
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
	return ownerKeyOf(s.identity) == key || ownerKeyOf(s.identity.Real()) == key
}

// respond answers a request; err nil is a success with status and payload.
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
