package query_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/query"
)

func TestFieldsValidate(t *testing.T) {
	require.NoError(t, testFields.Validate())
	testCases := []struct {
		name   string
		fields query.Fields
		want   string
	}{
		{"no name", query.Fields{{Type: query.TypeInteger, SQL: "t.x"}}, "has no name"},
		{"unknown type", query.Fields{{Name: "x", Type: "blob", SQL: "t.x"}}, "unknown type"},
		{"no SQL", query.Fields{{Name: "x", Type: query.TypeInteger}}, "no SQL expression"},
		{"enum without values", query.Fields{{Name: "x", Type: query.TypeEnum, SQL: "t.x"}}, "has no values"},
		{"values on a non-enum", query.Fields{{Name: "x", Type: query.TypeString, SQL: "t.x", Enum: []string{"a"}}}, "has enum values"},
		{"search on int", query.Fields{{Name: "x", Type: query.TypeInteger, SQL: "t.x", Search: true}}, "searchable"},
		{"filter on json", query.Fields{{Name: "x", Type: query.TypeJSON, SQL: "t.x", Filter: true}}, "cannot be filterable"},
		{"sort on json", query.Fields{{Name: "x", Type: query.TypeJSON, SQL: "t.x", Sort: true}}, "cannot be sortable"},
		{"sort on stringArray", query.Fields{{Name: "x", Type: query.TypeStringArray, SQL: "t.x", Sort: true}}, "cannot be sortable"},
		{"duplicate name", query.Fields{{Name: "x", Type: query.TypeInteger, SQL: "t.x"}, {Name: "x", Type: query.TypeText, SQL: "t.y"}}, "declared twice"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.fields.Validate()
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestFieldsLookup(t *testing.T) {
	field, found := testFields.Lookup("port")
	require.True(t, found)
	require.Equal(t, "parts.port", field.SQL)
	_, found = testFields.Lookup("nope")
	require.False(t, found)
}

func TestFieldsVisible(t *testing.T) {
	type key struct{}
	refuseWithoutKey := func(ctx context.Context) error {
		if ctx.Value(key{}) == nil {
			return errors.New("forbidden")
		}
		return nil
	}
	fields := query.Fields{
		{Name: "open", Type: query.TypeInteger, SQL: "t.open"},
		{Name: "guarded", Type: query.TypeInteger, SQL: "t.guarded", Read: refuseWithoutKey},
	}
	require.Equal(t, []string{"open"}, names(fields.Visible(t.Context())))
	require.Equal(t, []string{"open", "guarded"}, names(fields.Visible(context.WithValue(t.Context(), key{}, true))))
	require.Len(t, fields, 2)
}

func names(fields query.Fields) []string {
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, field.Name)
	}
	return out
}
