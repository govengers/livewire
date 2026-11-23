package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestNewEngineValidatesOptions(t *testing.T) {
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	valid := func() *livewire.EngineOptions {
		return &livewire.EngineOptions{Registry: registry, IdentitySource: newFakeIdentities(), Transactor: &fakeTransactor{}, AllowedOrigins: []string{"https://app.example"}}
	}
	testCases := []struct {
		name   string
		adjust func(options *livewire.EngineOptions)
	}{
		{"no registry", func(options *livewire.EngineOptions) { options.Registry = nil }},
		{"no identity source", func(options *livewire.EngineOptions) { options.IdentitySource = nil }},
		{"no transactor", func(options *livewire.EngineOptions) { options.Transactor = nil }},
		{"no origins", func(options *livewire.EngineOptions) { options.AllowedOrigins = nil }},
		{"origin with a path", func(options *livewire.EngineOptions) { options.AllowedOrigins = []string{"https://app.example/x"} }},
		{"origin without scheme", func(options *livewire.EngineOptions) { options.AllowedOrigins = []string{"app.example"} }},
		{"negative limit", func(options *livewire.EngineOptions) { options.InFlightPerSocket = -1 }},
		{"negative interval", func(options *livewire.EngineOptions) { options.RevalidateInterval = -time.Second }},
		{"expiry below heartbeat", func(options *livewire.EngineOptions) { options.PresenceExpiry = time.Second }},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := valid()
			testCase.adjust(options)
			_, err := livewire.NewEngine(options)
			require.Error(t, err)
		})
	}
	_, err = livewire.NewEngine(nil)
	require.Error(t, err)

	sameHost := valid()
	sameHost.AllowedOrigins, sameHost.SameHostOnly = nil, true
	_, err = livewire.NewEngine(sameHost)
	require.NoError(t, err)

	options := valid()
	before := *options
	_, err = livewire.NewEngine(options)
	require.NoError(t, err)
	require.Equal(t, before, *options, "NewEngine never writes the caller's options")
}

func TestSubscribeSnapshotDiffReplaceUnsubscribe(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor", Group: "a"})
	socket := bed.dial("ada")

	listSubscription := subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	snapshot := socket.push()
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.Equal(t, int64(1), snapshot.Sequence)
	list := listOf(t, snapshot.Payload)
	require.Equal(t, 1, list.Total)
	require.Equal(t, "Anchor", list.Items[0].Name)
	read, err := bed.engine.Read(auth.WithIdentity(t.Context(), person("ada", roleReader)), livewire.ReadRequest{Target: "gadgets"})
	require.NoError(t, err)
	require.JSONEq(t, string(read), string(snapshot.Payload), "the snapshot is the REST body (D23)")

	objectSubscription := subscribed(t, socket.do(livewire.OperationSubscribe, "gadgetCount", nil, nil))
	first := socket.push()
	require.Equal(t, livewire.PushReplace, first.Kind)
	require.JSONEq(t, `{"count":1}`, string(first.Payload))

	bed.world.put(gadget{ID: "b", Name: "Bolt", Group: "b"})
	bed.world.change("b", "b")
	pushes := map[string]livewire.PushFrame{}
	for range 2 {
		push := socket.push()
		pushes[push.Subscription] = push
	}
	pushed := pushes[listSubscription]
	require.Equal(t, livewire.PushListChange, pushed.Kind)
	require.Equal(t, int64(2), pushed.Sequence)
	change := changeOf(t, pushed.Payload)
	require.Len(t, change.Upserts, 1)
	require.Equal(t, []string{"a", "b"}, change.Order)
	require.Equal(t, 2, *change.Total)
	replace := pushes[objectSubscription]
	require.Equal(t, livewire.PushReplace, replace.Kind)
	require.JSONEq(t, `{"count":2}`, string(replace.Payload))

	response := socket.do(livewire.OperationUnsubscribe, listSubscription, nil, nil)
	require.True(t, response.OK)
	require.False(t, socket.do(livewire.OperationUnsubscribe, listSubscription, nil, nil).OK, "a second unsubscribe names nothing")
	bed.world.remove("a")
	bed.world.change("a", "a")
	push := socket.push()
	require.Equal(t, objectSubscription, push.Subscription, "only the open subscription is pushed")
	socket.noPush(50 * time.Millisecond)
}

