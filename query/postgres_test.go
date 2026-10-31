package query_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/query"
)

// The rows the SQL semantics are proven on: alpha is NULL in data and secret only, beta is
// NULL in note, port, seen_at, group_id, tags, data and secret, gamma is NULL in enabled,
// status, ratio, data and secret and has an empty tags array. The walk table holds
// generated rows with repeated sort values, several NULLs per column, a NaN and both
// infinities in weight, both infinite timestamps in at, and real fractions in share.
const schema = `
CREATE TABLE parts (
	id uuid PRIMARY KEY, name text NOT NULL, note text, port int, load float8 NOT NULL, enabled bool,
	seen_at timestamptz, status text, group_id uuid, tags text[], data jsonb, secret text, ratio real
);
INSERT INTO parts VALUES
	('` + alpha + `', 'alpha', 'first', 80, 0.5, true, '2026-09-01T00:00:00Z', 'online', '` + group + `', '{a,b}', NULL, NULL, 0),
	('` + beta + `', 'beta', NULL, NULL, 1.5, false, NULL, 'offline', NULL, NULL, NULL, NULL, 0.1),
	('` + gamma + `', 'gamma', 'third', 443, 2.5, NULL, '2026-09-02T00:00:00Z', NULL, '` + group + `', '{}', NULL, NULL, NULL);

CREATE TABLE walk (id uuid PRIMARY KEY, rank int, label text, at timestamptz, owner uuid, weight float8, share real);
INSERT INTO walk
SELECT
	('00000000-0000-0000-0000-' || lpad(to_hex(n), 12, '0'))::uuid,
	CASE WHEN n % 7 = 0 THEN NULL ELSE n % 4 END,
	CASE WHEN n % 5 = 0 THEN NULL ELSE chr(97 + n % 3) END,
	CASE WHEN n = 15 THEN 'infinity' WHEN n = 30 THEN '-infinity' WHEN n % 4 = 0 THEN NULL
		ELSE '2026-01-01T00:00:00Z'::timestamptz + make_interval(hours => n % 6) END,
	CASE WHEN n % 6 = 0 THEN NULL ELSE ('00000000-0000-0000-0000-00000000000' || (n % 3)::text)::uuid END,
	CASE WHEN n = 13 THEN 'NaN'::float8 WHEN n = 26 THEN 'Infinity'::float8 WHEN n = 39 THEN '-Infinity'::float8
		WHEN n % 8 = 0 THEN NULL ELSE (n * 0.1)::float8 END,
	CASE WHEN n % 9 = 0 THEN NULL ELSE (n * 0.1)::real END
FROM generate_series(1, 40) AS n;

CREATE TABLE indexed (id uuid PRIMARY KEY, name text NOT NULL, created_at timestamptz NOT NULL);
INSERT INTO indexed
SELECT ('00000000-0000-0000-0001-' || lpad(to_hex(n), 12, '0'))::uuid, 'name-' || n, '2026-01-01T00:00:00Z'::timestamptz + make_interval(mins => n)
FROM generate_series(1, 5000) AS n;
CREATE INDEX indexed_name ON indexed (name);
CREATE INDEX indexed_created_at ON indexed (created_at, id);
ANALYZE indexed;
`

// table is one fixture table: its declared fields and where its rows come from.
type table struct {
	fields        query.Fields
	specification *query.ResourceSQL
}

var (
	tableSQL   = &query.ResourceSQL{From: "parts", IDColumn: "parts.id"}
	partsTable = table{fields: testFields, specification: tableSQL}
	walkTable  = table{fields: walkFields, specification: &query.ResourceSQL{From: "walk", IDColumn: "walk.id"}}
	walkFields = query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "walk.id", Filter: true, Sort: true},
		{Name: "rank", Type: query.TypeInteger, SQL: "walk.rank", Filter: true, Sort: true, Nullable: true},
		{Name: "label", Type: query.TypeText, SQL: "walk.label", Filter: true, Sort: true, Nullable: true},
		{Name: "at", Type: query.TypeTime, SQL: "walk.at", Filter: true, Sort: true, Nullable: true},
		{Name: "owner", Type: query.TypeUUID, SQL: "walk.owner", Filter: true, Sort: true, Nullable: true},
		{Name: "weight", Type: query.TypeFloat, SQL: "walk.weight", Filter: true, Sort: true, Nullable: true},
		{Name: "share", Type: query.TypeFloat, SQL: "walk.share", Filter: true, Sort: true, Nullable: true},
		// bucket is non-nullable with heavy ties: the leading bound of a cursor page must keep them.
		{Name: "bucket", Type: query.TypeInteger, SQL: "coalesce(walk.rank, -1)", Filter: true, Sort: true},
	}
	indexedTable = table{specification: &query.ResourceSQL{From: "indexed", IDColumn: "indexed.id"}, fields: query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "indexed.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeString, SQL: "indexed.name", Filter: true, Sort: true},
		{Name: "createdAt", Type: query.TypeTime, SQL: "indexed.created_at", Filter: true, Sort: true},
	}}
)

