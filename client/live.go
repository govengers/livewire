package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/govengers/livewire"
)

// readTimeout is how long the socket may stay silent: the server pings every 30 s, so a
// socket without a frame or ping for two and a half intervals is half open and replaced.
const readTimeout = 75 * time.Second

// live is the socket side of a Client: its subscriptions and the one connection loop that
// serves them while there are any. Every field is guarded by mutex, which also serializes
// the writes on the connection.
type live struct {
	client        *Client
	mutex         sync.Mutex
	subscriptions map[*subscription]bool
	byServerID    map[string]*subscription
	pending       map[string]*subscription // subscribe request id to its subscription
	timers        map[string]*time.Timer
	abandoned     map[string]bool // subscribe requests that timed out
	connection    *websocket.Conn
	lastID        int64
	stop          context.CancelFunc // ends the running connection loop; nil while none runs
	ended         *EndedError
}

func newLive(client *Client) *live {
	return &live{
		client: client, subscriptions: map[*subscription]bool{}, byServerID: map[string]*subscription{},
		pending: map[string]*subscription{}, timers: map[string]*time.Timer{}, abandoned: map[string]bool{},
	}
}

// add registers a subscription, subscribes it on an open socket and starts the connection
// loop when none runs.
func (l *live) add(target string, parameters Parameters) (*subscription, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.ended != nil {
		return nil, l.ended
	}
	added := &subscription{target: target, parameters: maps.Clone(parameters), signal: make(chan struct{}, 1)}
	l.subscriptions[added] = true
	if l.connection != nil {
		l.subscribeLocked(added)
	}
	if l.stop == nil {
		ctx, stop := context.WithCancel(context.Background()) // the loop's lifetime: it ends with the last subscription
		l.stop = stop
		go l.run(ctx)
	}
	return added, nil
}

// next takes the subscription's next queued push.
func (l *live) next(queued *subscription) (rawPush, bool) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if len(queued.queue) == 0 {
		return rawPush{}, false
	}
	push := queued.queue[0]
	queued.queue = queued.queue[1:]
	return push, true
}

// remove ends a subscription its consumer is done with: unsubscribed on the server and, as
// the last one, the end of the connection loop.
func (l *live) remove(removed *subscription) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if !l.subscriptions[removed] {
		return
	}
	delete(l.subscriptions, removed)
	if removed.serverID != "" {
		delete(l.byServerID, removed.serverID)
		l.unsubscribeLocked(removed.serverID)
	}
	l.stopIfIdleLocked()
}

// failLocked ends a subscription with a last push of kind error.
func (l *live) failLocked(failed *subscription, err error) {
	if failed.ended {
		return
	}
	failed.ended = true
	delete(l.subscriptions, failed)
	if failed.serverID != "" {
		delete(l.byServerID, failed.serverID)
	}
	failed.enqueue(rawPush{kind: livewire.PushError, err: err})
	l.stopIfIdleLocked()
}

// stopIfIdleLocked ends the connection loop once no subscription is left; the connection
// closes under it and its state is dropped at once, so a new subscription starts afresh.
func (l *live) stopIfIdleLocked() {
	if len(l.subscriptions) > 0 || l.stop == nil {
		return
	}
	l.stop()
	l.stop = nil
	l.resetLocked()
}

// resetLocked drops the state of the current connection.
func (l *live) resetLocked() {
	l.connection = nil
	for _, timer := range l.timers {
		timer.Stop()
	}
	clear(l.timers)
	clear(l.pending)
	clear(l.abandoned)
	clear(l.byServerID)
	for current := range l.subscriptions {
		current.serverID, current.sequence = "", 0
	}
}

// end ends the live side for good (D66): every subscription with the EndedError.
func (l *live) end(code int) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.ended = &EndedError{CloseCode: code}
	for current := range l.subscriptions {
		l.failLocked(current, l.ended)
	}
	l.stopIfIdleLocked()
}

// crash ends every subscription after a panic of the connection loop, a bug, so nothing
// waits for a loop that is gone; the next subscription starts a new one.
func (l *live) crash(recovered any) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	for current := range l.subscriptions {
		l.failLocked(current, fmt.Errorf("livewire: the socket loop failed: %v", recovered))
	}
	l.stopIfIdleLocked()
}

// run connects, serves the connection until it closes and reconnects after a backoff,
// until ctx ends (the last subscription ended) or the identity is gone.
func (l *live) run(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			l.crash(recovered)
		}
	}()
	attempt := 0
	for {
		connection, status, err := l.dial(ctx)
		code := websocket.CloseAbnormalClosure
		switch {
		case ctx.Err() != nil:
			if connection != nil {
				_ = connection.Close()
			}
			return
		case status == http.StatusUnauthorized:
			l.end(http.StatusUnauthorized)
			return
		case err != nil:
			l.connectFailed(fmt.Errorf("livewire: connecting to %s: %w", l.client.options.SocketURL, err))
		default:
			attempt = 0
			code = l.serve(ctx, connection)
			if code == livewire.CloseIdentityGone {
				l.end(code)
				return
			}
		}
		if !wait(ctx, l.delay(attempt, code)) {
			return
		}
		attempt++
	}
}

