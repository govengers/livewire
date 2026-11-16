package livewire_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestADetailFieldTravelsWithTheItemOnly(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor", Group: "a", Secret: "s"})
	reader := auth.WithIdentity(t.Context(), person("ada", roleReader))

	list, err := bed.engine.Read(reader, livewire.ReadRequest{Target: "gadgetCards"})
	require.NoError(t, err)
	require.JSONEq(t, `{"limit":100,"offset":0,"total":1,"items":[{"id":"a","group":"a"}]}`, string(list), "a list read leaves the Detail field out (D93), and the guarded one")
	item, err := bed.engine.Read(reader, livewire.ReadRequest{Target: "gadgetCards", Kind: livewire.ReadItem, Source: livewire.MapSource{"id": "a"}})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"a","name":"Anchor","group":"a"}`, string(item), "the item read carries it")
	filtered := livewire.MapSource{"filter": `{"where":{"field":"name","op":"eq","value":"Anchor"},"search":"anc"}`, "sort": "-name"}
	_, err = bed.engine.Read(reader, livewire.ReadRequest{Target: "gadgetCards", Source: filtered})
	require.NoError(t, err, "a Detail field filters, searches and sorts like any field")

	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgetCards", nil, nil))
	snapshot := socket.push()
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.JSONEq(t, string(list), string(snapshot.Payload), "the snapshot is the list read")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgetCards.item", map[string]string{"id": "a"}, nil))
	socket.push()

	reruns := len(bed.outcomes.of(livewire.OutcomeRerun))
	bed.world.put(gadget{ID: "a", Name: "Anchor II", Group: "a", Secret: "s"})
	bed.world.change("a", "a")
	renamed := socket.push()
	require.Equal(t, livewire.PushReplace, renamed.Kind, "the rename's first push is the item's: the list has none")
	require.Contains(t, string(renamed.Payload), "Anchor II", "the item subscription follows the Detail field")
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeRerun)) == reruns+2 }, frameWait, time.Millisecond)
	bed.world.put(gadget{ID: "a", Name: "Anchor II", Group: "b", Secret: "s"})
	bed.world.change("a", "b")
	for range 2 { // the list's diff and the item's push, in either order
		pushed := socket.push()
		if pushed.Kind != livewire.PushListChange {
			continue
		}
		var change livewire.ListChange
		require.NoError(t, json.Unmarshal(pushed.Payload, &change))
		require.Len(t, change.Upserts, 1, "a rename changed no list row, so no diff carried it: this is the group's")
		require.JSONEq(t, `{"id":"a","group":"b"}`, string(change.Upserts[0]), "a diff leaves the Detail field out")
		return
	}
	t.Fatal("the group change pushed no diff")
}