func startPostgres(t *testing.T) *pgx.Conn {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	database := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)). //nolint:gosec // G115: a TCP port fits a uint32
		RuntimePath(t.TempDir()).
		Logger(io.Discard))
	if err := database.Start(); err != nil {
		t.Skipf("embedded postgres did not start (download or platform): %v", err)
	}
	t.Cleanup(func() { require.NoError(t, database.Stop()) })

	connection, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres", port))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close(context.Background()) }) // the database stops right after; the close error carries nothing
	_, err = connection.Exec(t.Context(), schema)
	require.NoError(t, err)
	return connection
}

// pageIDs runs the page query and returns the ids in page order with the order values of
// the last row as the driver delivers them, ready for NextCursor; an infinite timestamptz
// is handed over as its string, as the NextCursor contract asks.
func pageIDs(t *testing.T, connection *pgx.Conn, specification *query.ResourceSQL, listQuery *query.ListQuery) ([]string, []any) {
	t.Helper()
	text, arguments := listQuery.PageSQL(specification)
	rows, err := connection.Query(t.Context(), text, arguments...)
	require.NoError(t, err, text)
	defer rows.Close()
	var ids []string
	var lastOrderValues []any
	for rows.Next() {
		var id string
		orderValues := make([]any, len(listQuery.Order))
		targets := []any{&id}
		for index := range orderValues {
			targets = append(targets, &orderValues[index])
		}
		require.NoError(t, rows.Scan(targets...))
		for index, value := range orderValues {
			if modifier, infinite := value.(pgtype.InfinityModifier); infinite {
				orderValues[index] = modifier.String()
			}
		}
		ids = append(ids, id)
		lastOrderValues = orderValues
	}
	require.NoError(t, rows.Err())
	return ids, lastOrderValues
}

func filteredIDs(t *testing.T, connection *pgx.Conn, raw string) []string {
	t.Helper()
	listQuery, err := query.Compile(testFields, mustParse(t, raw), nil, query.Page{Limit: 10}, nil)
	require.NoError(t, err)
	ids, _ := pageIDs(t, connection, tableSQL, listQuery)
	return ids
}

