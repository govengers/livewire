package postgresadapter_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/govengers/postgrestest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire/postgresadapter"
)

// The fixture schema: parts is a live, versioned table with a native enum, nullable sort
// columns and a timestamptz that holds infinities; gadgets is partitioned by tenant (D25);
// orders is a live, versioned, natively partitioned table.
const fixtureSQL = `
CREATE TYPE part_status AS ENUM ('draft', 'active', 'retired');
CREATE TABLE parts (
	id uuid PRIMARY KEY DEFAULT uuidv7(),
	name text NOT NULL,
	status part_status NOT NULL DEFAULT 'draft',
	rank integer,
	due_at timestamptz
);
CREATE TABLE gadgets (id uuid PRIMARY KEY DEFAULT uuidv7(), tenant_id text NOT NULL, name text NOT NULL);
CREATE TABLE orders (id uuid NOT NULL DEFAULT uuidv7(), region text NOT NULL, total integer NOT NULL, PRIMARY KEY (id, region))
	PARTITION BY LIST (region);
CREATE TABLE orders_eu PARTITION OF orders FOR VALUES IN ('eu');
CREATE TABLE orders_us PARTITION OF orders FOR VALUES IN ('us');
`

// postgres is the one PostgreSQL of this test binary (TST-002, govengers/postgrestest): every
// test gets a database of its own, copied from the template the package's SQL migrated.
var postgres *postgrestest.Server

func TestMain(m *testing.M) {
	statements, err := templateSQL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgresadapter tests: the template's SQL: %v\n", err)
		os.Exit(1)
	}
	postgres = postgrestest.New(&postgrestest.Options{
		Migrate:     postgrestest.Statements(statements...),
		Fingerprint: strings.Join(statements, "\n"),
	})
	goleak.VerifyTestMain(postgres.Main(m))
}

// templateSQL is every SQL text of the package, in the order an application's migrations would
// apply them.
func templateSQL() ([]string, error) {
	partsHistory, err := postgresadapter.HistorySQL("parts")
	if err != nil {
		return nil, err
	}
	ordersHistory, err := postgresadapter.HistorySQL("orders")
	if err != nil {
		return nil, err
	}
	return []string{
		fixtureSQL,
		postgresadapter.TriggerFunctionSQL,
		postgresadapter.TriggerSQL("parts"),
		postgresadapter.PartitionedTriggerSQL("gadgets", "tenant_id"),
		partsHistory,
		postgresadapter.TriggerSQL("orders"),
		ordersHistory,
		postgresadapter.FiltersSQL,
		postgresadapter.IdempotencySQL,
		postgresadapter.AuditSQL,
		postgresadapter.PresenceSQL,
	}, nil
}

// testDatabase is a database of one test: its name, its URL and a pool on it.
type testDatabase struct {
	name string
	url  string
	pool *pgxpool.Pool
}

func (d *testDatabase) dsn() string { return d.url }

// dsnWith is the database's URL with the query parameters added (key=value pairs joined by &).
func (d *testDatabase) dsnWith(parameters string) string {
	if parameters == "" {
		return d.url
	}
	if strings.Contains(d.url, "?") {
		return d.url + "&" + parameters
	}
	return d.url + "?" + parameters
}

// newDatabase is a copy of the template of the test's own, dropped after the test.
func newDatabase(t *testing.T) *testDatabase {
	t.Helper()
	address := postgres.Database(t)
	parsed, err := url.Parse(address)
	require.NoError(t, err)
	pool, err := pgxpool.New(t.Context(), address)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return &testDatabase{name: strings.TrimPrefix(parsed.Path, "/"), url: address, pool: pool}
}

// notifications listens on the change channel of database on a connection of its own.
func notifications(t *testing.T, database *testDatabase) *pgx.Conn {
	t.Helper()
	connection, err := pgx.Connect(t.Context(), database.dsn())
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close(context.Background()) }) // the database is dropped next; the close error carries nothing
	_, err = connection.Exec(t.Context(), "LISTEN "+postgresadapter.Channel)
	require.NoError(t, err)
	return connection
}

// nextPayload waits up to wait for the next notification's payload; "" when none came.
func nextPayload(t *testing.T, connection *pgx.Conn, wait time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	notification, err := connection.WaitForNotification(ctx)
	if err != nil {
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "waiting for a notification: %v", err)
		return ""
	}
	return notification.Payload
}
