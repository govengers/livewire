package listkey_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/internal/listkey"
)

func TestItemIDKeysAsJavaScriptDoes(t *testing.T) {
	for item, expected := range map[string]string{
		`{"id":"a"}`: "a", `{"id":1}`: "1", `{"id":1.0}`: "1", `{"id":1e0}`: "1", `{"id":-0}`: "0",
		`{"id":1234567}`: "1234567", `{"id":9007199254740993}`: "9007199254740992", `{"id":1e21}`: "1e+21",
		`{"id":1e-7}`: "1e-7", `{"id":0.000001}`: "0.000001", `{"id":"a","ID":"b"}`: "a",
	} {
		id, err := listkey.ItemID(json.RawMessage(item))
		require.NoError(t, err, item)
		require.Equal(t, expected, id, item)
	}
	for _, refused := range []string{`[]`, `{}`, `{"ID":"a"}`, `{"id":null}`, `{"id":true}`, `{"id":{}}`} {
		_, err := listkey.ItemID(json.RawMessage(refused))
		require.Error(t, err, refused)
	}
}
