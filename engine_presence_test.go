package livewire_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestPresenceRunsTheRecordReadUnderTheCaller(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(person("bob", roleReader))
	bed.world.put(gadget{ID: "own", Name: "Mine", Group: "owned-ada"})
	ada, bob := bed.dial("ada"), bed.dial("bob")
	record := map[string]string{"key": "own"}

	require.True(t, ada.do(livewire.OperationJoin, "gadgets", record, nil).OK)
	require.Equal(t, http.StatusNotFound, bob.do(livewire.OperationJoin, "gadgets", record, nil).Status, "ownership decides who may join")
	require.Equal(t, http.StatusNotFound, bob.do(livewire.OperationSubscribe, "gadgets.presence", record, nil).Status, "and who sees who is there")
	_, err := bed.engine.Read(auth.WithIdentity(t.Context(), person("bob", roleReader)), livewire.ReadRequest{Target: "gadgets", Kind: livewire.ReadPresence, Source: livewire.MapSource(record)})
	requireStatus(t, err, http.StatusNotFound)
	present, err := bed.engine.Read(auth.WithIdentity(t.Context(), person("ada", roleReader)), livewire.ReadRequest{Target: "gadgets", Kind: livewire.ReadPresence, Source: livewire.MapSource(record)})
	require.NoError(t, err)
	require.Equal(t, 1, listOf(t, present).Total)
	require.Equal(t, http.StatusNotFound, ada.do(livewire.OperationJoin, "groupGadgets", record, nil).Status, "a list has no record read")
	require.Equal(t, http.StatusNotFound, bob.do(livewire.OperationLeave, "gadgets", record, nil).Status, "a record the socket did not join")
}

func TestPresenceTenantComesFromTheDeclarationParameters(t *testing.T) {
	tenant := func(_ context.Context, parameters any) (string, error) {
		if team, ok := parameters.(*teamParameters); ok {
			return team.Team, nil
		}
		return "default", nil
	}
	bed := newTestbedWith(t, livewire.RegistryOptions{Tenant: tenant}, nil)
	bed.identities.set(person("bob", roleReader))
	watcher, joiner := bed.dial("ada"), bed.dial("bob")
	record := map[string]string{"team": "red", "key": "board"}

	subscribed(t, watcher.do(livewire.OperationSubscribe, "team.presence", record, nil))
	require.Equal(t, 0, listOf(t, watcher.push().Payload).Total)
	require.True(t, joiner.do(livewire.OperationJoin, "team", record, nil).OK)
	require.Len(t, changeOf(t, watcher.push().Payload).Upserts, 1)
	stored, err := bed.presences.List(t.Context(), "red", "team", "board")
	require.NoError(t, err)
	require.Len(t, stored, 1, "the path parameter named the tenant (D42)")
	require.True(t, joiner.do(livewire.OperationLeave, "team", record, nil).OK)
	require.Len(t, changeOf(t, watcher.push().Payload).Removes, 1)
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

func TestExpiredIdentityCacheRunsTheRecordReadFirst(t *testing.T) {
	const interval = 300 * time.Millisecond
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.RevalidateInterval = interval })
	bed.world.put(gadget{ID: "own", Name: "Mine", Group: "owned-ada"})
	socket := bed.dial("ada")
	identified := time.Now()
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets.presence", map[string]string{"key": "own"}, nil))
	socket.push()

	bed.identities.fail(errTestConnectionLost) // the tick's revalidation fails and keeps the old identity
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeRevalidate, func(outcome livewire.Outcome) bool { return outcome.Status == http.StatusInternalServerError })
	}, frameWait, time.Millisecond)
	bed.identities.fail(nil)
	bed.identities.setFor("ada", person("bob", roleReader)) // who does not own the record
	require.Eventually(t, func() bool { return time.Since(identified) > interval+10*time.Millisecond }, frameWait, time.Millisecond, "the identity cache expired")

	block := make(chan struct{})
	bed.world.block.Store(&block) // holds every record read, so a presence push without one would go out first
	require.NoError(t, bed.presences.Join(t.Context(), &livewire.Presence{Socket: "elsewhere", Display: "Eve", Target: "gadgets", Key: "own", UpdatedAt: time.Now()}))
	socket.noPush(100 * time.Millisecond)
	bed.world.block.Store(nil)
	close(block)
	first := socket.push()
	require.Equal(t, livewire.PushError, first.Kind, "the re-run fetched a new identity: the record read runs before any presence goes out")
	require.Equal(t, livewire.CodeNotFound, errorOf(t, first.Payload).Code)
}

func TestResyncRunsThePresenceRecordRead(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets.presence", map[string]string{"key": "a"}, nil))
	socket.push()
	bed.world.remove("a") // the change notification is lost
	bed.feed.drop <- errTestConnectionLost
	bed.waitConnected()
	ended := socket.push()
	require.Equal(t, livewire.PushError, ended.Kind, "a resync runs the record read")
	require.Equal(t, livewire.CodeNotFound, errorOf(t, ended.Payload).Code)
}

