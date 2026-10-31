package livewire_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

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
