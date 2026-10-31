package query_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/query"
)

var testFields = query.Fields{
	{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
	{Name: "name", Type: query.TypeString, SQL: "parts.name", Filter: true, Sort: true, Search: true},
	{Name: "note", Type: query.TypeText, SQL: "parts.note", Filter: true, Sort: true, Search: true, Nullable: true},
	{Name: "port", Type: query.TypeInteger, SQL: "parts.port", Filter: true, Sort: true, Nullable: true},
	{Name: "load", Type: query.TypeFloat, SQL: "parts.load", Filter: true, Sort: true},
	{Name: "ratio", Type: query.TypeFloat, SQL: "parts.ratio", Filter: true, Sort: true, Nullable: true},
	{Name: "enabled", Type: query.TypeBoolean, SQL: "parts.enabled", Filter: true, Nullable: true},
	{Name: "seenAt", Type: query.TypeTime, SQL: "parts.seen_at", Filter: true, Sort: true, Nullable: true},
	{Name: "status", Type: query.TypeEnum, Enum: []string{"online", "offline"}, SQL: "parts.status", Filter: true, Sort: true, Nullable: true},
	{Name: "groupId", Type: query.TypeUUID, SQL: "parts.group_id", Filter: true, Sort: true, Nullable: true},
	{Name: "tags", Type: query.TypeStringArray, SQL: "parts.tags", Filter: true},
	{Name: "data", Type: query.TypeJSON, SQL: "parts.data"},
	{Name: "secret", Type: query.TypeString, SQL: "parts.secret"},
}

const (
	alpha = "00000000-0000-0000-0000-000000000001"
	beta  = "00000000-0000-0000-0000-000000000002"
	gamma = "00000000-0000-0000-0000-000000000003"
	group = "00000000-0000-0000-0000-00000000000a"
)

func requireQueryError(t *testing.T, err error, code query.Code) map[string]string {
	t.Helper()
	var queryError *query.Error
	require.ErrorAs(t, err, &queryError)
	require.Equal(t, code, queryError.Code)
	require.Len(t, queryError.Fields, 1)
	return queryError.Fields
}

func requireFilterPath(t *testing.T, err error, path string) {
	t.Helper()
	fields := requireQueryError(t, err, query.CodeInvalidFilter)
	_, found := fields[path]
	require.True(t, found, "error %v names path %v, want %q", err, fields, path)
}

func mustParse(t *testing.T, raw string) *query.Document {
	t.Helper()
	document, err := query.Parse(testFields, []byte(raw))
	require.NoError(t, err)
	return document
}

func compileSQL(t *testing.T, raw string) (string, []any) {
	t.Helper()
	listQuery, err := query.Compile(testFields, mustParse(t, raw), nil, query.Page{Limit: 10}, nil)
	require.NoError(t, err)
	var builder query.Builder
	listQuery.Where(&builder)
	return builder.Query()
}

func TestErrorText(t *testing.T) {
	_, err := query.Parse(testFields, []byte(`{"where":{"field":"nope","op":"eq","value":1}}`))
	require.EqualError(t, err, `invalid_filter: the filter is not valid for this resource (where.field: unknown or unfilterable field "nope")`)
	require.True(t, errors.As(err, new(*query.Error)))
}