func TestLeaveFailuresAreReported(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	socket := bed.dial("ada")
	require.True(t, socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": "a"}, nil).OK)
	bed.presences.failLeave.Store(true)
	require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationLeave, "gadgets", map[string]string{"key": "a"}, nil).Status)
	failedLeaves := func() int {
		count := 0
		for _, outcome := range bed.outcomes.of(livewire.OutcomeRequest) {
			if outcome.Operation == livewire.OperationLeave && errors.Is(outcome.Err, errTestConnectionLost) && outcome.Status == http.StatusInternalServerError {
				count++
			}
		}
		return count
	}
	require.Equal(t, 1, failedLeaves(), "the refused leave op")
	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool { return failedLeaves() == 2 && len(bed.outcomes.of(livewire.OutcomeClose)) == 1 },
		frameWait, time.Millisecond, "the leave of a closed socket that failed is reported too")
}

func TestLeaveWithoutAJoinResolvesNothing(t *testing.T) {
	var tenantCalls atomic.Int64
	tenant := func(context.Context, any) (string, error) {
		tenantCalls.Add(1)
		return "default", nil
	}
	bed := newTestbedWith(t, livewire.RegistryOptions{Tenant: tenant}, nil)
	socket := bed.dial("ada")
	calls := tenantCalls.Load()
	require.Equal(t, http.StatusNotFound, socket.do(livewire.OperationLeave, "team", map[string]string{"team": "red", "key": "board"}, nil).Status)
	require.Equal(t, http.StatusNotFound, socket.do(livewire.OperationLeave, "team", map[string]string{"key": "board"}, nil).Status, "not even the parameters are decoded")
	require.Equal(t, calls, tenantCalls.Load(), "a leave of nothing the socket joined runs no Tenant resolver")
}

func TestWaitingJoinsHoldNoQuerySlot(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.QueryConcurrency = 2 })
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.put(gadget{ID: "b", Name: "Bolt"})
	joiner, reader := bed.dial("ada"), bed.dial("ada")
	hold := make(chan struct{})
	bed.presences.holdJoin.Store(&hold)
	held := joiner.send(livewire.OperationJoin, "gadgets", map[string]string{"key": "a"}, nil)
	<-bed.presences.joinHeld
	waiting := joiner.send(livewire.OperationJoin, "gadgets", map[string]string{"key": "b"}, nil)
	require.True(t, reader.do(livewire.OperationQuery, "gadgets", nil, nil).OK, "the waiting join holds no slot, so other sockets read on")
	close(hold)
	require.True(t, joiner.response(held).OK)
	require.True(t, joiner.response(waiting).OK)
}

func TestEqualIdentitiesKeepTheRecordReadGeneration(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets.presence", map[string]string{"key": "a"}, nil))
	socket.push()
	join := func(id string) {
		require.NoError(t, bed.presences.Join(t.Context(), &livewire.Presence{Socket: id, Display: id, Target: "gadgets", Key: "a", UpdatedAt: time.Now()}))
		require.Len(t, changeOf(t, socket.push().Payload).Upserts, 1)
	}

	require.True(t, socket.do(livewire.OperationQuery, "gadgets.distinct", map[string]string{"field": "group"}, nil).OK, "an op adopts an equal identity")
	loads := bed.world.loads.Load()
	join("first")
	require.Equal(t, loads, bed.world.loads.Load(), "an equal identity keeps its generation: no record read")

	renamed := person("ada", roleReader, roleWriter)
	renamed.Name = "Ada Lovelace" // another identity with the same rights
	bed.identities.set(renamed)
	bed.engine.RevalidateAll()
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond, "a changed identity runs the record read once")
	join("second")
	require.Equal(t, loads+1, bed.world.loads.Load(), "and is the generation from then on")
}

func TestPresenceTableStaysInTheTenantWhenThePartitionMoves(t *testing.T) {
	tenant := func(context.Context, any) (string, error) { return "acme", nil }
	bed := newTestbedWith(t, livewire.RegistryOptions{Tenant: tenant}, nil)
	red := person("ada", roleReader, roleWriter)
	red.Data = map[string]any{"group": "red"}
	bed.identities.set(red)
	socket := bed.dial("ada")
	record := map[string]string{"key": "k"}
	subscribed(t, socket.do(livewire.OperationSubscribe, "myBoard.presence", record, nil))
	socket.push()
	join := func(id string) {
		require.NoError(t, bed.presences.Join(t.Context(), &livewire.Presence{Socket: id, Owner: livewire.Owner{Tenant: "acme"}, Display: id, Target: "myBoard", Key: "k", UpdatedAt: time.Now()}))
		require.Len(t, changeOf(t, socket.push().Payload).Upserts, 1)
	}
	join("before")

	reruns := len(bed.outcomes.of(livewire.OutcomeRerun))
	blue := person("ada", roleReader, roleWriter)
	blue.Data = map[string]any{"group": "blue"}
	bed.identities.set(blue)
	bed.engine.RevalidateAll()
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeRerun)) > reruns }, frameWait, time.Millisecond)
	join("after") // the declaration's partition moved to blue; the presence table stays in the tenant
}
