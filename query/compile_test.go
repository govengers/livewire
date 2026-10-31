package query_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/query"
)

var partsSQL = &query.ResourceSQL{
	From:     "parts JOIN groups ON groups.id = parts.group_id",
	IDColumn: "parts.id",
	Derived:  []query.DerivedColumn{{Name: "groupName", SQL: "groups.name"}},
}

func literal(text string) query.Predicate {
	return func(builder *query.Builder) { builder.WriteString(text) }
}

func render(predicate query.Predicate) string {
	var builder query.Builder
	predicate(&builder)
	text, _ := builder.Query()
	return text
}

func TestBuilderCombinators(t *testing.T) {
	require.Nil(t, query.And())
	require.Nil(t, query.Or(nil, nil))
	require.Equal(t, "a", render(query.And(nil, literal("a"))))
	require.Equal(t, "(a) AND (b)", render(query.And(literal("a"), nil, literal("b"))))
	require.Equal(t, "(a) OR ((b) AND (c))", render(query.Or(literal("a"), query.And(literal("b"), literal("c")))))
	require.Equal(t, "NOT COALESCE((a), FALSE)", render(query.Not(literal("a"))))
	require.Equal(t, "FALSE", render(query.Not(nil)))

	var builder query.Builder
	builder.WriteString("x = ").Argument(1).WriteString(" AND y IS ").Argument(nil).WriteString(" AND z = ").Argument("two")
	text, arguments := builder.Query()
	require.Equal(t, "x = $1 AND y IS NULL AND z = $2", text)
	require.Equal(t, []any{1, "two"}, arguments)
}

// TestBuilderParenthesizesBareOperands pins the composition that leaks rows without the
// parentheses: a scope AND a caller's `a OR b`, and NOT over a bare disjunction.
func TestBuilderParenthesizesBareOperands(t *testing.T) {
	disjunction := literal("team = 1 OR team = 2")
	require.Equal(t, "(tenant = 7) AND (team = 1 OR team = 2)", render(query.And(literal("tenant = 7"), disjunction)))
	require.Equal(t, "NOT COALESCE((team = 1 OR team = 2), FALSE)", render(query.Not(disjunction)))
	require.Equal(t, "(a = 1 AND b = 2) OR (c = 3)", render(query.Or(literal("a = 1 AND b = 2"), literal("c = 3"))))
}

