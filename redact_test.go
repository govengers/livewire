package livewire_test

import (
	"encoding/json"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

func TestRedactItems(t *testing.T) {
	fields := partFields()
	fields = append(fields, query.Field{Name: "cost", Type: query.TypeFloat, SQL: "parts.cost", Read: authorization.IsAdmin()})
	list := json.RawMessage(`[{"id":"a","name":"bolt","secret":"s","cost":1},{"id":"b","name":"nut"}]`)

	redacted, err := livewire.RedactItems(userContext(), fields, list)
	require.NoError(t, err)
	require.JSONEq(t, `[{"id":"a","name":"bolt"},{"id":"b","name":"nut"}]`, string(redacted))

	same, err := livewire.RedactItems(adminContext(), fields, list)
	require.NoError(t, err)
	require.Equal(t, list, same, "nothing to hide returns the input")

	empty, err := livewire.RedactItems(userContext(), fields, json.RawMessage(`[]`))
	require.NoError(t, err)
	require.Equal(t, `[]`, string(empty))

	envelope, err := json.Marshal(livewire.ListResponse[json.RawMessage]{Total: 1, Items: []json.RawMessage{json.RawMessage(`{"id":"a","secret":"s"}`)}})
	require.NoError(t, err)
	for name, raw := range map[string]json.RawMessage{"a list response": envelope, "an object": json.RawMessage(`{"id":"a"}`), "a null item": json.RawMessage(`[null]`), "text": json.RawMessage(`"x"`), "null": json.RawMessage(`null`)} {
		_, err := livewire.RedactItems(userContext(), fields, raw)
		require.Error(t, err, name)
	}
}

func TestRedactItem(t *testing.T) {
	fields := partFields()
	redacted, err := livewire.RedactItem(userContext(), fields, json.RawMessage(`{"id":"a","name":"bolt","secret":"s"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"a","name":"bolt"}`, string(redacted))

	object := json.RawMessage(`{"id":"a","secret":"s"}`)
	same, err := livewire.RedactItem(adminContext(), fields, object)
	require.NoError(t, err)
	require.Equal(t, object, same)

	for name, raw := range map[string]json.RawMessage{"an array": json.RawMessage(`[{"id":"a"}]`), "text": json.RawMessage(`"x"`), "null": json.RawMessage(`null`)} {
		_, err := livewire.RedactItem(userContext(), fields, raw)
		require.Error(t, err, name)
	}
}
