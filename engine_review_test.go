package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func admin(subject string) *auth.Identity {
	return person(subject, roleReader, roleWriter, auth.RoleAdmin)
}

func outcomeWith(bed *testbed, kind livewire.OutcomeKind, match func(livewire.Outcome) bool) bool {
	for _, outcome := range bed.outcomes.of(kind) {
		if match(outcome) {
			return true
		}
	}
	return false
}

func TestMutationResponsesPassTheResourceGuards(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(admin("root"))
	user, root := bed.dial("ada"), bed.dial("root")

	require.NotContains(t, string(user.call("revealGadget", nil, nil, "").Payload), "s3cret", "a guarded field leaves no mutation response (D39)")
	require.Contains(t, string(root.call("revealGadget", nil, nil, firstKey).Payload), "s3cret")
	batch := []livewire.BatchCall{{Target: "revealGadget"}}
	refusedBatch := user.response(user.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(batch)}))
	require.True(t, refusedBatch.OK)
	require.NotContains(t, string(refusedBatch.Payload), "s3cret", "every call of a batch passes its guards")
	rootBatch := root.response(root.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(batch), IdempotencyKey: secondKey}))
	require.Contains(t, string(rootBatch.Payload), "s3cret")

	bed.identities.set(person("root", roleReader, roleWriter))
	replayed := root.call("revealGadget", nil, nil, firstKey)
	require.True(t, replayed.OK)
	require.NotContains(t, string(replayed.Payload), "s3cret", "a replay passes the guards of the identity that asks now")
	replayedBatch := root.response(root.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(batch), IdempotencyKey: secondKey}))
	require.True(t, replayedBatch.OK)
	require.NotContains(t, string(replayedBatch.Payload), "s3cret")
}

func TestConcurrentIdentityRefreshesKeepTheNewest(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(admin("ada"))
	bed.world.put(gadget{ID: "a", Name: "Anchor", Secret: "s3cret"})
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.Contains(t, string(socket.push().Payload), "s3cret")

	hold := make(chan struct{})
	bed.identities.hold.Store(&hold)
	bed.engine.RevalidateAll()
	<-bed.identities.held // an Identify read the admin identity and is held
	bed.identities.set(person("ada", roleReader, roleWriter))
	require.True(t, socket.do(livewire.OperationPing, "", nil, nil).OK)
	require.True(t, socket.do(livewire.OperationQuery, "gadgets", nil, nil).OK, "a later Identify adopts the reader")
	require.NotContains(t, string(socket.push().Payload), "s3cret", "the subscription re-ran under the reader")

	close(hold)
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeRevalidate)) == 1 }, frameWait, time.Millisecond)
	socket.noPush(50 * time.Millisecond)
	bed.world.put(gadget{ID: "a", Name: "Anchor 2", Secret: "s3cret"})
	bed.world.change("a", "")
	require.NotContains(t, string(socket.push().Payload), "s3cret", "the older admin answer was dropped")
}

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

func TestPresencesPerSocketBoundsJoins(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.PresencesPerSocket = 2 })
	for _, id := range []string{"a", "b", "c"} {
		bed.world.put(gadget{ID: id, Name: id})
	}
	socket := bed.dial("ada")
	join := func(key string) livewire.ResponseFrame {
		return socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": key}, map[string]string{"at": key})
	}
	require.True(t, join("a").OK)
	require.True(t, join("b").OK)
	refused := join("c")
	require.Equal(t, http.StatusTooManyRequests, refused.Status)
	require.Contains(t, refused.Error.Fields, "presences")
	require.True(t, join("a").OK, "joining a record again updates it")
	require.True(t, socket.do(livewire.OperationLeave, "gadgets", map[string]string{"key": "a"}, nil).OK)
	require.True(t, join("c").OK, "a leave releases its place")

	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool {
		left := true
		for _, key := range []string{"a", "b", "c"} {
			presences, err := bed.presences.List(t.Context(), "", "gadgets", key)
			left = left && err == nil && len(presences) == 0
		}
		return left
	}, frameWait, time.Millisecond, "a closed socket leaves every record")
}

func TestPanicsReleaseTheirQuerySlot(t *testing.T) {
	tenant := func(_ context.Context, parameters any) (string, error) {
		if group, ok := parameters.(*groupParameters); ok && group.Group == "panic" {
			panic("the tenant resolver panicked")
		}
		return "default", nil
	}
	bed := newTestbedWith(t, livewire.RegistryOptions{Tenant: tenant}, func(options *livewire.EngineOptions) { options.QueryConcurrency = 1 })
	socket := bed.dial("ada")
	for range 3 {
		bed.identities.panics.Store(true)
		require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationQuery, "gadgets", nil, nil).Status, "a panicking IdentitySource")
		bed.identities.panics.Store(false)
		require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationQuery, "groupGadgets", map[string]string{"group": "panic"}, nil).Status, "a panicking Tenant")
	}
	require.True(t, socket.do(livewire.OperationQuery, "gadgets", nil, nil).OK, "the one slot is free again")
}

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