func TestCompileConditions(t *testing.T) {
	testCases := []struct {
		name          string
		raw           string
		wantSQL       string
		wantArguments []any
	}{
		{"eq casts the integer", `{"field":"port","op":"eq","value":1}`, `(parts.port) = $1::bigint`, []any{int64(1)}},
		{"ne keeps NULL rows", `{"field":"name","op":"ne","value":"a"}`, `(parts.name) IS DISTINCT FROM $1`, []any{"a"}},
		{"ne on an integer", `{"field":"port","op":"ne","value":1}`, `(parts.port) IS DISTINCT FROM $1::bigint`, []any{int64(1)}},
		{
			"gt gte lt lte", `{"and":[{"field":"load","op":"gt","value":1},{"field":"load","op":"gte","value":2},{"field":"load","op":"lt","value":3},{"field":"load","op":"lte","value":4}]}`,
			`((parts.load) > $1::float8) AND ((parts.load) >= $2::float8) AND ((parts.load) < $3::float8) AND ((parts.load) <= $4::float8)`,
			[]any{1.0, 2.0, 3.0, 4.0},
		},
		{"in", `{"field":"status","op":"in","value":["online","offline"]}`, `(parts.status) IN ($1, $2)`, []any{"online", "offline"}},
		{"in casts the integers", `{"field":"port","op":"in","value":[1,2]}`, `(parts.port) IN ($1::bigint, $2::bigint)`, []any{int64(1), int64(2)}},
		{"notIn keeps NULL rows", `{"field":"status","op":"notIn","value":["online"]}`, `NOT COALESCE(((parts.status) IN ($1)), FALSE)`, []any{"online"}},
		{"not in equals notIn", `{"not":{"field":"status","op":"in","value":["online"]}}`, `NOT COALESCE(((parts.status) IN ($1)), FALSE)`, []any{"online"}},
		{"contains escapes wildcards", `{"field":"name","op":"contains","value":"50%_a\\b"}`, `(parts.name) ILIKE $1 ESCAPE '\'`, []any{`%50\%\_a\\b%`}},
		{"startsWith", `{"field":"name","op":"startsWith","value":"x_"}`, `(parts.name) ILIKE $1 ESCAPE '\'`, []any{`x\_%`}},
		{"isNull true", `{"field":"name","op":"isNull","value":true}`, `(parts.name) IS NULL`, nil},
		{"isNull false", `{"field":"name","op":"isNull","value":false}`, `(parts.name) IS NOT NULL`, nil},
		{"bool", `{"field":"enabled","op":"eq","value":true}`, `(parts.enabled) = $1`, []any{true}},
		{"uuid", `{"field":"groupId","op":"eq","value":"6F1C1F2E-8A39-4C7A-9F7E-1A2B3C4D5E6F"}`, `(parts.group_id) = $1`, []any{"6f1c1f2e-8a39-4c7a-9f7e-1a2b3c4d5e6f"}},
		{"time in UTC", `{"field":"seenAt","op":"lte","value":"2026-09-23T12:00:00+02:00"}`, `(parts.seen_at) <= $1`, []any{time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}},
		{"relative time reads the clock", `{"field":"seenAt","op":"gt","value":{"relative":"-1h"}}`, `(parts.seen_at) > (now() + make_interval(secs => $1))`, []any{-3600.0}},
		{"has", `{"field":"tags","op":"has","value":"a"}`, `$1 = ANY((parts.tags))`, []any{"a"}},
		{"hasAny", `{"field":"tags","op":"hasAny","value":["a","b"]}`, `(parts.tags) && ARRAY[$1, $2]::text[]`, []any{"a", "b"}},
		{"hasAll", `{"field":"tags","op":"hasAll","value":["a"]}`, `(parts.tags) @> ARRAY[$1]::text[]`, []any{"a"}},
		{"isEmpty true", `{"field":"tags","op":"isEmpty","value":true}`, `COALESCE(cardinality((parts.tags)), 0) = 0`, nil},
		{"isEmpty false", `{"field":"tags","op":"isEmpty","value":false}`, `COALESCE(cardinality((parts.tags)), 0) > 0`, nil},
		{
			"nested not and or", `{"and":[{"field":"port","op":"eq","value":1},{"not":{"or":[{"field":"name","op":"eq","value":"a"},{"not":{"field":"enabled","op":"eq","value":true}}]}}]}`,
			`((parts.port) = $1::bigint) AND (NOT COALESCE((((parts.name) = $2) OR (NOT COALESCE(((parts.enabled) = $3), FALSE))), FALSE))`,
			[]any{int64(1), "a", true},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			text, arguments := compileSQL(t, `{"where":`+testCase.raw+`}`)
			require.Equal(t, testCase.wantSQL, text)
			require.Equal(t, testCase.wantArguments, arguments)
		})
	}
}

func TestCompileSearch(t *testing.T) {
	text, arguments := compileSQL(t, `{"search":"a%b"}`)
	require.Equal(t, `((parts.name) ILIKE $1 ESCAPE '\') OR ((parts.note) ILIKE $2 ESCAPE '\')`, text)
	require.Equal(t, []any{`%a\%b%`, `%a\%b%`}, arguments)

	text, _ = compileSQL(t, `{"where":{"field":"port","op":"eq","value":1},"search":"x"}`)
	require.Equal(t, `((parts.port) = $1::bigint) AND (((parts.name) ILIKE $2 ESCAPE '\') OR ((parts.note) ILIKE $3 ESCAPE '\'))`, text)
}

func TestCompileSearchOverGuardedFieldsMatchesNothing(t *testing.T) {
	document := mustParse(t, `{"search":"x"}`)
	visible := query.Fields{{Name: "port", Type: query.TypeInteger, SQL: "parts.port", Filter: true}}
	listQuery, err := query.Compile(visible, document, nil, query.Page{}, nil)
	require.NoError(t, err)
	require.Equal(t, "FALSE", render(listQuery.Where))
}

func TestCompileWithoutDocumentOrScope(t *testing.T) {
	listQuery, err := query.Compile(testFields, nil, []query.SortTerm{{Field: "seenAt", Descending: true}}, query.Page{Limit: 5, Offset: 10}, nil)
	require.NoError(t, err)
	require.Nil(t, listQuery.Where)
	require.Equal(t, []query.OrderTerm{{Field: "seenAt", SQL: "parts.seen_at", Descending: true, Nullable: true}}, listQuery.Order)
	require.Equal(t, 5, listQuery.Limit)
	require.Equal(t, 10, listQuery.Offset)

	empty, err := query.Compile(testFields, mustParse(t, `{}`), nil, query.Page{}, nil)
	require.NoError(t, err)
	require.Nil(t, empty.Where)
}