func TestSubscribeItemDistinctAndQuery(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor", Group: "a", Secret: "s3cret"})
	bed.world.put(gadget{ID: "b", Name: "Bolt", Group: "a"})
	socket := bed.dial("ada")

	item := socket.do(livewire.OperationQuery, "gadgets.item", map[string]string{"id": "a"}, nil)
	require.True(t, item.OK, "%+v", item.Error)
	require.JSONEq(t, `{"id":"a","name":"Anchor","group":"a"}`, string(item.Payload), "the guarded secret is stripped (D39)")

	distinct := socket.do(livewire.OperationQuery, "gadgets.distinct", map[string]string{"field": "group"}, nil)
	require.True(t, distinct.OK, "%+v", distinct.Error)
	require.JSONEq(t, `[{"value":"a","count":2}]`, string(distinct.Payload))

	missing := socket.do(livewire.OperationQuery, "gadgets.item", map[string]string{"id": "zz"}, nil)
	require.Equal(t, http.StatusNotFound, missing.Status)
	for _, target := range []string{"nothing", "createGadget", "gadgets.unknown", "gadgetCount.item"} {
		require.Equal(t, http.StatusNotFound, socket.do(livewire.OperationQuery, target, nil, nil).Status, target)
	}

	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets.item", map[string]string{"id": "b"}, nil))
	require.Equal(t, livewire.PushReplace, socket.push().Kind)
	bed.world.remove("b")
	bed.world.change("b", "a")
	ended := socket.push()
	require.Equal(t, livewire.PushError, ended.Kind, "a 4xx re-run ends the subscription")
	require.Equal(t, livewire.CodeNotFound, errorOf(t, ended.Payload).Code)
}

func TestInlineFilterIsRegisteredAndResolvedPerOwner(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(person("bob", roleReader))
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	socket := bed.dial("ada")
	filter := `{"where":{"field":"name","op":"eq","value":"Anchor"}}`

	response := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": filter}, nil)
	subscribed(t, response)
	require.NotEmpty(t, response.FilterID, "an inline filter is registered on subscribe (D13)")
	socket.push()
	again := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": filter}, nil)
	require.Equal(t, response.FilterID, again.FilterID, "the same filter dedups")
	socket.push()

	stored := socket.do(livewire.OperationQuery, "gadgets", map[string]string{"filterId": response.FilterID}, nil)
	require.True(t, stored.OK, "%+v", stored.Error)
	both := socket.do(livewire.OperationQuery, "gadgets", map[string]string{"filterId": response.FilterID, "filter": filter}, nil)
	require.Equal(t, http.StatusBadRequest, both.Status)

	other := bed.dial("bob")
	foreign := other.do(livewire.OperationQuery, "gadgets", map[string]string{"filterId": response.FilterID}, nil)
	require.Equal(t, http.StatusNotFound, foreign.Status, "a stored filter is its owner's only")
	invalid := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": `{"where":{"field":"nope","op":"eq","value":1}}`}, nil)
	require.Equal(t, http.StatusBadRequest, invalid.Status)
}

func TestTimeDependentSubscriptionRerunsOnTheSafetyTick(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.SafetyTickInterval = 10 * time.Millisecond })
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": `{"where":{"field":"createdAt","op":"gt","value":{"relative":"-7d"}}}`}, nil))
	socket.push()
	loads := bed.world.loads.Load()
	require.Eventually(t, func() bool { return bed.world.loads.Load() > loads+2 }, frameWait, 5*time.Millisecond)
}

func TestPartitionedChangesReachTheirPartitionOnly(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "groupGadgets", map[string]string{"group": "a"}, nil))
	socket.push()

	loads := bed.world.loads.Load()
	bed.world.put(gadget{ID: "b1", Name: "Bolt", Group: "b"})
	bed.world.change("b1", "b")
	socket.noPush(50 * time.Millisecond)
	require.Equal(t, loads, bed.world.loads.Load(), "a change of another partition re-runs nothing")

	bed.world.put(gadget{ID: "a1", Name: "Anchor", Group: "a"})
	bed.world.change("a1", "a")
	pushed := socket.push()
	require.Equal(t, livewire.PushListChange, pushed.Kind)
	require.Len(t, changeOf(t, pushed.Payload).Upserts, 1)
}

