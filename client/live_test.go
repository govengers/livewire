package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
)

// fakeSocket is one connection of the fake socket server, driven by the test.
type fakeSocket struct {
	t          *testing.T
	connection *websocket.Conn
	frames     chan livewire.RequestFrame
	mutex      sync.Mutex
}

// next is the next request frame the client sent.
func (s *fakeSocket) next() livewire.RequestFrame {
	s.t.Helper()
	select {
	case frame := <-s.frames:
		return frame
	case <-time.After(5 * time.Second):
		s.t.Fatal("the client sent no frame")
		return livewire.RequestFrame{}
	}
}

// nextOf skips frames of other operations (unsubscribes) until one of operation.
func (s *fakeSocket) nextOf(operation livewire.Operation) livewire.RequestFrame {
	s.t.Helper()
	for {
		if frame := s.next(); frame.Operation == operation {
			return frame
		}
	}
}

func (s *fakeSocket) send(frame any) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_ = s.connection.WriteJSON(frame)
}

// subscribed answers a subscribe with the subscription id.
func (s *fakeSocket) subscribed(frame livewire.RequestFrame, serverID, filterID string) {
	s.send(livewire.ResponseFrame{ID: frame.ID, OK: true, Status: http.StatusOK, Subscription: serverID, FilterID: filterID})
}

func (s *fakeSocket) push(serverID string, sequence int64, kind livewire.PushKind, payload string) {
	s.send(livewire.PushFrame{Subscription: serverID, Sequence: sequence, Kind: kind, Payload: json.RawMessage(payload)})
}

func (s *fakeSocket) close(code int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_ = s.connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
	_ = s.connection.Close()
}

// socketServer is a fake livewire socket: every connection is handed to the test.
type socketServer struct {
	url         string
	connections chan *fakeSocket
	refuse      chan int // a status to refuse the next handshake with
}

func newSocketServer(t *testing.T) *socketServer {
	t.Helper()
	server := &socketServer{connections: make(chan *fakeSocket, 8), refuse: make(chan int, 8)}
	upgrader := websocket.Upgrader{}
	var handlers sync.WaitGroup
	var mutex sync.Mutex
	var open []*websocket.Conn
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case status := <-server.refuse:
			writer.WriteHeader(status)
			return
		default:
		}
		assert.Equal(t, "Bearer "+apiKey, request.Header.Get("Authorization"))
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		mutex.Lock()
		open = append(open, connection)
		mutex.Unlock()
		socket := &fakeSocket{t: t, connection: connection, frames: make(chan livewire.RequestFrame, 64)}
		server.connections <- socket
		handlers.Go(func() {
			for {
				var frame livewire.RequestFrame
				if connection.ReadJSON(&frame) != nil {
					close(socket.frames)
					return
				}
				socket.frames <- frame
			}
		})
	}))
	t.Cleanup(func() {
		mutex.Lock()
		for _, connection := range open {
			_ = connection.Close()
		}
		mutex.Unlock()
		httpServer.Close()
		handlers.Wait()
	})
	server.url = "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/socket"
	return server
}

func (s *socketServer) accept(t *testing.T) *fakeSocket {
	t.Helper()
	select {
	case socket := <-s.connections:
		return socket
	case <-time.After(5 * time.Second):
		t.Fatal("the client did not connect")
		return nil
	}
}

func newLiveClient(t *testing.T, server *socketServer, requestTimeout time.Duration) *client.Client {
	t.Helper()
	created, err := client.New(&client.Options{
		BaseURL: "http://127.0.0.1:1", APIKey: apiKey, SocketURL: server.url,
		RequestTimeout: requestTimeout, ReconnectDelay: time.Millisecond, MaximumReconnectDelay: 5 * time.Millisecond,
	})
	require.NoError(t, err)
	return created
}

type item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func receive[Data any](t *testing.T, pushes <-chan client.Push[Data]) client.Push[Data] {
	t.Helper()
	select {
	case push, open := <-pushes:
		require.True(t, open, "the subscription ended")
		return push
	case <-time.After(5 * time.Second):
		t.Fatal("no push arrived")
		return client.Push[Data]{}
	}
}

func requireClosed[Data any](t *testing.T, pushes <-chan client.Push[Data]) {
	t.Helper()
	select {
	case _, open := <-pushes:
		require.False(t, open, "a push arrived after the last")
	case <-time.After(5 * time.Second):
		t.Fatal("the channel did not close")
	}
}

