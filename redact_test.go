package livewire_test

import (
	"encoding/json"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

func TestRedact(t *testing.T) {
	fields := partFields()
	fields = append(fields, query.Field{Name: "cost", Type: query.TypeFloat, SQL: "parts.cost", Read: authorization.IsAdmin()})
	list := json.RawMessage(`[{"id":"a","name":"bolt","secret":"s","cost":1},{"id":"b","name":"nut"}]`)
	object := json.RawMessage(`{"id":"a","name":"bolt","secret":"s"}`)

	redacted, err := livewire.Redact(userContext(), fields, list)
	require.NoError(t, err)
	require.JSONEq(t, `[{"id":"a","name":"bolt"},{"id":"b","name":"nut"}]`, string(redacted))

	redacted, err = livewire.Redact(userContext(), fields, object)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"a","name":"bolt"}`, string(redacted))

	same, err := livewire.Redact(adminContext(), fields, list)
	require.NoError(t, err)
	require.Equal(t, list, same, "nothing to hide returns the input")

	empty, err := livewire.Redact(userContext(), fields, json.RawMessage(`[]`))
	require.NoError(t, err)
	require.Equal(t, `[]`, string(empty))

	_, err = livewire.Redact(userContext(), fields, json.RawMessage(`"text"`))
	require.ErrorContains(t, err, "neither a list nor an object")
}
