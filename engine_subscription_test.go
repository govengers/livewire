package livewire_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

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
	bed.world.stubborn.Store(true) // the first load finishes despite the cancellation: only the publish fence holds
	loads := bed.world.loads.Load()
	id := socket.send(livewire.OperationSubscribe, "gadgets", nil, nil)
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond)
	require.True(t, socket.do(livewire.OperationUnsubscribe, "1", nil, nil).OK)
	bed.world.block.Store(nil)
	close(block)
	require.Equal(t, http.StatusNotFound, socket.response(id).Status, "the subscription ended before it started")
	socket.noPush(50 * time.Millisecond)
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

func TestFilterOfASubscriptionEndedDuringItsFirstLoadIsReleased(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.FiltersPerSocket = 1 })
	socket := bed.dial("ada")
	block := make(chan struct{})
	bed.world.block.Store(&block)
	bed.world.stubborn.Store(true) // the first load ends after the unsubscribe, and the filter registers then
	loads := bed.world.loads.Load()
	id := socket.send(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": `{"where":{"field":"name","op":"eq","value":"a"}}`}, nil)
	require.Eventually(t, func() bool { return bed.world.loads.Load() == loads+1 }, frameWait, time.Millisecond)
	require.True(t, socket.do(livewire.OperationUnsubscribe, "1", nil, nil).OK)
	bed.world.block.Store(nil)
	close(block)
	require.Equal(t, http.StatusNotFound, socket.response(id).Status)
	second := socket.do(livewire.OperationSubscribe, "gadgets", map[string]string{"filter": `{"where":{"field":"name","op":"eq","value":"b"}}`}, nil)
	require.True(t, second.OK, "the ended subscription's filter was released: %+v", second.Error)
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

func TestPartitionFollowingTheIdentityMovesTheSubscription(t *testing.T) {
	bed := newTestbed(t, nil)
	red := person("ada", roleReader, roleWriter)
	red.Data = map[string]any{"group": "red"}
	bed.identities.set(red)
	bed.world.put(gadget{ID: "b1", Name: "Blue one", Group: "blue"})
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "myGadgets", nil, nil))
	require.Equal(t, 0, listOf(t, socket.push().Payload).Total)

	blue := person("ada", roleReader, roleWriter)
	blue.Data = map[string]any{"group": "blue"}
	bed.identities.set(blue)
	bed.engine.RevalidateAll()
	require.Len(t, changeOf(t, socket.push().Payload).Upserts, 1, "the re-run reads blue")

	bed.world.put(gadget{ID: "b2", Name: "Blue two", Group: "blue"})
	bed.world.change("b2", "blue")
	require.Len(t, changeOf(t, socket.push().Payload).Upserts, 1, "a blue change reaches the subscription now")
	loads := bed.world.loads.Load()
	bed.world.put(gadget{ID: "r1", Name: "Red one", Group: "red"})
	bed.world.change("r1", "red")
	socket.noPush(50 * time.Millisecond)
	require.Equal(t, loads, bed.world.loads.Load(), "a red change no longer does")
}
