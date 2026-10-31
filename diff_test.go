package livewire_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestNewListStateRefusesUnusableIDs(t *testing.T) {
	testCases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "not an array", raw: `{"id":"a"}`, want: "not a JSON array"},
		{name: "item without id", raw: `[{"name":"bolt"}]`, want: "has no id"},
		{name: "null id", raw: `[{"id":null}]`, want: "has no id"},
		{name: "object id", raw: `[{"id":{}}]`, want: "neither a string nor a number"},
		{name: "array id", raw: `[{"id":[1]}]`, want: "neither a string nor a number"},
		{name: "boolean id", raw: `[{"id":true}]`, want: "neither a string nor a number"},
		{name: "item not an object", raw: `["a"]`, want: "has no id"},
		{name: "duplicate string ids", raw: `[{"id":"a"},{"id":"a","v":2}]`, want: `two list items carry the id "a"`},
		{name: "number and string collide", raw: `[{"id":1},{"id":"1"}]`, want: `two list items carry the id "1"`},
		{name: "number spellings collide", raw: `[{"id":1},{"id":1.0}]`, want: `two list items carry the id "1"`},
		{name: "exponent spelling collides", raw: `[{"id":1},{"id":1e0}]`, want: `two list items carry the id "1"`},
		{name: "uppercase alias is no id", raw: `[{"ID":"b"}]`, want: "has no id"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := livewire.NewListState(json.RawMessage(testCase.raw), 0)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestListStateKeysNumbersLikeJavaScript(t *testing.T) {
	state, err := livewire.NewListState(json.RawMessage(`[{"id":1234567},{"id":1000000},{"id":1e21},{"id":1e-7},{"id":-0},{"id":"a","ID":"b"}]`), 6)
	require.NoError(t, err)
	previous, err := livewire.NewListState(json.RawMessage(`[]`), 0)
	require.NoError(t, err)
	require.Equal(t, []string{"1234567", "1000000", "1e+21", "1e-7", "0", "a"}, state.ChangesSince(previous).Order, "competing spellings key by the exact id member")
}

func TestListStateSnapshotKeepsOrderAndNumericIDs(t *testing.T) {
	state, err := livewire.NewListState(json.RawMessage(`[{"id":2,"name":"nut"},{"id":"a","name":"bolt"},{"id":1.5}]`), 3)
	require.NoError(t, err)
	require.JSONEq(t, `[{"id":2,"name":"nut"},{"id":"a","name":"bolt"},{"id":1.5}]`, string(state.Snapshot()))

	empty, err := livewire.NewListState(json.RawMessage(`[]`), 0)
	require.NoError(t, err)
	require.Equal(t, `[]`, string(empty.Snapshot()))
}

func TestListStateChangesSince(t *testing.T) {
	testCases := []struct {
		name          string
		previous      string
		previousTotal int
		next          string
		nextTotal     int
		want          string
	}{
		{
			name:     "nothing changed",
			previous: `[{"id":"a","v":1},{"id":"b","v":1}]`, previousTotal: 2,
			next: `[{"id":"a","v":1},{"id":"b","v":1}]`, nextTotal: 2,
			want: "",
		},
		{
			name:     "changed item",
			previous: `[{"id":"a","v":1},{"id":"b","v":1}]`, previousTotal: 2,
			next: `[{"id":"a","v":2},{"id":"b","v":1}]`, nextTotal: 2,
			want: `{"upserts":[{"id":"a","v":2}],"removes":[]}`,
		},
		{
			name:     "new item at the end",
			previous: `[{"id":"a","v":1}]`, previousTotal: 1,
			next: `[{"id":"a","v":1},{"id":"b","v":1}]`, nextTotal: 2,
			want: `{"upserts":[{"id":"b","v":1}],"removes":[],"order":["a","b"],"total":2}`,
		},
		{
			name:     "removed item",
			previous: `[{"id":"a","v":1},{"id":"b","v":1}]`, previousTotal: 2,
			next: `[{"id":"b","v":1}]`, nextTotal: 1,
			want: `{"upserts":[],"removes":["a"],"order":["b"],"total":1}`,
		},
		{
			name:     "reordered",
			previous: `[{"id":"a","v":1},{"id":"b","v":1}]`, previousTotal: 2,
			next: `[{"id":"b","v":1},{"id":"a","v":1}]`, nextTotal: 2,
			want: `{"upserts":[],"removes":[],"order":["b","a"]}`,
		},
		{
			name:     "total changed beyond the page",
			previous: `[{"id":"a","v":1}]`, previousTotal: 10,
			next: `[{"id":"a","v":1}]`, nextTotal: 11,
			want: `{"upserts":[],"removes":[],"total":11}`,
		},
		{
			name:     "from empty",
			previous: `[]`, previousTotal: 0,
			next: `[{"id":1}]`, nextTotal: 1,
			want: `{"upserts":[{"id":1}],"removes":[],"order":["1"],"total":1}`,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			previous, err := livewire.NewListState(json.RawMessage(testCase.previous), testCase.previousTotal)
			require.NoError(t, err)
			next, err := livewire.NewListState(json.RawMessage(testCase.next), testCase.nextTotal)
			require.NoError(t, err)

			changes := next.ChangesSince(previous)
			if testCase.want == "" {
				require.Nil(t, changes)
				return
			}
			raw, err := json.Marshal(changes)
			require.NoError(t, err)
			require.JSONEq(t, testCase.want, string(raw))
		})
	}
}