func TestRerunsUseTheCachedIdentity(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()
	calls := bed.identities.calls.Load()
	for _, id := range []string{"a", "b"} {
		bed.world.put(gadget{ID: id, Name: id})
		bed.world.change(id, "")
		require.Equal(t, livewire.PushListChange, socket.push().Kind)
	}
	require.Equal(t, calls, bed.identities.calls.Load(), "a re-run within RevalidateInterval asks the source nothing (section 5)")
}

func TestAnonymousCallsSkipIdempotency(t *testing.T) {
	bed := newTestbed(t, nil)
	for want := 1; want <= 2; want++ {
		_, payload, err := bed.engine.Call(t.Context(), livewire.CallRequest{Target: "anonymousGadget", IdempotencyKey: firstKey})
		require.NoError(t, err)
		require.JSONEq(t, `{"count":`+strconv.Itoa(want)+`}`, string(payload), "an anonymous caller has no owner to replay for (D16)")
	}
}

func TestSubscriptionIsIndexedBeforeItsFirstLoad(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	block := make(chan struct{})
	bed.world.block.Store(&block)
	loads := bed.world.loads.Load()
	id := socket.send(livewire.OperationSubscribe, "gadgets", nil, nil)
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond)
	bed.world.change("a", "")
	bed.world.block.Store(nil)
	close(block)
	subscribed(t, socket.response(id))
	socket.push()
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+2 }, frameWait, time.Millisecond,
		"a change during the first load re-runs the subscription")
}

func TestUnsubscribeBeforeTheFirstPushIsFenced(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	block := make(chan struct{})
	bed.world.block.Store(&block)
	loads := bed.world.loads.Load()
	id := socket.send(livewire.OperationSubscribe, "gadgets", nil, nil)
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond)
	require.True(t, socket.do(livewire.OperationUnsubscribe, "1", nil, nil).OK)
	bed.world.block.Store(nil)
	close(block)
	require.Equal(t, http.StatusNotFound, socket.response(id).Status, "the subscription ended before it started")
	socket.noPush(50 * time.Millisecond)
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

func TestReplayRollsBackItsTransaction(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	require.True(t, socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, firstKey).OK)
	rollbacks := bed.transactor.rollbacks.Load()
	require.True(t, socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, firstKey).OK)
	require.Equal(t, rollbacks+1, bed.transactor.rollbacks.Load(), "a replay writes nothing and rolls back")
}

func TestRevalidationTickAndRealOwner(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.RevalidateInterval = 20 * time.Millisecond })
	ticked := bed.dial("ada")
	bed.identities.revoke("ada")
	require.Equal(t, livewire.CloseIdentityGone, ticked.closeCode(), "the tick re-validates without an op")

	other := newTestbed(t, nil)
	impersonated := person("ada", roleReader)
	impersonated.Impersonator = person("root")
	other.identities.set(impersonated)
	socket := other.dial("ada")
	require.True(t, socket.do(livewire.OperationPing, "", nil, nil).OK)
	other.identities.revoke("ada")
	other.engine.RevalidateOwner(livewire.OwnerKey(person("root")))
	require.Equal(t, livewire.CloseIdentityGone, socket.closeCode(), "RevalidateOwner matches the real identity")
	require.Equal(t, "test:root", livewire.OwnerKey(person("root")))
	require.Empty(t, livewire.OwnerKey(nil))
}

func TestFailedRerunsBackOffAndCoverLaterChanges(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.RetryInterval = time.Second })
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()

	bed.world.failures.Store(1)
	loads := bed.world.loads.Load()
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.change("a", "")
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeRerun, func(outcome livewire.Outcome) bool { return outcome.Status == http.StatusInternalServerError })
	}, frameWait, time.Millisecond)
	bed.world.put(gadget{ID: "b", Name: "Bolt"})
	bed.world.change("b", "")
	bed.world.change("b", "")
	socket.noPush(100 * time.Millisecond)
	require.Equal(t, loads+1, bed.world.loads.Load(), "changes during the backoff wait for the retry")
	recovered := socket.push()
	require.Len(t, changeOf(t, recovered.Payload).Upserts, 2, "the retry reads every change since")
	require.Equal(t, loads+2, bed.world.loads.Load())
}

