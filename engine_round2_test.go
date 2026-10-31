package livewire_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func (c *client) batch(calls []livewire.BatchCall, key string) livewire.ResponseFrame {
	c.t.Helper()
	return c.response(c.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(calls), IdempotencyKey: key}))
}

func TestBatchReplayIsBoundToItsCalls(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(admin("root"))
	root := bed.dial("root")
	recorded := root.batch([]livewire.BatchCall{{Target: "revealGadget"}}, firstKey)
	require.Contains(t, string(recorded.Payload), "s3cret")

	bed.identities.set(person("root", roleReader, roleWriter))
	substituted := root.batch([]livewire.BatchCall{{Target: "anonymousGadget"}}, firstKey)
	require.Equal(t, http.StatusConflict, substituted.Status, "a key sent again with other targets is no replay")
	require.NotContains(t, string(substituted.Payload), "s3cret")
	counted := root.batch([]livewire.BatchCall{{Target: "revealGadget"}, {Target: "revealGadget"}}, firstKey)
	require.Equal(t, http.StatusConflict, counted.Status, "another number of calls is a conflict, not a server failure")
	replayed := root.batch([]livewire.BatchCall{{Target: "revealGadget"}}, firstKey)
	require.True(t, replayed.OK)
	require.NotContains(t, string(replayed.Payload), "s3cret", "the recorded calls' guards under the identity of the replay")
}

func TestJoinAndLeaveOfARecordAreSerialized(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) {
		options.PresencesPerSocket, options.PresenceHeartbeat = 1, 5*time.Millisecond
	})
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.put(gadget{ID: "b", Name: "Bolt"})
	socket := bed.dial("ada")
	record := map[string]string{"key": "a"}

	hold := make(chan struct{})
	bed.presences.holdJoin.Store(&hold)
	joining := socket.send(livewire.OperationJoin, "gadgets", record, nil)
	<-bed.presences.joinHeld
	leaving := socket.send(livewire.OperationLeave, "gadgets", record, nil)
	socket.noPush(50 * time.Millisecond) // the leave waits for the join's store call
	close(hold)
	require.True(t, socket.response(joining).OK)
	require.True(t, socket.response(leaving).OK)

	heartbeats := bed.presences.heartbeats.Load()
	sweeps := bed.presences.sweeps.Load()
	require.Eventually(t, func() bool { return bed.presences.sweeps.Load() > sweeps+2 }, frameWait, time.Millisecond)
	presences, err := bed.presences.List(t.Context(), "", "gadgets", "a")
	require.NoError(t, err)
	require.Empty(t, presences, "no row outlives the leave that came after its join")
	require.Equal(t, heartbeats, bed.presences.heartbeats.Load(), "a socket without presences sends no heartbeat")
	require.True(t, socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": "b"}, nil).OK, "the accounting matches the store")

	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool {
		remaining, err := bed.presences.List(t.Context(), "", "gadgets", "b")
		return err == nil && len(remaining) == 0
	}, frameWait, time.Millisecond, "close removes the socket's presences")
}

func TestInlineFiltersCountLiveSubscriptionsOnly(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.SubscriptionsPerSocket = 100 })
	socket := bed.dial("ada")
	filterOf := func(index int) map[string]string {
		return map[string]string{"filter": `{"where":{"field":"name","op":"eq","value":"name ` + strconv.Itoa(index) + `"}}`}
	}
	for index := range 65 {
		response := socket.do(livewire.OperationSubscribe, "gadgets", filterOf(index), nil)
		require.True(t, response.OK, "edit %d: %+v", index, response.Error)
		socket.push()
		require.True(t, socket.do(livewire.OperationUnsubscribe, response.Subscription, nil, nil).OK)
	}
	for index := range 64 {
		subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", filterOf(100+index), nil))
		socket.push()
	}
	burst := socket.do(livewire.OperationSubscribe, "gadgets", filterOf(200), nil)
	require.Equal(t, http.StatusTooManyRequests, burst.Status, "the 65th live inline filter")
	require.Contains(t, burst.Error.Fields, "filters")
}