func TestPostgresSemantics(t *testing.T) {
	connection := startPostgres(t)

	t.Run("two-valued conditions", func(t *testing.T) {
		testCases := []struct {
			name string
			raw  string
			want []string
		}{
			{"eq", `{"where":{"field":"port","op":"eq","value":80}}`, []string{alpha}},
			{"ne includes the NULL row", `{"where":{"field":"note","op":"ne","value":"first"}}`, []string{beta, gamma}},
			{"in excludes the NULL row", `{"where":{"field":"status","op":"in","value":["online","offline"]}}`, []string{alpha, beta}},
			{"notIn includes the NULL row", `{"where":{"field":"status","op":"notIn","value":["online"]}}`, []string{beta, gamma}},
			{"not eq includes the NULL row", `{"where":{"not":{"field":"port","op":"eq","value":80}}}`, []string{beta, gamma}},
			{"not in equals notIn", `{"where":{"not":{"field":"status","op":"in","value":["online"]}}}`, []string{beta, gamma}},
			{"not contains includes the NULL row", `{"where":{"not":{"field":"note","op":"contains","value":"fir"}}}`, []string{beta, gamma}},
			{"gt excludes the NULL row", `{"where":{"field":"port","op":"gt","value":1}}`, []string{alpha, gamma}},
			{"gt beyond the int4 range", `{"where":{"field":"port","op":"gt","value":-3000000000}}`, []string{alpha, gamma}},
			{"eq beyond the int4 range", `{"where":{"field":"port","op":"eq","value":3000000000}}`, nil},
			{"in beyond the int4 range", `{"where":{"field":"port","op":"in","value":[3000000000,80]}}`, []string{alpha}},
			{"isNull", `{"where":{"field":"port","op":"isNull","value":true}}`, []string{beta}},
			{"isNull false", `{"where":{"field":"groupId","op":"isNull","value":false}}`, []string{alpha, gamma}},
			{"contains is case-insensitive", `{"where":{"field":"name","op":"contains","value":"ALPH"}}`, []string{alpha}},
			{"startsWith escapes wildcards", `{"where":{"field":"name","op":"startsWith","value":"a_"}}`, nil},
			{"has", `{"where":{"field":"tags","op":"has","value":"a"}}`, []string{alpha}},
			{"hasAny", `{"where":{"field":"tags","op":"hasAny","value":["b","z"]}}`, []string{alpha}},
			{"hasAll", `{"where":{"field":"tags","op":"hasAll","value":["a","b"]}}`, []string{alpha}},
			{"isEmpty counts NULL as empty", `{"where":{"field":"tags","op":"isEmpty","value":true}}`, []string{beta, gamma}},
			{"isEmpty false", `{"where":{"field":"tags","op":"isEmpty","value":false}}`, []string{alpha}},
			{"not hasAny includes the NULL row", `{"where":{"not":{"field":"tags","op":"hasAny","value":["a"]}}}`, []string{beta, gamma}},
			{"bool eq", `{"where":{"field":"enabled","op":"eq","value":true}}`, []string{alpha}},
			{"bool ne includes the NULL row", `{"where":{"field":"enabled","op":"ne","value":true}}`, []string{beta, gamma}},
			{"uuid eq", `{"where":{"field":"groupId","op":"eq","value":"` + group + `"}}`, []string{alpha, gamma}},
			{"relative time", `{"where":{"field":"seenAt","op":"lt","value":{"relative":"-1d"}}}`, []string{alpha, gamma}},
			{"relative time in the future", `{"where":{"field":"seenAt","op":"gt","value":{"relative":"+1d"}}}`, nil},
			{"absolute time", `{"where":{"field":"seenAt","op":"gte","value":"2026-09-02T02:00:00+02:00"}}`, []string{gamma}},
			{"float", `{"where":{"field":"load","op":"in","value":[0.5,2.5]}}`, []string{alpha, gamma}},
			{"search", `{"search":"IR"}`, []string{alpha, gamma}},
			{"search and filter", `{"where":{"field":"port","op":"gt","value":100},"search":"ir"}`, []string{gamma}},
			{"or", `{"where":{"or":[{"field":"name","op":"eq","value":"beta"},{"field":"port","op":"eq","value":443}]}}`, []string{beta, gamma}},
		}
		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				require.Equal(t, testCase.want, filteredIDs(t, connection, testCase.raw))
			})
		}
	})

	t.Run("a scope with a bare OR stays inside its AND", func(t *testing.T) {
		scope := func(builder *query.Builder) {
			builder.WriteString("parts.name = ").Argument("alpha").WriteString(" OR parts.name = ").Argument("beta")
		}
		filtered, err := query.Compile(testFields, mustParse(t, `{"where":{"field":"port","op":"isNull","value":true}}`), nil, query.Page{Limit: 10}, scope)
		require.NoError(t, err)
		ids, _ := pageIDs(t, connection, tableSQL, filtered)
		require.Equal(t, []string{beta}, ids)

		negated, err := query.Compile(testFields, nil, nil, query.Page{Limit: 10}, query.Not(scope))
		require.NoError(t, err)
		ids, _ = pageIDs(t, connection, tableSQL, negated)
		require.Equal(t, []string{gamma}, ids)
	})

	t.Run("nullable sort keeps NULLs last in both directions", func(t *testing.T) {
		ascending, err := query.Compile(testFields, nil, []query.SortTerm{{Field: "port"}}, query.Page{Limit: 10}, nil)
		require.NoError(t, err)
		ids, _ := pageIDs(t, connection, tableSQL, ascending)
		require.Equal(t, []string{alpha, gamma, beta}, ids)

		descending, err := query.Compile(testFields, nil, []query.SortTerm{{Field: "port", Descending: true}}, query.Page{Limit: 10}, nil)
		require.NoError(t, err)
		ids, _ = pageIDs(t, connection, tableSQL, descending)
		require.Equal(t, []string{gamma, alpha, beta}, ids)
	})

	t.Run("cursor pages walk every row once", func(t *testing.T) {
		for _, sort := range [][]query.SortTerm{
			{{Field: "port", Descending: true}},
			{{Field: "seenAt"}, {Field: "name", Descending: true}},
			{{Field: "status"}, {Field: "load"}},
			{{Field: "groupId"}},
			{{Field: "id"}},
		} {
			t.Run(fmt.Sprint(sort), func(t *testing.T) {
				whole, err := query.Compile(testFields, nil, sort, query.Page{Limit: 10}, nil)
				require.NoError(t, err)
				wantIDs, _ := pageIDs(t, connection, tableSQL, whole)
				require.Equal(t, wantIDs, walkPages(t, connection, partsTable, sort, 1))
			})
		}
	})

	t.Run("cursor pages of three walk ties and NULL runs", func(t *testing.T) {
		for _, sort := range [][]query.SortTerm{
			{{Field: "rank", Descending: true}, {Field: "label"}},
			{{Field: "label"}, {Field: "at", Descending: true}},
			{{Field: "at"}},
			{{Field: "owner", Descending: true}, {Field: "rank"}},
			{{Field: "rank"}, {Field: "label", Descending: true}, {Field: "at"}, {Field: "owner"}},
			{{Field: "bucket"}},
			{{Field: "bucket", Descending: true}},
			{{Field: "bucket"}, {Field: "label", Descending: true}},
			{{Field: "bucket", Descending: true}, {Field: "label"}},
		} {
			t.Run(fmt.Sprint(sort), func(t *testing.T) {
				whole, err := query.Compile(walkTable.fields, nil, sort, query.Page{Limit: 100}, nil)
				require.NoError(t, err)
				wantIDs, _ := pageIDs(t, connection, walkTable.specification, whole)
				require.Len(t, wantIDs, 40)
				require.Equal(t, wantIDs, walkPages(t, connection, walkTable, sort, 3))
			})
		}
	})

	t.Run("cursor pages of one walk NaN, infinities and real fractions", func(t *testing.T) {
		for _, sort := range [][]query.SortTerm{
			{{Field: "weight", Descending: true}},
			{{Field: "weight"}},
			{{Field: "share"}},
			{{Field: "share", Descending: true}, {Field: "weight"}},
			{{Field: "at", Descending: true}},
			{{Field: "at"}},
		} {
			t.Run(fmt.Sprint(sort), func(t *testing.T) {
				whole, err := query.Compile(walkTable.fields, nil, sort, query.Page{Limit: 100}, nil)
				require.NoError(t, err)
				wantIDs, _ := pageIDs(t, connection, walkTable.specification, whole)
				require.Len(t, wantIDs, 40)
				require.Equal(t, wantIDs, walkPages(t, connection, walkTable, sort, 1))
			})
		}
	})

	t.Run("a term declared non-nullable is wrong in cursor mode", func(t *testing.T) {
		declaredNotNull := table{specification: tableSQL, fields: query.Fields{
			{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Sort: true},
			{Name: "port", Type: query.TypeInteger, SQL: "parts.port", Sort: true},
		}}
		// Ascending skips the NULL row; descending the leading NULL row ends the walk.
		require.Equal(t, []string{alpha, gamma}, walkPages(t, connection, declaredNotNull, []query.SortTerm{{Field: "port"}}, 1))
		require.Equal(t, []string{beta}, walkPages(t, connection, declaredNotNull, []query.SortTerm{{Field: "port", Descending: true}}, 1))
	})

	t.Run("float arguments bind as float8 on a real column", func(t *testing.T) {
		require.Nil(t, filteredIDs(t, connection, `{"where":{"field":"ratio","op":"eq","value":1e-300}}`))
		require.Equal(t, []string{alpha}, filteredIDs(t, connection, `{"where":{"field":"ratio","op":"eq","value":0}}`))
		require.Equal(t, []string{beta}, filteredIDs(t, connection, `{"where":{"field":"ratio","op":"gt","value":1e-300}}`))
	})

	t.Run("a U+0000 never reaches the database", func(t *testing.T) {
		var postgresError *pgconn.PgError
		err := connection.QueryRow(t.Context(), "SELECT $1::text", "a\x00b").Scan(new(string))
		require.ErrorAs(t, err, &postgresError)
		require.Equal(t, "22021", postgresError.Code)

		_, err = query.Parse(testFields, []byte(`{"where":{"field":"name","op":"eq","value":"a\u0000b"}}`))
		requireFilterPath(t, err, "where.value")
		require.Equal(t, []string{alpha}, filteredIDs(t, connection, `{"where":{"field":"name","op":"eq","value":"alpha"}}`))
	})

	t.Run("filters are served by an index", func(t *testing.T) {
		for name, raw := range map[string]string{
			"eq":            `{"where":{"field":"name","op":"eq","value":"name-4242"}}`,
			"in":            `{"where":{"field":"name","op":"in","value":["name-1","name-2"]}}`,
			"gte on a time": `{"where":{"field":"createdAt","op":"gte","value":"2026-01-04T10:00:00Z"}}`,
		} {
			t.Run(name, func(t *testing.T) {
				document, err := query.Parse(indexedTable.fields, []byte(raw))
				require.NoError(t, err)
				listQuery, err := query.Compile(indexedTable.fields, document, []query.SortTerm{{Field: "createdAt"}}, query.Page{Limit: 3}, nil)
				require.NoError(t, err)
				require.Contains(t, explain(t, connection, listQuery, indexedTable), "Index Cond")
			})
		}
	})

	t.Run("a deep cursor page is served by an index range", func(t *testing.T) {
		for _, sort := range [][]query.SortTerm{{{Field: "createdAt"}}, {{Field: "createdAt", Descending: true}}} {
			t.Run(fmt.Sprint(sort), func(t *testing.T) {
				deep, err := query.Compile(indexedTable.fields, nil, sort, query.Page{Limit: 1, Offset: 4500}, nil)
				require.NoError(t, err)
				ids, orderValues := pageIDs(t, connection, indexedTable.specification, deep)
				require.Len(t, ids, 1)
				encoded, err := deep.NextCursor(orderValues, ids[0]).Encode()
				require.NoError(t, err)
				after, err := query.ParseCursor(indexedTable.fields, encoded)
				require.NoError(t, err)

				page, err := query.Compile(indexedTable.fields, nil, sort, query.Page{Limit: 3, After: after}, nil)
				require.NoError(t, err)
				plan := explain(t, connection, page, indexedTable)
				require.Contains(t, plan, "Index Cond: (created_at")
				nextIDs, _ := pageIDs(t, connection, indexedTable.specification, page)
				require.Len(t, nextIDs, 3)
			})
		}
	})

	t.Run("count cap", func(t *testing.T) {
		listQuery, err := query.Compile(testFields, nil, nil, query.Page{Limit: 1}, nil)
		require.NoError(t, err)
		require.Equal(t, 3, count(t, connection, listQuery))
		listQuery.CountCap = 1
		require.Equal(t, 2, count(t, connection, listQuery))
	})

	t.Run("distinct", func(t *testing.T) {
		listQuery, err := query.Compile(testFields, mustParse(t, `{"where":{"field":"load","op":"gt","value":0}}`), nil, query.Page{}, nil)
		require.NoError(t, err)
		status, _ := testFields.Lookup("status")
		text, arguments, err := listQuery.DistinctSQL(tableSQL, status, 10)
		require.NoError(t, err)
		rows, err := connection.Query(t.Context(), text, arguments...)
		require.NoError(t, err)
		values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]any, error) { return row.Values() })
		require.NoError(t, err)
		require.Equal(t, [][]any{{"offline", int64(1)}, {"online", int64(1)}, {nil, int64(1)}}, values)
	})
}