func TestFeedReconnectResyncsWithSnapshots(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.Equal(t, livewire.PushSnapshot, socket.push().Kind)

	bed.feed.drop <- errTestConnectionLost
	bed.waitConnected()
	resynced := socket.push()
	require.Equal(t, livewire.PushSnapshot, resynced.Kind, "every connect resyncs, even without a change")
	require.Equal(t, int64(2), resynced.Sequence)
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeFeed)) == 1 }, frameWait, time.Millisecond)
}

func TestRunRestartsAFailingFeedWithBackoff(t *testing.T) {
	feed := newFakeFeed()
	feed.failFirst.Store(3)
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.ChangeFeed = nil })
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: bed.registry(), ChangeFeed: feed, IdentitySource: bed.identities, Transactor: bed.transactor,
		SameHostOnly: true, RetryInterval: time.Millisecond,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error)
	go func() { stopped <- engine.Run(ctx) }()
	select {
	case <-feed.connected:
	case <-time.After(frameWait):
		t.Fatal("the feed never connected")
	}
	require.Equal(t, int64(4), feed.runs.Load(), "three refused connects, then one that holds")
	require.Equal(t, int64(1), feed.connects.Load())
	require.Error(t, engine.Run(ctx), "one Run at a time")
	cancel()
	require.NoError(t, <-stopped)
	require.NoError(t, engine.Close(t.Context()))
}

func TestIdentityGoneClosesTheSocket4401(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(person("bob", roleReader))
	revalidated := bed.dial("ada")
	operated := bed.dial("bob")
	owned := bed.dial("bob")
	require.True(t, revalidated.do(livewire.OperationPing, "", nil, nil).OK)

	bed.identities.revoke("ada")
	bed.engine.RevalidateOwner("test:ada")
	require.Equal(t, livewire.CloseIdentityGone, revalidated.closeCode())

	bed.identities.revoke("bob")
	operated.send(livewire.OperationQuery, "gadgets", nil, nil)
	require.Equal(t, livewire.CloseIdentityGone, operated.closeCode(), "every op re-validates (D6)")
	bed.engine.RevalidateAll()
	require.Equal(t, livewire.CloseIdentityGone, owned.closeCode())
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeRevalidate)) >= 2 }, frameWait, time.Millisecond)
}

func TestIdentitySourceFailureKeepsTheSocket(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	bed.identities.fail(errTestConnectionLost)
	response := socket.do(livewire.OperationQuery, "gadgets", nil, nil)
	require.Equal(t, http.StatusInternalServerError, response.Status)
	bed.identities.fail(nil)
	require.True(t, socket.do(livewire.OperationQuery, "gadgets", nil, nil).OK, "the socket stays open and the next op asks again")
}

func TestIdentityChangeEndsForbiddenSubscriptions(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscription := subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()

	bed.identities.set(person("ada", roleWriter))
	bed.engine.RevalidateAll()
	ended := socket.push()
	require.Equal(t, subscription, ended.Subscription)
	require.Equal(t, livewire.PushError, ended.Kind, "a changed identity re-runs under the new rights (D7)")
	require.Equal(t, livewire.CodeForbidden, errorOf(t, ended.Payload).Code)
	require.True(t, socket.do(livewire.OperationPing, "", nil, nil).OK, "the socket stays open")
}

func TestOriginCheck(t *testing.T) {
	bed := newTestbed(t, nil)
	testCases := []struct {
		name   string
		header http.Header
		status int
	}{
		{"allowed origin", http.Header{"Origin": {"https://APP.example"}}, http.StatusSwitchingProtocols},
		{"own host", http.Header{"Origin": {bed.server.URL}}, http.StatusSwitchingProtocols},
		{"foreign origin", http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden},
		{"cookie without origin", http.Header{"Cookie": {"session=1"}}, http.StatusForbidden},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			connection, response, err := bed.dialRaw("ada", testCase.header)
			require.Equal(t, testCase.status, response.StatusCode)
			require.NoError(t, response.Body.Close())
			if err == nil {
				require.NoError(t, connection.Close())
			}
		})
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, bed.server.URL, nil)
	require.NoError(t, err)
	request.Header.Set(subjectHeader, "ada")
	plain, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, plain.StatusCode, "a request that is no handshake")
	require.NoError(t, plain.Body.Close())
}

