package postgresadapter_test

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

func TestWriteCSVWritesEveryValueAsText(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	var output bytes.Buffer
	_, err := postgresadapter.WriteCSV(t.Context(), database.pool, &postgresadapter.Export{
		Specification: partSQL,
		Query:         compile(t, `{"where":{"field":"id","op":"eq","value":"00000000-0000-7000-8000-000000000007"}}`, "", query.Page{}),
		Columns: query.Fields{
			{Name: "ranked", SQL: "parts.rank IS NOT NULL"},
			{Name: "half", SQL: "parts.rank::float8 / 2"},
			{Name: "small", SQL: "parts.rank::smallint"},
			{Name: "big", SQL: "parts.rank::bigint"},
			{Name: "amount", SQL: "parts.rank::numeric + 0.25"},
			{Name: "tags", SQL: "ARRAY[parts.name, 'x']"},
			{Name: "data", SQL: "jsonb_build_object('rank', parts.rank)"},
			{Name: "past", SQL: "'-infinity'::timestamptz"},
		},
	}, &output)
	require.NoError(t, err)
	records, err := csv.NewReader(&output).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"true", "0.5", "1", "1", "1.25", `["part-2","x"]`, `{"rank":1}`, "-infinity"}, records[1])
}

// TestWriteCSVNeutralizesSpreadsheetFormulas pins D69: a text a spreadsheet would run gets a
// single quote in front, a number, a time or a text with the character elsewhere does not.
func TestWriteCSVNeutralizesSpreadsheetFormulas(t *testing.T) {
	t.Parallel()
	database := seedParts(t)
	var output bytes.Buffer
	columns := query.Fields{
		{Name: "equals", SQL: "'=HYPERLINK(\"http://x\")'::text"},
		{Name: "plus", SQL: "'+1'::text"},
		{Name: "minus", SQL: "'-1+1'::text"},
		{Name: "at", SQL: "'@SUM(A1)'::text"},
		{Name: "tab", SQL: "E'\\t=1'"},
		{Name: "return", SQL: "E'\\r=1'"},
		{Name: "inside", SQL: "'a=b'::text"},
		{Name: "number", SQL: "-parts.rank"},
		{Name: "past", SQL: "'-infinity'::timestamptz"},
		{Name: "empty", SQL: "''::text"},
	}
	_, err := postgresadapter.WriteCSV(t.Context(), database.pool, &postgresadapter.Export{
		Specification: partSQL,
		Query:         compile(t, `{"where":{"field":"id","op":"eq","value":"00000000-0000-7000-8000-000000000007"}}`, "", query.Page{}),
		Columns:       columns,
	}, &output)
	require.NoError(t, err)
	records, err := csv.NewReader(&output).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{`'=HYPERLINK("http://x")`, "'+1", "'-1+1", "'@SUM(A1)", "'\t=1", "'\r=1", "a=b", "-1", "-infinity", ""}, records[1])
}

func TestStoresReportDatabaseFailures(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	closed, err := pgxpool.New(t.Context(), database.dsn())
	require.NoError(t, err)
	closed.Close()
	ctx := t.Context()

	filters, err := postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: closed})
	require.NoError(t, err)
	_, err = filters.Save(ctx, unnamed("a"))
	require.Error(t, err)
	named := unnamed("a")
	named.Name = "Mine"
	_, err = filters.Save(ctx, named)
	require.Error(t, err)
	_, err = filters.Get(ctx, alice, uuid.NewString())
	require.Error(t, err)
	_, err = filters.List(ctx, alice, "")
	require.Error(t, err)
	require.Error(t, filters.Delete(ctx, alice, uuid.NewString()))
	_, err = filters.Sweep(ctx)
	require.Error(t, err)

	presences, err := postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: closed})
	require.NoError(t, err)
	require.Error(t, presences.Join(ctx, &livewire.Presence{Owner: alice}))
	require.Error(t, presences.Leave(ctx, "", "", "", ""))
	require.Error(t, presences.Heartbeat(ctx, ""))
	_, err = presences.List(ctx, "", "", "")
	require.Error(t, err)
	_, err = presences.Sweep(ctx, time.Minute)
	require.Error(t, err)

	idempotency, err := postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: closed})
	require.NoError(t, err)
	_, err = idempotency.Sweep(ctx)
	require.Error(t, err)
	audit, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: closed})
	require.NoError(t, err)
	require.Error(t, audit.Write(ctx, &livewire.AuditEntry{}))
	require.Error(t, audit.Maintain(ctx))
	_, err = postgresadapter.ListPage(ctx, closed, partSQL, compile(t, "", "", query.Page{Limit: 1}), hydrateParts)
	require.Error(t, err)
	_, err = postgresadapter.WriteCSV(ctx, closed, &postgresadapter.Export{Specification: partSQL, Query: compile(t, "", "", query.Page{})}, &bytes.Buffer{})
	require.Error(t, err)
}
