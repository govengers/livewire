package postgresadapter_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

// The list fixture: 37 parts with five names, three statuses, a rank that is NULL on every
// fourth row and a due time that is NULL on every fifth, infinite on rows 3 and 6, and
// repeats over four hours otherwise.
const partsSeedSQL = `INSERT INTO parts (id, name, status, rank, due_at)
SELECT ('00000000-0000-7000-8000-' || lpad(to_hex(n), 12, '0'))::uuid, 'part-' || (n % 5),
	(ARRAY['draft', 'active', 'retired']::part_status[])[1 + n % 3],
	CASE WHEN n % 4 = 0 THEN NULL ELSE n % 3 END,
	CASE WHEN n = 3 THEN 'infinity' WHEN n = 6 THEN '-infinity' WHEN n % 5 = 0 THEN NULL
		ELSE '2026-01-01T00:00:00Z'::timestamptz + make_interval(hours => n % 4) END
FROM generate_series(1, 37) AS n`

const seededParts = 37

var (
	partFields = query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeText, SQL: "parts.name", Filter: true, Sort: true, Search: true},
		{Name: "status", Type: query.TypeEnum, SQL: "parts.status", Enum: []string{"draft", "active", "retired", "ghost"}, Filter: true, Sort: true},
		{Name: "rank", Type: query.TypeInteger, SQL: "parts.rank", Filter: true, Sort: true, Nullable: true},
		{Name: "dueAt", Type: query.TypeTime, SQL: "parts.due_at", Filter: true, Sort: true, Nullable: true},
	}
	partSQL = &query.ResourceSQL{From: "parts", IDColumn: "parts.id", Derived: []query.DerivedColumn{{Name: "shout", SQL: "upper(parts.name)"}}}
)

// part is the item of the list fixture.
type part struct {
	ID    uuid.UUID
	Name  string
	Shout string
}

func hydrateParts(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]part, error) {
	result, err := database.Query(ctx, "SELECT id, name, '' FROM parts WHERE id = ANY($1)", postgresadapter.PageIDs(rows))
	if err != nil {
		return nil, err
	}
	loaded, err := pgx.CollectRows(result, pgx.RowToStructByPos[part])
	if err != nil {
		return nil, err
	}
	shouts := map[uuid.UUID]string{}
	for _, row := range rows {
		shouts[row.ID], _ = row.Derived["shout"].(string)
	}
	byID := map[uuid.UUID]part{}
	for _, item := range loaded {
		item.Shout = shouts[item.ID]
		byID[item.ID] = item
	}
	return byID, nil
}

func seedParts(t *testing.T) *testDatabase {
	t.Helper()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), partsSeedSQL)
	require.NoError(t, err)
	return database
}

func compile(t *testing.T, document string, sort string, page query.Page) *query.ListQuery {
	t.Helper()
	var parsed *query.Document
	if document != "" {
		var err error
		parsed, err = query.Parse(partFields, []byte(document))
		require.NoError(t, err)
	}
	terms, err := query.ParseSort(partFields, sort)
	require.NoError(t, err)
	listQuery, err := query.Compile(partFields, parsed, terms, page, nil)
	require.NoError(t, err)
	return listQuery
}

// walk reads every page of sort in cursor mode through the cursors ListPage hands out.
func walk(t *testing.T, database postgresadapter.DBTX, sort string, limit int) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	var after *query.Cursor
	for pages := 0; ; pages++ {
		require.Less(t, pages, seededParts+2, "the walk ends")
		page, err := postgresadapter.ListPage(t.Context(), database, partSQL, compile(t, "", sort, query.Page{Limit: limit, After: after}), hydrateParts)
		require.NoError(t, err)
		require.Equal(t, seededParts, page.Total)
		for _, item := range page.Items {
			require.Equal(t, strings.ToUpper(item.Name), item.Shout, "the derived column reaches the hydration")
			ids = append(ids, item.ID)
		}
		if page.Next == nil {
			require.Less(t, len(page.Items), limit)
			return ids
		}
		after, err = query.ParseCursor(partFields, *page.Next)
		require.NoError(t, err)
	}
}