// TestHandshakeIdentity pins D97: a handshake without an identity in its context asks the
// IdentitySource; a gone identity is upgraded and closed 4401 (a refused handshake shows a
// browser only 1006 and counts against the host), a failing source is refused before it.
func TestHandshakeIdentity(t *testing.T) {
	t.Run("a gone identity is upgraded and closed 4401", func(t *testing.T) {
		bed := newTestbed(t, nil)
		connection := dialGone(t, bed)
		// a browser's client subscribes as soon as the socket opens: the close still arrives
		require.NoError(t, connection.WriteJSON(livewire.RequestFrame{ID: "1", Operation: livewire.OperationSubscribe, Target: "gadgets"}))
		requireClosedGone(t, connection)
		require.Eventually(t, func() bool {
			closes := bed.outcomes.of(livewire.OutcomeClose)
			return len(closes) == 1 && closes[0].Status == livewire.CloseIdentityGone
		}, frameWait, time.Millisecond, "the close is observed")
		require.Len(t, bed.outcomes.of(livewire.OutcomeOpen), 1, "an open per close: an Observer pairing them (the sockets gauge) stays balanced")
	})

	t.Run("the close waits for the client's close", func(t *testing.T) {
		bed := newTestbed(t, nil)
		connection := dialGone(t, bed)
		var code int
		connection.SetCloseHandler(func(received int, _ string) error { code = received; return nil }) // no echo yet
		require.NoError(t, connection.SetReadDeadline(time.Now().Add(frameWait)))
		_, _, err := connection.ReadMessage()
		require.Error(t, err)
		require.Equal(t, livewire.CloseIdentityGone, code)
		require.Never(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeClose)) > 0 }, 200*time.Millisecond, 10*time.Millisecond,
			"the server reads until the client's close: closing with unread frames resets the connection, which may lose the 4401")
		require.NoError(t, connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(frameWait)))
		require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeClose)) == 1 }, frameWait, time.Millisecond)
	})

	t.Run("a closing engine refuses it and Close waits for it", func(t *testing.T) {
		bed := newTestbed(t, nil)
		connection := dialGone(t, bed)
		connection.SetCloseHandler(func(int, string) error { return nil }) // holds the server until its read deadline
		go func() { _, _, _ = connection.ReadMessage() }()
		closeContext, cancel := context.WithTimeout(t.Context(), frameWait)
		defer cancel()
		require.NoError(t, bed.engine.Close(closeContext))
		require.Len(t, bed.outcomes.of(livewire.OutcomeClose), 1, "Close returned after the gone socket was observed")

		_, response, err := bed.dialRaw("nobody", http.Header{})
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
		require.NoError(t, response.Body.Close())
	})

	t.Run("the source names the identity without a middleware", func(t *testing.T) {
		var opened atomic.Pointer[auth.Identity]
		bed := newTestbedWith(t, livewire.RegistryOptions{StartUnit: func(ctx context.Context, unit livewire.Unit) (context.Context, func()) {
			if unit.Kind == livewire.OutcomeOpen {
				opened.Store(auth.IdentityFrom(ctx))
			}
			return ctx, func() {}
		}}, nil)
		server := httptest.NewServer(bed.engine.Handler())
		defer server.Close()
		header := http.Header{subjectHeader: {"ada"}}
		connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), header)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		defer func() { _ = connection.Close() }()
		require.NoError(t, connection.WriteJSON(livewire.RequestFrame{ID: "1", Operation: livewire.OperationPing}))
		require.NoError(t, connection.SetReadDeadline(time.Now().Add(frameWait)))
		var answer livewire.ResponseFrame
		require.NoError(t, connection.ReadJSON(&answer))
		require.True(t, answer.OK, "the socket serves ada")
		require.NotNil(t, opened.Load(), "the socket's units run on a context with its identity")
		require.Equal(t, "ada", opened.Load().Subject)
	})

	t.Run("a failing source is refused before the upgrade", func(t *testing.T) {
		bed := newTestbed(t, nil)
		bed.identities.fail(errors.New("the session store is down"))
		_, response, err := bed.dialRaw("nobody", http.Header{})
		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, response.StatusCode)
		require.NoError(t, response.Body.Close())

		bed.identities.fail(nil)
		bed.identities.panics.Store(true)
		_, response, err = bed.dialRaw("nobody", http.Header{})
		require.Error(t, err)
		require.Equal(t, http.StatusInternalServerError, response.StatusCode, "a panicking source")
		require.NoError(t, response.Body.Close())

		failures := bed.outcomes.of(livewire.OutcomeRevalidate)
		require.Len(t, failures, 2, "each failure is observed")
		require.ErrorContains(t, failures[0].Err, "the session store is down")
		require.ErrorContains(t, failures[1].Err, "the identity source panicked")
		require.Empty(t, bed.outcomes.of(livewire.OutcomeOpen))
	})

	t.Run("every gone answer closes 4401", func(t *testing.T) {
		for name, gone := range map[string]error{
			"forbidden":               auth.ErrForbidden,
			"wrapped unauthenticated": fmt.Errorf("the session ended: %w", auth.ErrUnauthenticated),
		} {
			t.Run(name, func(t *testing.T) {
				bed := newTestbed(t, nil)
				bed.identities.fail(gone)
				requireClosedGone(t, dialGone(t, bed))
			})
		}
	})

	t.Run("a foreign origin is refused before the source is asked", func(t *testing.T) {
		bed := newTestbed(t, nil)
		_, response, err := bed.dialRaw("nobody", http.Header{"Origin": {"https://evil.example"}, "Cookie": {"session=1"}})
		require.Error(t, err)
		require.Equal(t, http.StatusForbidden, response.StatusCode, "D9 before D97: a cross-site page never reaches the IdentitySource")
		require.NoError(t, response.Body.Close())
		require.Zero(t, bed.identities.calls.Load())
	})
}