// walkPages reads pages of pageSize rows through encoded cursors until a page comes back
// short, and returns every id seen.
func walkPages(t *testing.T, connection *pgx.Conn, fixture table, sort []query.SortTerm, pageSize int) []string {
	t.Helper()
	var all []string
	var after *query.Cursor
	for range 100 {
		listQuery, err := query.Compile(fixture.fields, nil, sort, query.Page{Limit: pageSize, After: after}, nil)
		require.NoError(t, err)
		ids, orderValues := pageIDs(t, connection, fixture.specification, listQuery)
		all = append(all, ids...)
		if len(ids) < pageSize {
			return all
		}
		encoded, err := listQuery.NextCursor(orderValues, ids[len(ids)-1]).Encode()
		require.NoError(t, err)
		after, err = query.ParseCursor(fixture.fields, encoded)
		require.NoError(t, err)
	}
	t.Fatal("the cursor walk did not end")
	return nil
}

// explain returns the planner's text for the page query.
func explain(t *testing.T, connection *pgx.Conn, listQuery *query.ListQuery, fixture table) string {
	t.Helper()
	text, arguments := listQuery.PageSQL(fixture.specification)
	rows, err := connection.Query(t.Context(), "EXPLAIN "+text, arguments...)
	require.NoError(t, err)
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return strings.Join(lines, "\n")
}

func count(t *testing.T, connection *pgx.Conn, listQuery *query.ListQuery) int {
	t.Helper()
	text, arguments := listQuery.CountSQL(tableSQL)
	var total int
	require.NoError(t, connection.QueryRow(t.Context(), text, arguments...).Scan(&total))
	return total
}