func TestListPageWalksCursorsOverTiesNullsAndInfinities(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	read, err := newTransactor(t, database.pool).Begin(t.Context(), livewire.TransactionDetails{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = read.Rollback(context.Background()) }) // a read transaction; nothing to keep
	handle := postgresadapter.TransactionFrom(livewire.WithTransaction(t.Context(), read.Handle()))

	for _, sort := range []string{"-dueAt,rank", "dueAt", "rank,-name", "status,-dueAt", "-rank", ""} {
		t.Run(sort, func(t *testing.T) {
			whole, err := postgresadapter.ListPage(t.Context(), database.pool, partSQL, compile(t, "", sort, query.Page{Limit: 100}), hydrateParts)
			require.NoError(t, err)
			require.Nil(t, whole.Next, "a page that is not full has no next")
			expected := make([]uuid.UUID, 0, seededParts)
			for _, item := range whole.Items {
				expected = append(expected, item.ID)
			}
			require.Len(t, expected, seededParts)
			require.Equal(t, expected, walk(t, database.pool, sort, 4), "on the pool")
			require.Equal(t, expected, walk(t, handle, sort, 5), "in the engine's read transaction")
		})
	}
}

func TestListPageCountsUnderTheCapAndFilters(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	listQuery := compile(t, `{"where":{"field":"name","op":"eq","value":"part-1"}}`, "", query.Page{Limit: 3})
	page, err := postgresadapter.ListPage(t.Context(), database.pool, partSQL, listQuery, hydrateParts)
	require.NoError(t, err)
	require.Equal(t, 8, page.Total)
	require.Len(t, page.Items, 3)
	require.NotNil(t, page.Next)

	listQuery = compile(t, "", "", query.Page{Limit: 0})
	listQuery.CountCap = 10
	page, err = postgresadapter.ListPage(t.Context(), database.pool, partSQL, listQuery, hydrateParts)
	require.NoError(t, err)
	require.Equal(t, 11, page.Total, "a capped total is at most the cap + 1")
	require.Empty(t, page.Items)
	require.NotNil(t, page.Items)
	require.Nil(t, page.Next)
}

func TestListPageMapsAForgedEnumCursorToInvalidCursor(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	terms, err := query.ParseSort(partFields, "status")
	require.NoError(t, err)
	forged, err := (&query.Cursor{Sort: terms, Values: []any{"bogus"}, ID: uuid.NewString()}).Encode()
	require.NoError(t, err)
	cursor, err := query.ParseCursor(partFields, forged)
	require.NoError(t, err, "the query package cannot know the native enum's values")
	_, err = postgresadapter.ListPage(t.Context(), database.pool, partSQL, compile(t, "", "status", query.Page{Limit: 5, After: cursor}), hydrateParts)
	var queryError *query.Error
	require.ErrorAs(t, err, &queryError)
	require.Equal(t, query.CodeInvalidCursor, queryError.Code)
	require.Contains(t, queryError.Fields, "after")

	_, err = postgresadapter.ListPage(t.Context(), database.pool, partSQL,
		compile(t, `{"where":{"field":"status","op":"eq","value":"ghost"}}`, "", query.Page{Limit: 5}), hydrateParts)
	require.Error(t, err)
	require.False(t, errors.As(err, &queryError), "outside cursor mode a 22P02 is the declaration's mistake, not the client's")
}

