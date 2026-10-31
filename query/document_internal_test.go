package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var internalFields = Fields{
	{Name: "port", Type: TypeInteger, SQL: "t.port", Filter: true},
	{Name: "load", Type: TypeFloat, SQL: "t.load", Filter: true},
	{Name: "groupId", Type: TypeUUID, SQL: "t.group_id", Filter: true},
	{Name: "seenAt", Type: TypeTime, SQL: "t.seen_at", Filter: true},
}

func TestParseDecodesValuesByType(t *testing.T) {
	document, err := Parse(internalFields, []byte(`{"where":{"and":[
		{"field":"port","op":"eq","value":2.0},
		{"field":"load","op":"eq","value":2},
		{"field":"load","op":"gt","value":1e16},
		{"field":"groupId","op":"eq","value":"6F1C1F2E-8A39-4C7A-9F7E-1A2B3C4D5E6F"},
		{"field":"seenAt","op":"gt","value":{"relative":"-1h30m"}},
		{"field":"seenAt","op":"lt","value":"2026-09-23T12:00:00+02:00"}]}}`))
	require.NoError(t, err)
	values := make([]any, 0, 6)
	for _, child := range document.where.and {
		values = append(values, child.value)
	}
	require.Equal(t, []any{
		int64(2), float64(2), 1e16, "6f1c1f2e-8a39-4c7a-9f7e-1a2b3c4d5e6f",
		relative{duration: -90 * time.Minute},
		time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}, values)
	require.Equal(t, time.UTC, values[5].(time.Time).Location())
}

func TestParseRelative(t *testing.T) {
	accepted := map[string]time.Duration{
		"-7d":    -7 * 24 * time.Hour,
		"-1h30m": -90 * time.Minute,
		"+2w":    14 * 24 * time.Hour,
		"6h":     6 * time.Hour,
		"-45s":   -45 * time.Second,
		"5200w":  5200 * 7 * 24 * time.Hour,
	}
	for expression, want := range accepted {
		got, err := parseRelative(expression)
		require.NoError(t, err, expression)
		require.Equal(t, want, got, expression)
	}
	refused := []string{
		"7", "-1x", "", "-", "d", "-7 d", "--7d", "1.5h", "-99999999999999999999d", "-40000w", "5300w",
		"-20000w", "-30500w", "213504d", "106752d", "9223372036854775807s", "36600d1s",
	}
	for _, expression := range refused {
		_, err := parseRelative(expression)
		require.Error(t, err, expression)
	}
}
