package livewire_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestPanickingObserverCostsItsOutcomeOnly(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.SocketsPerOwner, options.PresenceHeartbeat, options.RevalidateInterval = 1, 5*time.Millisecond, 5*time.Millisecond
	})
	bed.outcomes.panics.Store(true)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()
	require.True(t, socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, "").OK)
	require.Equal(t, livewire.PushListChange, socket.push().Kind)
	sweeps := bed.presences.sweeps.Load()
	require.Eventually(t, func() bool { return bed.presences.sweeps.Load() > sweeps+2 }, frameWait, time.Millisecond, "the ticks go on")
	bed.feed.drop <- errTestConnectionLost
	bed.waitConnected()

	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool {
		connection, response, err := bed.dialRaw("ada", http.Header{})
		if response != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			return false
		}
		_ = connection.Close()
		return true
	}, frameWait, 5*time.Millisecond, "the closed socket gave its slot back")
	require.NotEmpty(t, bed.outcomes.of(livewire.OutcomeClose), "every outcome reached the Observer before it panicked")
}

func TestCloseWaitsForSocketsAndRun(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationCall, Target: "blockGadget", Parameters: map[string]string{"wait": "stubborn"}})
	require.Eventually(t, func() bool { return len(bed.transactor.writes()) == 1 }, frameWait, time.Millisecond)
	cleanup := make(chan struct{})
	bed.feed.cleanup.Store(&cleanup)

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, bed.engine.Close(short), context.DeadlineExceeded, "a handler that ignores its context holds Close")
	close(bed.release)
	short, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, bed.engine.Close(short), context.DeadlineExceeded, "the feed's cleanup holds Close")
	close(cleanup)
	require.NoError(t, bed.engine.Close(t.Context()))
}

func TestObserverStatusesAndCloseCodes(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.FrameBytes, options.InFlightPerSocket = 1024, 1 })
	socket := bed.dial("ada")
	require.NoError(t, socket.connection.WriteMessage(websocket.TextMessage, []byte(`not json`)))
	socket.response("")
	socket.do("dance", "nothing", nil, nil)
	blocked := socket.send(livewire.OperationCall, "blockGadget", nil, nil)
	require.Eventually(t, func() bool { return len(bed.transactor.writes()) == 1 }, frameWait, time.Millisecond)
	socket.do(livewire.OperationQuery, "gadgets", nil, nil)
	close(bed.release)
	socket.response(blocked)
	for _, expected := range []struct {
		operation livewire.Operation
		status    int
	}{{"", http.StatusBadRequest}, {"other", http.StatusBadRequest}, {livewire.OperationQuery, http.StatusTooManyRequests}} {
		require.True(t, outcomeWith(bed, livewire.OutcomeRequest, func(outcome livewire.Outcome) bool {
			return outcome.Operation == expected.operation && outcome.Status == expected.status && outcome.Err != nil
		}), "one outcome per refused frame (D30): %+v", expected)
	}

	require.NoError(t, socket.connection.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 2048))))
	require.Equal(t, websocket.CloseMessageTooBig, socket.closeCode())
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeClose, func(outcome livewire.Outcome) bool { return outcome.Status == websocket.CloseMessageTooBig })
	}, frameWait, time.Millisecond, "the close outcome carries the real code")

	gone := bed.dial("ada")
	bed.identities.revoke("ada")
	gone.send(livewire.OperationQuery, "gadgets", nil, nil)
	require.Equal(t, livewire.CloseIdentityGone, gone.closeCode())
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeRequest, func(outcome livewire.Outcome) bool {
			return outcome.Operation == livewire.OperationQuery && outcome.Status == http.StatusUnauthorized
		})
	}, frameWait, time.Millisecond, "a gone identity is observed as 401")
}

func TestAllowedOriginsDropDefaultPorts(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.AllowedOrigins = []string{"https://app.example:443", "http://local.example:80"}
	})
	for _, origin := range []string{"https://app.example", "http://local.example"} {
		connection, response, err := bed.dialRaw("ada", http.Header{"Origin": {origin}})
		require.NoError(t, err, origin)
		require.NoError(t, response.Body.Close())
		require.NoError(t, connection.Close())
	}
}

func TestSlowWriteClosesTheSocket1013(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.WriteTimeout, options.QueuedBytesPerSocket = 20*time.Millisecond, 1<<30
	})
	for index := range 4000 {
		id := strconv.Itoa(index)
		bed.world.put(gadget{ID: id, Name: strings.Repeat("n", 250), Group: id})
	}
	connection, response, err := bed.dialRaw("ada", http.Header{})
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	defer func() { _ = connection.Close() }()
	for index := range 16 {
		if connection.WriteJSON(livewire.RequestFrame{ID: strconv.Itoa(index), Operation: livewire.OperationQuery, Target: "gadgets"}) != nil {
			break
		}
	}
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeClose, func(outcome livewire.Outcome) bool { return outcome.Status == livewire.CloseTooSlow })
	}, frameWait, time.Millisecond, "a write the client does not take within WriteTimeout: %+v", bed.outcomes.of(livewire.OutcomeClose))
}

func TestCloseGivesUpCloseFramesAtItsDeadline(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.QueuedBytesPerSocket = 1 << 30 })
	for index := range 4000 {
		id := strconv.Itoa(index)
		bed.world.put(gadget{ID: id, Name: strings.Repeat("n", 250), Group: id})
	}
	connection, response, err := bed.dialRaw("ada", http.Header{})
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	defer func() { _ = connection.Close() }()
	for index := range 16 { // openGadgets guards no field: its answers are large but cheap, no redaction under -race
		require.NoError(t, connection.WriteJSON(livewire.RequestFrame{ID: strconv.Itoa(index), Operation: livewire.OperationQuery, Target: "openGadgets"}))
	}
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeRequest)) >= 16 }, frameWait, time.Millisecond)

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = bed.engine.Close(short) // the client takes neither the answers nor the close frame
	require.Less(t, time.Since(started), 500*time.Millisecond, "Close keeps its deadline, not the one-second close-frame wait")
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeClose)) == 1 }, 400*time.Millisecond, time.Millisecond,
		"the close frame was given up at Close's deadline, not after the one-second wait")
}

func TestCloseCancelsRunsStoreCalls(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.PresenceHeartbeat = 5 * time.Millisecond })
	bed.presences.blockSweep.Store(true)
	<-bed.presences.sweepBlocked // a tick waits in a store call that only its context ends
	closing, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, bed.engine.Close(closing), "Close cancels Run's work though Run's own parent context lives on")
}

func TestEndedSubscriptionsRunNothingForPendingMarks(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.SubscriptionsPerSocket = 400 })
	socket := bed.dial("ada")
	for range 400 {
		subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
		socket.push()
	}
	block := make(chan struct{})
	bed.world.block.Store(&block)
	defer close(block)
	loads := bed.world.loads.Load()
	bed.world.change("a", "")
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+32 }, frameWait, time.Millisecond, "every query slot holds a blocked re-run")
	bed.world.change("a", "") // every subscription now has a pending mark
	require.NoError(t, bed.engine.Close(t.Context()))
	require.Equal(t, loads+32, bed.world.loads.Load(), "no ended subscription loads for a pending mark or a slot freed by the close")
	require.False(t, outcomeWith(bed, livewire.OutcomeRerun, func(outcome livewire.Outcome) bool {
		return outcome.Status == http.StatusInternalServerError
	}), "a closing socket's re-runs report no failures")
}
