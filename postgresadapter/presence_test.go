package postgresadapter_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
)

var _ livewire.PresenceStore = (*postgresadapter.PresenceStore)(nil)

func TestPresenceNotifiesJoinsChangesAndLeavesButNotHeartbeats(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store, err := postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	listening := notifications(t, database)
	presence := &livewire.Presence{Socket: "socket-1", Owner: alice, Display: "Alice", Target: "parts", Key: "7", State: json.RawMessage(`{"field":"name"}`)}
	notified := `{"table":"livewire_presence","op":"%s","id":"","partition":"north"}`

	require.NoError(t, store.Join(t.Context(), presence))
	require.JSONEq(t, fmt.Sprintf(notified, "insert"), nextPayload(t, listening, time.Second))
	require.NoError(t, store.Join(t.Context(), presence))
	require.Empty(t, nextPayload(t, listening, quiet), "joining again with the same state changes nothing")
	require.NoError(t, store.Heartbeat(t.Context(), "socket-1"))
	require.Empty(t, nextPayload(t, listening, quiet), "a heartbeat reaches no subscription")
	presence.State = json.RawMessage(`{"field":"status"}`)
	require.NoError(t, store.Join(t.Context(), presence))
	require.JSONEq(t, fmt.Sprintf(notified, "update"), nextPayload(t, listening, time.Second))

	presences, err := store.List(t.Context(), "north", "parts", "7")
	require.NoError(t, err)
	require.Len(t, presences, 1)
	require.WithinDuration(t, time.Now(), presences[0].UpdatedAt, time.Minute)
	presences[0].UpdatedAt = time.Time{}
	presence.UpdatedAt = time.Time{}
	require.JSONEq(t, `{"field":"status"}`, string(presences[0].State))
	presences[0].State = presence.State
	require.Equal(t, *presence, presences[0])
	other, err := store.List(t.Context(), "south", "parts", "7")
	require.NoError(t, err)
	require.Empty(t, other, "presences are per tenant")

	require.NoError(t, store.Leave(t.Context(), "north", "socket-1", "parts", "7"))
	require.JSONEq(t, fmt.Sprintf(notified, "delete"), nextPayload(t, listening, time.Second))
	require.NoError(t, store.Leave(t.Context(), "north", "socket-1", "parts", "7"), "an absent presence is no error")
}

func TestPresenceSweepRemovesStalePresences(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store, err := postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	for _, socket := range []string{"stale", "fresh"} {
		require.NoError(t, store.Join(t.Context(), &livewire.Presence{Socket: socket, Owner: alice, Target: "parts", Key: "7"}))
	}
	_, err = database.pool.Exec(t.Context(), "UPDATE livewire_presence SET updated_at = now() - interval '2 minutes'")
	require.NoError(t, err)
	require.NoError(t, store.Heartbeat(t.Context(), "fresh"))
	swept, err := store.Sweep(t.Context(), 90*time.Second)
	require.NoError(t, err)
	require.Equal(t, 1, swept)
	presences, err := store.List(t.Context(), "north", "parts", "7")
	require.NoError(t, err)
	require.Len(t, presences, 1)
	require.Equal(t, "fresh", presences[0].Socket)
	require.JSONEq(t, "null", string(presences[0].State), "no state is null")

	_, err = postgresadapter.NewPresenceStore(nil)
	require.Error(t, err)
}