// dialGone opens a socket as a subject the IdentitySource does not know, from an allowed origin.
func dialGone(t *testing.T, bed *testbed) *websocket.Conn {
	t.Helper()
	connection, response, err := bed.dialRaw("nobody", http.Header{"Origin": {"https://app.example"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
	require.NoError(t, response.Body.Close())
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// requireClosedGone reads the socket until its close, which must be 4401.
func requireClosedGone(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(frameWait)))
	_, _, err := connection.ReadMessage()
	var closeError *websocket.CloseError
	require.ErrorAs(t, err, &closeError)
	require.Equal(t, livewire.CloseIdentityGone, closeError.Code)
}

func TestSocketLimits(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.SubscriptionsPerSocket, options.InFlightPerSocket, options.FrameBytes, options.SocketsPerOwner = 2, 1, 1024, 2
	})
	socket := bed.dial("ada")
	for range 2 {
		subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	}
	refused := socket.do(livewire.OperationSubscribe, "gadgets", nil, nil)
	require.Equal(t, http.StatusTooManyRequests, refused.Status)
	require.Contains(t, refused.Error.Fields, "subscriptions")

	blocked := socket.send(livewire.OperationCall, "blockGadget", nil, nil)
	require.Eventually(t, func() bool { return len(bed.transactor.writes()) == 1 }, frameWait, time.Millisecond)
	inFlight := socket.do(livewire.OperationQuery, "gadgets", nil, nil)
	require.Equal(t, http.StatusTooManyRequests, inFlight.Status)
	require.Contains(t, inFlight.Error.Fields, "requests")
	close(bed.release)
	require.True(t, socket.response(blocked).OK)

	bed.dial("ada")
	_, response, err := bed.dialRaw("ada", http.Header{})
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode, "a third socket of the owner")
	require.NoError(t, response.Body.Close())

	require.NoError(t, socket.connection.WriteMessage(websocket.TextMessage, []byte(`{"id":"1","op":"ping","target":"`+strings.Repeat("x", 2048)+`"}`)))
	require.Equal(t, websocket.CloseMessageTooBig, socket.closeCode())
}

func TestSlowConsumerIsClosed1013(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.QueuedBytesPerSocket, options.InFlightPerSocket = 64<<10, 64
	})
	for index := range 500 {
		id := strconv.Itoa(index)
		bed.world.put(gadget{ID: id, Name: strings.Repeat("n", 200), Group: id})
	}
	connection, response, err := bed.dialRaw("ada", http.Header{})
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	defer func() { _ = connection.Close() }()
	for index := range 400 {
		if connection.WriteJSON(livewire.RequestFrame{ID: strconv.Itoa(index), Operation: livewire.OperationQuery, Target: "gadgets"}) != nil {
			break // the server closed the socket already
		}
	}
	require.Eventually(t, func() bool {
		for _, outcome := range bed.outcomes.of(livewire.OutcomeClose) {
			if outcome.Status == livewire.CloseTooSlow {
				return true
			}
		}
		return false
	}, frameWait, time.Millisecond, "a client that does not read what it asks for is a slow consumer (D10)")
}