// connectFailed reports a failed connect to Options.ConnectFailed, under recovery: a
// panicking callback costs nothing but its report.
func (l *live) connectFailed(err error) {
	if l.client.options.ConnectFailed == nil {
		return
	}
	defer func() { _ = recover() }() // a callback that panics loses its report, nothing else
	l.client.options.ConnectFailed(err)
}

// dial opens the socket with the API key; a refused handshake answers its status.
func (l *live) dial(ctx context.Context) (*websocket.Conn, int, error) {
	dialer := websocket.Dialer{HandshakeTimeout: l.client.options.RequestTimeout, Proxy: http.ProxyFromEnvironment}
	header := http.Header{"Authorization": {"Bearer " + l.client.options.APIKey}}
	connection, response, err := dialer.DialContext(ctx, l.client.options.SocketURL, header)
	status := 0
	if response != nil {
		status = response.StatusCode
		_ = response.Body.Close()
	}
	return connection, status, err
}

// delay is the wait before the next connect: a going-away close (a deploy) reconnects soon
// with jitter, so the clients spread out; any other doubles the wait per failed attempt.
func (l *live) delay(attempt int, code int) time.Duration {
	initial := l.client.options.ReconnectDelay
	if code == websocket.CloseGoingAway {
		return time.Duration(rand.Int64N(int64(initial))) //nolint:gosec // G404: jitter, not a secret; [0, initial) cannot overflow
	}
	exponential := l.client.options.MaximumReconnectDelay
	if attempt < 62 && initial <= exponential>>attempt {
		exponential = initial << attempt // compared before shifting: no overflow, whatever the options
	}
	return exponential/2 + time.Duration(rand.Int64N(int64(exponential/2)+1)) //nolint:gosec // G404: jitter, not a secret
}

// wait waits for delay and reports whether ctx is still running.
func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// serve subscribes every subscription on the new connection and reads frames until it
// closes, then answers its close code.
func (l *live) serve(ctx context.Context, connection *websocket.Conn) int {
	defer func() { _ = connection.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if !l.attach(ctx, connection) {
		return websocket.CloseNormalClosure
	}
	defer l.detach(connection)
	connection.SetPingHandler(func(data string) error {
		_ = connection.SetReadDeadline(time.Now().Add(readTimeout)) // a deadline on a live connection cannot fail
		err := connection.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(l.client.options.RequestTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		return err
	})
	for {
		_ = connection.SetReadDeadline(time.Now().Add(readTimeout))
		_, data, err := connection.ReadMessage()
		if err != nil {
			var closeError *websocket.CloseError
			if errors.As(err, &closeError) {
				return closeError.Code
			}
			return websocket.CloseAbnormalClosure
		}
		l.receive(data)
	}
}

// attach makes connection the current one and subscribes every subscription on it, unless
// the loop ended meanwhile.
func (l *live) attach(ctx context.Context, connection *websocket.Conn) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if ctx.Err() != nil {
		return false
	}
	l.connection = connection
	for current := range l.subscriptions {
		l.subscribeLocked(current)
	}
	return true
}

// detach drops a closed connection's state: every subscription subscribes again on the next.
func (l *live) detach(connection *websocket.Conn) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.connection == connection {
		l.resetLocked()
	}
}

// writeLocked writes a frame on the current connection; a write that fails closes it, so
// the reader ends and the loop reconnects.
func (l *live) writeLocked(frame *livewire.RequestFrame) {
	encoded, err := json.Marshal(frame)
	if err != nil || l.connection == nil {
		return // a frame of strings and a JSON body always marshals
	}
	_ = l.connection.SetWriteDeadline(time.Now().Add(l.client.options.RequestTimeout))
	if l.connection.WriteMessage(websocket.TextMessage, encoded) != nil {
		_ = l.connection.Close()
	}
}

func (l *live) nextIDLocked() string {
	l.lastID++
	return strconv.FormatInt(l.lastID, 10)
}

// subscribeLocked sends a subscribe; unanswered within RequestTimeout, the subscription ends
// and a late answer is unsubscribed.
func (l *live) subscribeLocked(subscribed *subscription) {
	id := l.nextIDLocked()
	l.pending[id] = subscribed
	timeout := l.client.options.RequestTimeout
	l.timers[id] = time.AfterFunc(timeout, func() { l.timedOut(id, timeout) })
	l.writeLocked(&livewire.RequestFrame{ID: id, Operation: livewire.OperationSubscribe, Target: subscribed.target, Parameters: subscribed.parameters})
}

func (l *live) unsubscribeLocked(serverID string) {
	l.writeLocked(&livewire.RequestFrame{ID: l.nextIDLocked(), Operation: livewire.OperationUnsubscribe, Target: serverID})
}

