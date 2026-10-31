package livewire_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

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

func TestCloseCancelsRunsStoreCalls(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.PresenceHeartbeat = 5 * time.Millisecond })
	bed.presences.blockSweep.Store(true)
	<-bed.presences.sweepBlocked // a tick waits in a store call that only its context ends
	closing, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, bed.engine.Close(closing), "Close cancels Run's work though Run's own parent context lives on")
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

func TestEngineReadRecoversAPanickingFilterStore(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	stored, err := bed.filters.Save(ctx, &livewire.StoredFilter{Owner: livewire.Owner{Key: "test:ada"}, Resource: "gadgets", Document: []byte(`{}`), Hash: "h"})
	require.NoError(t, err)
	bed.filters.panicGet.Store(true)
	_, err = bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets", Source: livewire.MapSource{"filterId": stored.ID}})
	requireStatus(t, err, http.StatusInternalServerError)
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