func TestCompileRefusesSortOnUnknownField(t *testing.T) {
	_, err := query.Compile(testFields, nil, []query.SortTerm{{Field: "nope"}}, query.Page{}, nil)
	fields := requireQueryError(t, err, query.CodeInvalidSort)
	require.Contains(t, fields, "sort[0]")
}

func TestCompileRefusesFilterOnFieldOutsideTheVisibleSet(t *testing.T) {
	document := mustParse(t, `{"where":{"not":{"field":"port","op":"eq","value":1}}}`)
	_, err := query.Compile(query.Fields{}, document, nil, query.Page{}, nil)
	requireFilterPath(t, err, "where")
}

func TestCompileScopeComesFirst(t *testing.T) {
	scope := func(builder *query.Builder) { builder.WriteString("parts.owner = ").Argument("me") }
	listQuery, err := query.Compile(testFields, mustParse(t, `{"where":{"field":"port","op":"eq","value":1}}`), nil, query.Page{Limit: 1}, scope)
	require.NoError(t, err)
	var builder query.Builder
	listQuery.Where(&builder)
	text, arguments := builder.Query()
	require.Equal(t, "(parts.owner = $1) AND ((parts.port) = $2::bigint)", text)
	require.Equal(t, []any{"me", int64(1)}, arguments)
}

func TestPageSQL(t *testing.T) {
	sort := []query.SortTerm{{Field: "seenAt", Descending: true}, {Field: "name"}, {Field: "load", Descending: true}}
	listQuery, err := query.Compile(testFields, mustParse(t, `{"where":{"field":"port","op":"eq","value":1}}`), sort, query.Page{Limit: 20, Offset: 40}, nil)
	require.NoError(t, err)
	text, arguments := listQuery.PageSQL(partsSQL)
	require.Equal(t, `SELECT parts.id, (groups.name), (parts.seen_at), (parts.name), (parts.load)`+
		` FROM parts JOIN groups ON groups.id = parts.group_id`+
		` WHERE (parts.port) = $1::bigint`+
		` ORDER BY (parts.seen_at) DESC NULLS LAST, (parts.name) ASC, (parts.load) DESC, parts.id ASC`+
		` LIMIT $2 OFFSET $3`, text)
	require.Equal(t, []any{int64(1), 20, 40}, arguments)
}

func TestPageSQLWithoutFilterOrSort(t *testing.T) {
	listQuery, err := query.Compile(testFields, nil, nil, query.Page{Limit: 3}, nil)
	require.NoError(t, err)
	text, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	require.Equal(t, `SELECT parts.id FROM parts ORDER BY parts.id ASC LIMIT $1 OFFSET $2`, text)
	require.Equal(t, []any{3, 0}, arguments)
}

func TestPageSQLInCursorMode(t *testing.T) {
	sort := []query.SortTerm{{Field: "seenAt", Descending: true}, {Field: "port"}, {Field: "name"}}
	seenAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	cursor := &query.Cursor{Sort: sort, Values: []any{seenAt, nil, "widget"}, ID: alpha}
	listQuery, err := query.Compile(testFields, mustParse(t, `{"search":"w"}`), sort, query.Page{Limit: 20, Offset: 99, After: cursor}, nil)
	require.NoError(t, err)
	text, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	require.Equal(t, `SELECT parts.id, (parts.seen_at), (parts.port), (parts.name) FROM parts`+
		` WHERE (((parts.name) ILIKE $1 ESCAPE '\') OR ((parts.note) ILIKE $2 ESCAPE '\'))`+
		` AND ((((parts.seen_at) < $3) OR ((parts.seen_at) IS NULL))`+
		` OR (((parts.seen_at) = $4) AND ((parts.port) IS NULL) AND ((parts.name) > $5))`+
		` OR (((parts.seen_at) = $6) AND ((parts.port) IS NULL) AND ((parts.name) = $7) AND (parts.id > $8)))`+
		` ORDER BY (parts.seen_at) DESC NULLS LAST, (parts.port) ASC, (parts.name) ASC, parts.id ASC LIMIT $9`, text)
	require.Equal(t, []any{"%w%", "%w%", seenAt, seenAt, "widget", seenAt, "widget", alpha, 20}, arguments)
}