func TestPresenceSubscriptionsRunTheRecordReadOnRecordChanges(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets.presence", map[string]string{"key": "a"}, nil))
	socket.push()

	loads := bed.world.loads.Load()
	require.NoError(t, bed.presences.Join(t.Context(), &livewire.Presence{Socket: "elsewhere", Display: "Eve", Target: "gadgets", Key: "a", UpdatedAt: time.Now()}))
	require.Len(t, changeOf(t, socket.push().Payload).Upserts, 1)
	require.Equal(t, loads, bed.world.loads.Load(), "a presence change lists the presences without the record read")

	bed.world.change("a", "")
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond, "a change of the record's table runs it")
	bed.identities.set(person("ada", roleReader))
	bed.engine.RevalidateAll()
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+2 }, frameWait, time.Millisecond, "an identity change runs it")

	bed.world.remove("a")
	bed.world.change("a", "")
	ended := socket.push()
	require.Equal(t, livewire.PushError, ended.Kind, "a lost record ends the presence subscription")
	require.Equal(t, livewire.CodeNotFound, errorOf(t, ended.Payload).Code)
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
	for index := range 16 {
		require.NoError(t, connection.WriteJSON(livewire.RequestFrame{ID: strconv.Itoa(index), Operation: livewire.OperationQuery, Target: "gadgets"}))
	}
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeRequest)) >= 16 }, frameWait, time.Millisecond)

	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = bed.engine.Close(short) // the client takes neither the answers nor the close frame
	require.Less(t, time.Since(started), 500*time.Millisecond, "Close keeps its deadline, not the one-second close-frame wait")
}

func TestFailedJoinFreesItsPlace(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.PresencesPerSocket = 1 })
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.put(gadget{ID: "b", Name: "Bolt"})
	socket := bed.dial("ada")
	bed.presences.failJoin.Store(true)
	require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": "a"}, nil).Status)
	bed.presences.failJoin.Store(false)
	require.True(t, socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": "b"}, nil).OK, "the failed join gave its place back")
}

func TestEffectTargetGuardsApplyOnTheirOwn(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	revealed := socket.call("revealGadget", nil, nil, "")
	require.True(t, revealed.OK)
	require.NotContains(t, string(revealed.Payload), "s3cret", "an Effect target declaring the field unguarded cancels nothing")
	grouped := socket.call("groupReveal", nil, nil, "")
	require.True(t, grouped.OK)
	require.NotContains(t, string(grouped.Payload), "s3cret", "an Effect-only mutation passes its list's guards")
	_, payload, err := bed.engine.Call(auth.WithIdentity(t.Context(), admin("root")), livewire.CallRequest{Target: "groupReveal"})
	require.NoError(t, err)
	require.Contains(t, string(payload), "s3cret")
}

func TestLeaveMatchesTheTenantOfTheRecord(t *testing.T) {
	tenant := func(_ context.Context, parameters any) (string, error) {
		if team, ok := parameters.(*teamParameters); ok {
			return team.Team, nil
		}
		return "default", nil
	}
	bed := newTestbedWith(t, livewire.RegistryOptions{Tenant: tenant}, nil)
	socket := bed.dial("ada")
	red, blue := map[string]string{"team": "red", "key": "board"}, map[string]string{"team": "blue", "key": "board"}
	require.True(t, socket.do(livewire.OperationJoin, "team", red, nil).OK)
	require.True(t, socket.do(livewire.OperationJoin, "team", blue, nil).OK)
	require.True(t, socket.do(livewire.OperationLeave, "team", red, nil).OK)
	for team, want := range map[string]int{"red": 0, "blue": 1} {
		presences, err := bed.presences.List(t.Context(), team, "team", "board")
		require.NoError(t, err)
		require.Len(t, presences, want, "team %s", team)
	}
	require.Equal(t, http.StatusNotFound, socket.do(livewire.OperationLeave, "team", red, nil).Status)
}

func TestPanicsDuringInstallFreeTheSubscription(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.SubscriptionsPerSocket = 1 })
	socket := bed.dial("ada")
	bed.transactor.panicBegin.Store(true)
	require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil).Status)
	stored := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": `{"where":{"field":"name","op":"eq","value":"x"}}`}, nil)
	subscribed(t, stored)
	socket.push()
	require.True(t, socket.do(livewire.OperationUnsubscribe, stored.Subscription, nil, nil).OK)

	bed.filters.panicGet.Store(true)
	require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filterId": stored.FilterID}, nil).Status, "a panicking FilterStore.Get")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
}