func TestCoalescingUnderABurst(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()

	block := make(chan struct{})
	bed.world.block.Store(&block)
	loads := bed.world.loads.Load()
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.change("a", "")
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond, "the leading edge re-runs at once")
	for index := range 50 {
		id := "burst" + strconv.Itoa(index)
		bed.world.put(gadget{ID: id, Name: id})
		bed.world.change(id, "")
	}
	bed.world.block.Store(nil)
	close(block)
	pushed := socket.push()
	require.Equal(t, livewire.PushListChange, pushed.Kind)
	require.Equal(t, 51, *changeOf(t, pushed.Payload).Total, "the blocked re-run reads after the burst")
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+2 }, frameWait, time.Millisecond)
	socket.noPush(50 * time.Millisecond)
	require.Equal(t, loads+2, bed.world.loads.Load(), "fifty changes during a re-run collapse into one follow-up")
}

func TestDeliveryBatchMergesPushes(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.DeliveryBatchWait = 300 * time.Millisecond })
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()

	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.change("a", "")
	first := socket.push()
	require.Len(t, changeOf(t, first.Payload).Upserts, 1, "the first change after a quiet period leaves at once")
	for _, id := range []string{"b", "c"} {
		loads := bed.world.loads.Load()
		bed.world.put(gadget{ID: id, Name: id})
		bed.world.change(id, "")
		require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond)
	}
	merged := socket.push()
	require.Equal(t, int64(3), merged.Sequence)
	require.Len(t, changeOf(t, merged.Payload).Upserts, 2, "two re-runs within the wait leave as one push")
	socket.noPush(50 * time.Millisecond)
}

func TestCloseEndsEverySocketGoingAway(t *testing.T) {
	bed := newTestbed(t, nil)
	first, second := bed.dial("ada"), bed.dial("ada")
	subscribed(t, first.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.NoError(t, bed.engine.Close(t.Context()))
	require.Equal(t, livewire.CloseGoingAway, first.closeCode())
	require.Equal(t, livewire.CloseGoingAway, second.closeCode())
	_, response, err := bed.dialRaw("ada", http.Header{})
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestPanickingLoaderCostsOneRetry(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()

	bed.world.panics.Store(1)
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.change("a", "")
	pushed := socket.push()
	require.Equal(t, livewire.PushListChange, pushed.Kind, "the retry after the panic delivers the change")
	failed := 0
	for _, outcome := range bed.outcomes.of(livewire.OutcomeRerun) {
		if outcome.Status == http.StatusInternalServerError {
			failed++
		}
	}
	require.Equal(t, 1, failed)

	bed.world.failures.Store(2)
	bed.world.put(gadget{ID: "b", Name: "Bolt"})
	bed.world.change("b", "")
	require.Equal(t, livewire.PushListChange, socket.push().Kind, "5xx re-runs retry with backoff")

	bed.world.panics.Store(1)
	response := socket.do(livewire.OperationQuery, "gadgets", nil, nil)
	require.Equal(t, http.StatusInternalServerError, response.Status)
	require.Equal(t, livewire.CodeInternal, response.Error.Code)
}

func TestFrameErrorsAndPing(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	require.NoError(t, socket.connection.WriteMessage(websocket.TextMessage, []byte(`not json`)))
	bad := socket.response("")
	require.Equal(t, http.StatusBadRequest, bad.Status)
	require.Contains(t, bad.Error.Fields, "frame")
	unknown := socket.do("dance", "", nil, nil)
	require.Equal(t, http.StatusBadRequest, unknown.Status)
	require.Contains(t, unknown.Error.Fields, "op")
	pong := socket.do(livewire.OperationPing, "", nil, nil)
	require.True(t, pong.OK)
	require.Equal(t, http.StatusOK, pong.Status)
	notLive := socket.do(livewire.OperationSubscribe, "gadgets.distinct", map[string]string{"field": "group"}, nil)
	require.True(t, notLive.OK, "a distinct read of a live resource is subscribable (D15)")
	require.Equal(t, livewire.PushReplace, socket.push().Kind)
}

func TestPresenceJoinLeaveAndExpiry(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.PresenceHeartbeat, options.PresenceExpiry = 10*time.Millisecond, time.Minute
	})
	bed.identities.set(person("bob", roleReader))
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	watcher, joiner := bed.dial("ada"), bed.dial("bob")
	record := map[string]string{"key": "a"}
	subscribed(t, watcher.do(livewire.OperationSubscribe, "gadgets.presence", record, nil))
	require.Equal(t, 0, listOf(t, watcher.push().Payload).Total)

	joined := joiner.do(livewire.OperationJoin, "gadgets", record, map[string]string{"field": "name"})
	require.True(t, joined.OK, "%+v", joined.Error)
	change := changeOf(t, watcher.push().Payload)
	require.Len(t, change.Upserts, 1)
	var presence livewire.PresenceItem
	require.NoError(t, json.Unmarshal(change.Upserts[0], &presence))
	require.Equal(t, "Bob", presence.Display)
	require.JSONEq(t, `{"field":"name"}`, string(presence.State))
	require.Eventually(t, func() bool { return bed.presences.heartbeats.Load() > 0 }, frameWait, time.Millisecond)

	require.True(t, joiner.do(livewire.OperationLeave, "gadgets", record, nil).OK)
	require.Len(t, changeOf(t, watcher.push().Payload).Removes, 1)

	require.NoError(t, bed.presences.Join(t.Context(), &livewire.Presence{Socket: "elsewhere", Display: "Eve", Target: "gadgets", Key: "a", UpdatedAt: time.Now()}))
	require.Len(t, changeOf(t, watcher.push().Payload).Upserts, 1)
	bed.presences.age(2 * time.Minute)
	require.Len(t, changeOf(t, watcher.push().Payload).Removes, 1, "a presence without heartbeat is swept after the expiry")

	require.True(t, joiner.do(livewire.OperationJoin, "gadgets", record, nil).OK)
	require.Len(t, changeOf(t, watcher.push().Payload).Upserts, 1)
	require.NoError(t, joiner.connection.Close())
	require.Len(t, changeOf(t, watcher.push().Payload).Removes, 1, "a closed socket leaves its presences")

	missingKey := watcher.do(livewire.OperationJoin, "gadgets", nil, nil)
	require.Equal(t, http.StatusBadRequest, missingKey.Status)
	unknown := watcher.do(livewire.OperationJoin, "createGadget", record, nil)
	require.Equal(t, http.StatusNotFound, unknown.Status)
}

