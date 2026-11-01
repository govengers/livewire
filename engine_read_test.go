package livewire_test

import (
	"encoding/json"
	"testing"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestCursorModeAnswersTheNextCursor(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	read := func(target string, parameters livewire.MapSource) livewire.ListResponse[pagedGadget] {
		t.Helper()
		payload, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: target, Source: parameters})
		require.NoError(t, err)
		return listOfPaged(t, payload)
	}

	first := read("pagedGadgets", livewire.MapSource{"limit": "1"})
	require.Equal(t, []pagedGadget{{ID: firstKey}}, first.Items)
	require.NotNil(t, first.Next, "a full page in cursor mode answers the cursor after its last row")
	second := read("pagedGadgets", livewire.MapSource{"limit": "1", "after": *first.Next})
	require.Equal(t, []pagedGadget{{ID: secondKey}}, second.Items)
	require.Nil(t, second.Next, "the last page has none")
	require.Equal(t, 2, second.Total)

	bed.world.put(gadget{ID: "a"})
	payload, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: "openGadgets"})
	require.NoError(t, err)
	require.NotContains(t, string(payload), `"next"`, "an offset resource ignores the loader's cursor")

	socket := bed.dial("ada")
	query := socket.do(livewire.OperationQuery, "pagedGadgets", map[string]string{"limit": "1"}, nil)
	require.True(t, query.OK)
	require.Equal(t, mustJSON(first), mustJSON(listOfPaged(t, query.Payload)), "the socket answers the same cursor")
}

func listOfPaged(t *testing.T, payload json.RawMessage) livewire.ListResponse[pagedGadget] {
	t.Helper()
	var list livewire.ListResponse[pagedGadget]
	require.NoError(t, json.Unmarshal(payload, &list))
	return list
}