func TestStoredAndInlineFiltersResolveOnceAtSubscribe(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.FiltersPerSocket = 1 })
	socket := bed.dial("ada")
	filter := `{"where":{"field":"name","op":"eq","value":"Anchor"}}`
	inline := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": filter}, nil)
	subscribed(t, inline)
	socket.push()
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filterId": inline.FilterID}, nil))
	socket.push()

	gets := bed.filters.gets.Load()
	require.NoError(t, bed.filters.Delete(t.Context(), livewire.Owner{Key: "test:ada"}, inline.FilterID))
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	bed.world.change("a", "")
	for range 2 {
		require.Equal(t, livewire.PushListChange, socket.push().Kind, "a deleted filter does not end a live subscription")
	}
	require.Equal(t, gets, bed.filters.gets.Load(), "re-runs never ask the FilterStore")

	again := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": filter}, nil)
	require.True(t, again.OK, "a filter the socket registered before is free")
	socket.push()
	other := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": `{"where":{"field":"name","op":"eq","value":"Bolt"}}`}, nil)
	require.Equal(t, http.StatusTooManyRequests, other.Status, "FiltersPerSocket bounds inline registrations")
	require.Contains(t, other.Error.Fields, "filters")
}

func TestTenantChangeEndsTheSubscription(t *testing.T) {
	tenant := func(ctx context.Context, _ any) (string, error) {
		if named, ok := auth.IdentityFrom(ctx).Data["tenant"].(string); ok {
			return named, nil
		}
		return "first", nil
	}
	bed := newTestbedWith(t, livewire.RegistryOptions{Tenant: tenant}, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()
	moved := person("ada", roleReader, roleWriter)
	moved.Data = map[string]any{"tenant": "second"}
	bed.identities.set(moved)
	bed.engine.RevalidateAll()
	ended := socket.push()
	require.Equal(t, livewire.PushError, ended.Kind)
	require.Equal(t, livewire.CodeForbidden, errorOf(t, ended.Payload).Code, "a re-run under another tenant ends")
}

func TestBatchNameIsReservedAndKeysDoNotCross(t *testing.T) {
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.ErrorContains(t, registry.Add(livewire.NewMutation("batch", http.MethodPost, "/batch", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})), "reserved")

	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	batch := mustJSON([]livewire.BatchCall{{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})}})
	require.True(t, socket.response(socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: batch, IdempotencyKey: firstKey})).OK)
	require.Equal(t, http.StatusConflict, socket.call("createGadget", nil, map[string]string{"name": "Bolt"}, firstKey).Status, "a batch's key is no call's")
	require.True(t, socket.call("createGadget", nil, map[string]string{"name": "Bolt"}, secondKey).OK)
	crossed := socket.response(socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: batch, IdempotencyKey: secondKey}))
	require.Equal(t, http.StatusConflict, crossed.Status, "a call's key is no batch's")
}

func TestOwnerCountMovesWithTheIdentity(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.SocketsPerOwner = 1 })
	bed.identities.set(person("bob", roleReader))
	socket := bed.dial("ada")
	bed.identities.setFor("ada", person("bob", roleReader))
	require.True(t, socket.do(livewire.OperationQuery, "gadgets", nil, nil).OK)
	_, response, err := bed.dialRaw("bob", http.Header{})
	require.Error(t, err)
	require.Equal(t, http.StatusTooManyRequests, response.StatusCode, "the socket counts for its new owner")
	require.NoError(t, response.Body.Close())
	bed.identities.setFor("ada", person("ada", roleReader, roleWriter))
	bed.dial("ada")
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
	}{{"", http.StatusBadRequest}, {"dance", http.StatusBadRequest}, {livewire.OperationQuery, http.StatusTooManyRequests}} {
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

func TestLeaveFailuresAreReported(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor"})
	socket := bed.dial("ada")
	require.True(t, socket.do(livewire.OperationJoin, "gadgets", map[string]string{"key": "a"}, nil).OK)
	bed.presences.failLeave.Store(true)
	require.Equal(t, http.StatusInternalServerError, socket.do(livewire.OperationLeave, "gadgets", map[string]string{"key": "a"}, nil).Status)
	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeRequest, func(outcome livewire.Outcome) bool {
			return outcome.Operation == livewire.OperationLeave && errors.Is(outcome.Err, errTestConnectionLost) && outcome.Status == http.StatusInternalServerError
		}) && len(bed.outcomes.of(livewire.OutcomeClose)) == 1
	}, frameWait, time.Millisecond, "the leave of a closed socket that failed is reported")
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

func TestBatchBodyMustEndAfterTheArray(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter))
	for _, body := range []string{`[{"target":"failGadget"}]]`, `[{"target":"failGadget"}] {}`, `[{"target":"failGadget"}] []`} {
		_, err := bed.engine.Batch(ctx, livewire.BatchRequest{Body: json.RawMessage(body)})
		requireStatus(t, err, http.StatusBadRequest) // the handler never runs: it would answer 409
	}
}