func TestPresenceIsOffWithoutAStore(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.PresenceStore = nil })
	socket := bed.dial("ada")
	require.Equal(t, http.StatusNotFound, socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": "a"}, nil).Status)
	require.Equal(t, http.StatusNotFound, socket.do(livewire.OperationSubscribe, "gadgets.presence", map[string]string{"key": "a"}, nil).Status)
}

func TestSocketOutcomesAndRedaction(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(&auth.Identity{Provider: "test", Subject: "root", Active: true, Roles: []string{roleReader, auth.RoleAdmin}})
	bed.world.put(gadget{ID: "a", Name: "Anchor", Secret: "s3cret"})
	user, admin := bed.dial("ada"), bed.dial("root")

	subscribed(t, user.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.NotContains(t, string(user.push().Payload), "s3cret", "a guarded field is stripped from every item (D39)")
	subscribed(t, admin.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.Contains(t, string(admin.push().Payload), "s3cret")

	calls := bed.identities.calls.Load()
	bed.engine.RevalidateOwner("test:nobody")
	require.True(t, user.do(livewire.OperationPing, "", nil, nil).OK)
	require.Equal(t, calls, bed.identities.calls.Load(), "a revalidation of another owner asks nothing; a ping neither")

	require.NoError(t, user.connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")))
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeClose)) == 1 }, frameWait, time.Millisecond)
	closed := bed.outcomes.of(livewire.OutcomeClose)[0]
	require.Equal(t, livewire.CloseNormal, closed.Status)
	require.Equal(t, "ada", closed.Identity.Subject)
	require.Len(t, bed.outcomes.of(livewire.OutcomeOpen), 2)
	subscribes := bed.outcomes.of(livewire.OutcomeSubscribe)
	require.Len(t, subscribes, 2)
	require.Equal(t, "gadgets", subscribes[0].Target)
	require.Equal(t, "1", subscribes[0].Subscription)
}