func TestAListSubscriptionAppliesItsDiffs(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	ctx, cancel := context.WithCancel(t.Context())
	pushes, err := client.Subscribe[livewire.ListResponse[item]](ctx, created, "parts", client.Parameters{"limit": "10"})
	require.NoError(t, err)
	socket := server.accept(t)
	frame := socket.next()
	require.Equal(t, livewire.RequestFrame{ID: frame.ID, Operation: livewire.OperationSubscribe, Target: "parts", Parameters: map[string]string{"limit": "10"}}, frame)
	socket.subscribed(frame, "s1", "")
	socket.push("s1", 1, livewire.PushSnapshot, `{"limit":10,"offset":0,"total":2,"items":[{"id":"a","name":"A"},{"id":"b","name":"B"}],"next":"cursor"}`)
	snapshot := receive(t, pushes)
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.Equal(t, []item{{"a", "A"}, {"b", "B"}}, snapshot.Data.Items)
	require.Equal(t, "cursor", *snapshot.Data.Next)

	socket.push("s1", 2, livewire.PushListChange, `{"upserts":[{"id":"c","name":"C"},{"id":"a","name":"A2"}],"removes":["b"],"total":2}`)
	diff := receive(t, pushes)
	require.Equal(t, livewire.PushListChange, diff.Kind)
	require.Equal(t, []item{{"a", "A2"}, {"c", "C"}}, diff.Data.Items)
	require.Equal(t, 2, diff.Data.Total)
	require.Equal(t, []string{"b"}, diff.Change.Removes)

	socket.push("s1", 3, livewire.PushListChange, `{"upserts":[],"removes":[],"order":["c","a"]}`)
	require.Equal(t, []item{{"c", "C"}, {"a", "A2"}}, receive(t, pushes).Data.Items)

	cancel()
	requireClosed(t, pushes)
	require.Equal(t, "s1", socket.nextOf(livewire.OperationUnsubscribe).Target)
}

func TestAnObjectSubscriptionEndsWithItsErrorPush(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	pushes, err := client.Subscribe[item](t.Context(), created, "dashboard", nil)
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s1", "")
	socket.push("s1", 1, livewire.PushReplace, `{"id":"d","name":"D"}`)
	require.Equal(t, client.Push[item]{Kind: livewire.PushReplace, Data: item{"d", "D"}}, receive(t, pushes))
	socket.push("s1", 2, livewire.PushError, `{"code":"forbidden","message":"forbidden"}`)
	last := receive(t, pushes)
	var refusal *client.Error
	require.ErrorAs(t, last.Err, &refusal)
	require.Equal(t, client.Error{Status: http.StatusForbidden, Code: livewire.CodeForbidden, Message: "forbidden"}, *refusal)
	requireClosed(t, pushes)
}

func TestAReconnectSubscribesAgain(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	pushes, err := client.Subscribe[livewire.ListResponse[item]](t.Context(), created, "parts", nil)
	require.NoError(t, err)
	first := server.accept(t)
	first.subscribed(first.next(), "s1", "")
	first.push("s1", 1, livewire.PushSnapshot, `{"limit":10,"offset":0,"total":0,"items":[]}`)
	receive(t, pushes)
	first.close(websocket.CloseGoingAway)

	second := server.accept(t)
	second.subscribed(second.next(), "s7", "")
	second.push("s7", 1, livewire.PushSnapshot, `{"limit":10,"offset":0,"total":1,"items":[{"id":"a","name":"A"}]}`)
	again := receive(t, pushes)
	require.Equal(t, livewire.PushSnapshot, again.Kind)
	require.Len(t, again.Data.Items, 1)

	second.push("s7", 3, livewire.PushListChange, `{"upserts":[],"removes":["a"]}`) // seq 2 was lost
	resubscribe := second.nextOf(livewire.OperationSubscribe)
	second.subscribed(resubscribe, "s8", "")
	second.push("s8", 1, livewire.PushSnapshot, `{"limit":10,"offset":0,"total":0,"items":[]}`)
	require.Empty(t, receive(t, pushes).Data.Items)

	second.close(websocket.CloseInternalServerErr) // a failure: the client backs off and connects again
	third := server.accept(t)
	third.subscribed(third.next(), "s9", "")
}

func TestIdentityGoneEndsTheLiveSide(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	pushes, err := client.Subscribe[item](t.Context(), created, "dashboard", nil)
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s1", "")
	socket.close(livewire.CloseIdentityGone)
	last := receive(t, pushes)
	var ended *client.EndedError
	require.ErrorAs(t, last.Err, &ended)
	require.Equal(t, livewire.CloseIdentityGone, ended.CloseCode)
	requireClosed(t, pushes)
	_, err = client.Subscribe[item](t.Context(), created, "dashboard", nil)
	require.ErrorAs(t, err, &ended)
}

func TestARefusedHandshakeEndsTheLiveSide(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	server.refuse <- http.StatusServiceUnavailable // a failure: retried
	server.refuse <- http.StatusUnauthorized       // the key is gone: ended
	pushes, err := client.Subscribe[item](t.Context(), created, "dashboard", nil)
	require.NoError(t, err)
	var ended *client.EndedError
	require.ErrorAs(t, receive(t, pushes).Err, &ended)
	require.Equal(t, http.StatusUnauthorized, ended.CloseCode)
}