func (l *live) timedOut(id string, timeout time.Duration) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	waiting, ok := l.pending[id]
	if !ok {
		return
	}
	delete(l.pending, id)
	delete(l.timers, id)
	l.abandoned[id] = true
	l.failLocked(waiting, fmt.Errorf("livewire: subscribe %s got no answer within %s", waiting.target, timeout))
}

// incomingFrame is a response frame or a push frame; a push carries "sub".
type incomingFrame struct {
	ID           string            `json:"id"`
	OK           bool              `json:"ok"`
	Status       int               `json:"status"`
	Error        json.RawMessage   `json:"error"`
	Subscription string            `json:"subscription"`
	FilterID     string            `json:"filterId"`
	Push         string            `json:"sub"`
	Sequence     int64             `json:"seq"`
	Kind         livewire.PushKind `json:"kind"`
	Payload      json.RawMessage   `json:"payload"`
}

func (l *live) receive(data []byte) {
	var frame incomingFrame
	if json.Unmarshal(data, &frame) != nil {
		return // not a frame of the protocol
	}
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if frame.Push != "" {
		l.pushedLocked(&frame)
		return
	}
	l.answeredLocked(&frame)
}

// answeredLocked handles the answer of a subscribe; other answers (unsubscribe) carry
// nothing to handle.
func (l *live) answeredLocked(frame *incomingFrame) {
	answered, ok := l.pending[frame.ID]
	if !ok {
		if l.abandoned[frame.ID] && frame.OK && frame.Subscription != "" {
			l.unsubscribeLocked(frame.Subscription) // a subscribe answered after its deadline
		}
		delete(l.abandoned, frame.ID)
		return
	}
	delete(l.pending, frame.ID)
	if timer := l.timers[frame.ID]; timer != nil {
		timer.Stop()
		delete(l.timers, frame.ID)
	}
	switch {
	case !l.subscriptions[answered]:
		if frame.OK && frame.Subscription != "" {
			l.unsubscribeLocked(frame.Subscription) // its consumer left while it was on its way
		}
	case frame.OK:
		answered.serverID, answered.sequence = frame.Subscription, 0
		l.byServerID[frame.Subscription] = answered
		if inline, sent := answered.parameters[livewire.ParameterFilter]; sent && frame.FilterID != "" {
			l.client.filters.remember(frame.FilterID, inline)
		}
	default:
		refusal := errorOf(frame.Status, frame.Error)
		if inline, known := l.client.filters.inline(answered.parameters, refusal); known && !answered.retriedInline {
			answered.retriedInline, answered.parameters = true, inline // the filter was evicted (D54): register it inline, once
			l.subscribeLocked(answered)
			return
		}
		l.failLocked(answered, refusal)
	}
}

// pushedLocked applies a push in sequence; a gap (a lost push) subscribes again, which
// starts with a snapshot.
func (l *live) pushedLocked(frame *incomingFrame) {
	pushed := l.byServerID[frame.Push]
	if pushed == nil {
		return
	}
	if frame.Sequence != pushed.sequence+1 {
		delete(l.byServerID, frame.Push)
		l.unsubscribeLocked(frame.Push)
		pushed.serverID, pushed.sequence = "", 0
		l.subscribeLocked(pushed)
		return
	}
	pushed.sequence = frame.Sequence
	var err error
	switch frame.Kind {
	case livewire.PushError:
		l.failLocked(pushed, errorOf(0, frame.Payload))
	case livewire.PushListChange:
		err = l.changedLocked(pushed, frame.Payload)
	case livewire.PushSnapshot:
		pushed.list, err = newListState(frame.Payload)
		if err == nil {
			err = l.enqueueListLocked(pushed, livewire.PushSnapshot, nil)
		}
	default:
		pushed.list = nil
		pushed.enqueue(rawPush{kind: frame.Kind, data: frame.Payload})
	}
	if err != nil {
		l.unsubscribeLocked(frame.Push)
		l.failLocked(pushed, err)
	}
}

// changedLocked applies a diff to the list the subscription holds.
func (l *live) changedLocked(changed *subscription, payload json.RawMessage) error {
	if changed.list == nil {
		return errors.New("livewire: a diff arrived before the snapshot")
	}
	var change livewire.ListChange
	if err := json.Unmarshal(payload, &change); err != nil {
		return fmt.Errorf("livewire: a diff does not decode: %w", err)
	}
	if err := changed.list.apply(&change); err != nil {
		return err
	}
	return l.enqueueListLocked(changed, livewire.PushListChange, &change)
}

func (l *live) enqueueListLocked(listed *subscription, kind livewire.PushKind, change *livewire.ListChange) error {
	data, err := listed.list.payload()
	if err != nil {
		return err
	}
	listed.enqueue(rawPush{kind: kind, data: data, change: change})
	return nil
}