func TestPageSQLKeysetCastsIntegers(t *testing.T) {
	sort := []query.SortTerm{{Field: "port"}}
	cursor := &query.Cursor{Sort: sort, Values: []any{int64(80)}, ID: alpha}
	listQuery, err := query.Compile(testFields, nil, sort, query.Page{Limit: 1, After: cursor}, nil)
	require.NoError(t, err)
	text, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	require.Equal(t, `SELECT parts.id, (parts.port) FROM parts`+
		` WHERE (((parts.port) > $1::bigint) OR ((parts.port) IS NULL)) OR (((parts.port) = $2::bigint) AND (parts.id > $3))`+
		` ORDER BY (parts.port) ASC, parts.id ASC LIMIT $4`, text)
	require.Equal(t, []any{int64(80), int64(80), alpha, 1}, arguments)
}

func TestPageSQLKeysetBoundsANonNullableLeadingTerm(t *testing.T) {
	sort := []query.SortTerm{{Field: "load", Descending: true}}
	cursor := &query.Cursor{Sort: sort, Values: []any{2.5}, ID: alpha}
	listQuery, err := query.Compile(testFields, nil, sort, query.Page{Limit: 3, After: cursor}, nil)
	require.NoError(t, err)
	text, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	require.Equal(t, `SELECT parts.id, (parts.load) FROM parts`+
		` WHERE ((parts.load) <= $1::float8) AND (((parts.load) < $2::float8) OR (((parts.load) = $3::float8) AND (parts.id > $4)))`+
		` ORDER BY (parts.load) DESC, parts.id ASC LIMIT $5`, text)
	require.Equal(t, []any{2.5, 2.5, 2.5, alpha, 3}, arguments)
}

func TestNextCursor(t *testing.T) {
	sort := []query.SortTerm{{Field: "seenAt", Descending: true}, {Field: "port"}}
	listQuery, err := query.Compile(testFields, nil, sort, query.Page{Limit: 1}, nil)
	require.NoError(t, err)
	cursor := listQuery.NextCursor([]any{nil, int64(1)}, alpha)
	require.Equal(t, &query.Cursor{Sort: sort, Values: []any{nil, int64(1)}, ID: alpha}, cursor)
	encoded, err := cursor.Encode()
	require.NoError(t, err)
	decoded, err := query.ParseCursor(testFields, encoded)
	require.NoError(t, err)
	require.Equal(t, cursor, decoded)
}

func TestCountSQL(t *testing.T) {
	listQuery, err := query.Compile(testFields, mustParse(t, `{"where":{"field":"port","op":"eq","value":1}}`), nil, query.Page{Limit: 1}, nil)
	require.NoError(t, err)
	listQuery.Cursor = &query.Cursor{ID: "ignored"}
	text, arguments := listQuery.CountSQL(partsSQL)
	require.Equal(t, `SELECT count(*) FROM parts JOIN groups ON groups.id = parts.group_id WHERE (parts.port) = $1::bigint`, text)
	require.Equal(t, []any{int64(1)}, arguments)

	listQuery.CountCap = math.MaxInt
	_, arguments = listQuery.CountSQL(partsSQL)
	require.Equal(t, []any{int64(1), math.MaxInt}, arguments)

	listQuery.CountCap = 1000
	text, arguments = listQuery.CountSQL(partsSQL)
	require.Equal(t, `SELECT count(*) FROM (SELECT 1 FROM parts JOIN groups ON groups.id = parts.group_id WHERE (parts.port) = $1::bigint LIMIT $2) AS capped`, text)
	require.Equal(t, []any{int64(1), 1001}, arguments)

	unfiltered, err := query.Compile(testFields, nil, nil, query.Page{}, nil)
	require.NoError(t, err)
	text, arguments = unfiltered.CountSQL(partsSQL)
	require.Equal(t, `SELECT count(*) FROM parts JOIN groups ON groups.id = parts.group_id`, text)
	require.Empty(t, arguments)
}

func TestDistinctSQL(t *testing.T) {
	scope := func(builder *query.Builder) { builder.WriteString("parts.owner = ").Argument("me") }
	listQuery, err := query.Compile(testFields, nil, nil, query.Page{}, scope)
	require.NoError(t, err)
	status, _ := testFields.Lookup("status")
	text, arguments, err := listQuery.DistinctSQL(partsSQL, status, 1000)
	require.NoError(t, err)
	require.Equal(t, `SELECT (parts.status) AS value, count(*) FROM parts JOIN groups ON groups.id = parts.group_id WHERE parts.owner = $1 GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $2`, text)
	require.Equal(t, []any{"me", 1000}, arguments)

	for _, name := range []string{"tags", "secret", "data"} {
		field, _ := testFields.Lookup(name)
		_, _, err := listQuery.DistinctSQL(partsSQL, field, 1000)
		requireFilterPath(t, err, "field")
	}
}