func TestAnEvictedFilterIsRegisteredInlineOnce(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	document := `{"where":{"field":"name","op":"eq","value":"a"}}`
	pushes, err := client.Subscribe[livewire.ListResponse[item]](t.Context(), created, "parts", client.Parameters{livewire.ParameterFilter: document})
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s1", "filter-1") // the inline filter is registered as filter-1
	socket.push("s1", 1, livewire.PushSnapshot, `{"limit":10,"offset":0,"total":0,"items":[]}`)
	receive(t, pushes)

	byID, err := client.Subscribe[livewire.ListResponse[item]](t.Context(), created, "parts", client.Parameters{livewire.ParameterFilterID: "filter-1"})
	require.NoError(t, err)
	refused := socket.next()
	require.Equal(t, map[string]string{livewire.ParameterFilterID: "filter-1"}, refused.Parameters)
	notFound := livewire.ErrorBody{Code: livewire.CodeNotFound, Message: "not found"}
	socket.send(livewire.ResponseFrame{ID: refused.ID, Status: http.StatusNotFound, Error: &notFound})
	inline := socket.next()
	require.Equal(t, map[string]string{livewire.ParameterFilter: document}, inline.Parameters)
	socket.send(livewire.ResponseFrame{ID: inline.ID, Status: http.StatusNotFound, Error: &notFound})
	var refusal *client.Error
	require.ErrorAs(t, receive(t, byID).Err, &refusal, "the recovery runs once")
	require.Equal(t, livewire.CodeNotFound, refusal.Code)
}

func TestAnUnansweredSubscribeEnds(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 300*time.Millisecond)
	kept, err := client.Subscribe[item](t.Context(), created, "kept", nil) // keeps the socket open
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s-kept", "")
	pushes, err := client.Subscribe[item](t.Context(), created, "dashboard", nil)
	require.NoError(t, err)
	late := socket.next()
	require.ErrorContains(t, receive(t, pushes).Err, "no answer within")
	requireClosed(t, pushes)
	socket.subscribed(late, "s-late", "")
	require.Equal(t, livewire.RequestFrame{ID: "3", Operation: livewire.OperationUnsubscribe, Target: "s-late"}, socket.nextOf(livewire.OperationUnsubscribe))
	require.NotNil(t, kept)
}

func TestAConsumerThatFallsBehindGetsTheCurrentState(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	pushes, err := client.Subscribe[livewire.ListResponse[item]](t.Context(), created, "parts", nil)
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s1", "")
	socket.push("s1", 1, livewire.PushSnapshot, `{"limit":500,"offset":0,"total":0,"items":[]}`)
	const changes = 200
	for sequence := 2; sequence < changes+2; sequence++ {
		socket.push("s1", int64(sequence), livewire.PushListChange, `{"upserts":[{"id":"`+strconv.Itoa(sequence)+`","name":"x"}],"removes":[]}`)
	}
	var last client.Push[livewire.ListResponse[item]]
	received := 0
	for len(last.Data.Items) < changes {
		last = receive(t, pushes)
		received++
	}
	require.Less(t, received, changes, "the queue collapsed")
}

func TestBrokenPushesEndTheSubscription(t *testing.T) {
	testCases := []struct {
		name    string
		kind    livewire.PushKind
		payload string
		message string
	}{
		{"a diff before a snapshot", livewire.PushListChange, `{"upserts":[],"removes":[]}`, "before the snapshot"},
		{"a snapshot without ids", livewire.PushSnapshot, `{"limit":1,"offset":0,"total":1,"items":[{"name":"x"}]}`, "has no id"},
		{"a snapshot with a repeated id", livewire.PushSnapshot, `{"limit":1,"offset":0,"total":2,"items":[{"id":1},{"id":"1"}]}`, "repeats the id"},
		{"a snapshot that is no list", livewire.PushSnapshot, `[]`, "no list response"},
		{"an id of another kind", livewire.PushSnapshot, `{"limit":1,"offset":0,"total":1,"items":[{"id":true}]}`, "no string or number"},
		{"an item that does not decode", livewire.PushSnapshot, `{"limit":1,"offset":0,"total":1,"items":[{"id":1}]}`, "does not decode"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newSocketServer(t)
			created := newLiveClient(t, server, 0)
			pushes, err := client.Subscribe[livewire.ListResponse[item]](t.Context(), created, "parts", nil)
			require.NoError(t, err)
			socket := server.accept(t)
			socket.subscribed(socket.next(), "s1", "")
			socket.push("s1", 1, testCase.kind, testCase.payload)
			require.ErrorContains(t, receive(t, pushes).Err, testCase.message)
		})
	}
}