func TestListPageRefusesAnIncompleteHydration(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	listQuery := compile(t, "", "", query.Page{Limit: 5})
	_, err := postgresadapter.ListPage(t.Context(), database.pool, partSQL, listQuery,
		func(context.Context, postgresadapter.DBTX, []postgresadapter.PageRow) (map[uuid.UUID]part, error) {
			return map[uuid.UUID]part{}, nil
		})
	require.ErrorContains(t, err, "no item for id")
	_, err = postgresadapter.ListPage(t.Context(), database.pool, partSQL, listQuery,
		func(context.Context, postgresadapter.DBTX, []postgresadapter.PageRow) (map[uuid.UUID]part, error) {
			return nil, errors.New("hydration failed")
		})
	require.ErrorContains(t, err, "hydration failed")
	_, err = postgresadapter.ListPage(t.Context(), database.pool, &query.ResourceSQL{From: "missing", IDColumn: "missing.id"}, listQuery, hydrateParts)
	require.Error(t, err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = postgresadapter.ListPage(cancelled, database.pool, partSQL, listQuery, hydrateParts)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDistinctCountsTheValuesOfAField(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	status, _ := partFields.Lookup("status")
	values, err := postgresadapter.Distinct(t.Context(), database.pool, partSQL, compile(t, "", "", query.Page{Limit: 2}), status)
	require.NoError(t, err)
	require.Equal(t, []livewire.DistinctValue{{Value: "active", Count: 13}, {Value: "draft", Count: 12}}, values)

	id, _ := partFields.Lookup("id")
	values, err = postgresadapter.Distinct(t.Context(), database.pool, partSQL,
		compile(t, `{"where":{"field":"id","op":"eq","value":"00000000-0000-7000-8000-000000000001"}}`, "", query.Page{Limit: 5}), id)
	require.NoError(t, err)
	require.Equal(t, []livewire.DistinctValue{{Value: "00000000-0000-7000-8000-000000000001", Count: 1}}, values, "a uuid is hyphenated")

	_, err = postgresadapter.Distinct(t.Context(), database.pool, partSQL, compile(t, "", "", query.Page{Limit: 5}), query.Field{Name: "secret", Type: query.TypeText, SQL: "parts.name"})
	var queryError *query.Error
	require.ErrorAs(t, err, &queryError, "a field that is not filterable")
	_, err = postgresadapter.Distinct(t.Context(), database.pool, &query.ResourceSQL{From: "missing", IDColumn: "missing.id"}, compile(t, "", "", query.Page{Limit: 5}), status)
	require.Error(t, err)
}

func TestWriteCSVStreamsEveryRowInChunks(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	columns := slices.Clone(partFields)
	var output bytes.Buffer
	written, err := postgresadapter.WriteCSV(t.Context(), database.pool, &postgresadapter.Export{
		Specification: partSQL, Query: compile(t, "", "-dueAt", query.Page{Limit: 3, Offset: 10}), Columns: columns, ChunkSize: 5,
	}, &output)
	require.NoError(t, err)
	require.Equal(t, seededParts, written, "limit and offset do not bound an export")
	records, err := csv.NewReader(&output).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, seededParts+1)
	require.Equal(t, []string{"id", "name", "status", "rank", "dueAt"}, records[0])
	require.Equal(t, []string{"00000000-0000-7000-8000-000000000003", "part-3", "draft", "0", "infinity"}, records[1], "infinity sorts first descending")
	require.Equal(t, []string{"00000000-0000-7000-8000-000000000007", "part-2", "active", "1", "2026-01-01T03:00:00Z"}, records[2])
	seen := map[string]bool{}
	for _, record := range records[1:] {
		require.False(t, seen[record[0]], "every row once")
		seen[record[0]] = true
	}

	written, err = postgresadapter.WriteCSV(t.Context(), database.pool, &postgresadapter.Export{
		Specification: partSQL, Query: compile(t, `{"where":{"field":"name","op":"eq","value":"none"}}`, "", query.Page{}), Columns: columns[:1],
	}, &output)
	require.NoError(t, err)
	require.Zero(t, written)

	_, err = postgresadapter.WriteCSV(t.Context(), database.pool, &postgresadapter.Export{
		Specification: &query.ResourceSQL{From: "missing", IDColumn: "missing.id"}, Query: compile(t, "", "", query.Page{}), Columns: columns,
	}, &output)
	require.Error(t, err)
}
