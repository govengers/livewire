package postgresadapter_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire/postgresadapter"
)

// The fixture schema: parts is a live, versioned table with a native enum, nullable sort
// columns and a timestamptz that holds infinities; gadgets is partitioned by tenant.
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
`

// postgres is the one embedded server of this test binary (TST-002): every test gets a
// database of its own, cloned from the migrated template.
var postgres struct {
	port     int
	mutex    sync.Mutex // CREATE DATABASE ... TEMPLATE refuses a template that another clone reads
	sequence atomic.Int64
}

const templateDatabase = "livewire_template"

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fail("pick a port", err)
	}
	postgres.port = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return fail("free the port", err)
	}
	runtimePath, err := os.MkdirTemp("", "livewire-postgresadapter-")
	if err != nil {
		return fail("create the runtime directory", err)
	}
	defer func() { _ = os.RemoveAll(runtimePath) }() // a temporary directory; a leftover is harmless
	server := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(postgres.port)). //nolint:gosec // G115: a TCP port fits a uint32
		RuntimePath(runtimePath).
		Logger(io.Discard))
	if err := server.Start(); err != nil {
		return fail("start embedded postgres", err)
	}
	defer func() { _ = server.Stop() }() // the test result is decided; a failed stop changes nothing
	if err := migrateTemplate(); err != nil {
		return fail("migrate the template database", err)
	}
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			return fail("goroutines leaked", err)
		}
	}
	return code
}

func fail(what string, err error) int {
	fmt.Fprintf(os.Stderr, "postgresadapter tests: %s: %v\n", what, err)
	return 1
}

func dsn(database string) string {
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s", postgres.port, database)
}

// migrateTemplate applies every SQL text of the package, as an application's migrations
// would, to the template database.
func migrateTemplate() error {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn("postgres"))
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDatabase); err != nil {
		return err
	}
	connection, err := pgx.Connect(ctx, dsn(templateDatabase))
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close(ctx) }()
	for _, statements := range []string{
		fixtureSQL,
		postgresadapter.TriggerFunctionSQL,
		postgresadapter.TriggerSQL("parts"),
		postgresadapter.PartitionedTriggerSQL("gadgets", "tenant_id"),
		postgresadapter.HistorySQL("parts"),
		postgresadapter.FiltersSQL,
		postgresadapter.IdempotencySQL,
		postgresadapter.AuditSQL,
		postgresadapter.PresenceSQL,
	} {
		if _, err := connection.Exec(ctx, statements); err != nil {
			return fmt.Errorf("%w\n%s", err, statements)
		}
	}
	return nil
}

// testDatabase is a database of one test: its name and a pool on it.
type testDatabase struct {
	name string
	pool *pgxpool.Pool
}

func (d *testDatabase) dsn() string { return dsn(d.name) }

// newDatabase clones the template into a database of the test's own and drops it after.
func newDatabase(t *testing.T) *testDatabase {
	t.Helper()
	name := "test_" + strconv.FormatInt(postgres.sequence.Add(1), 10)
	admin, err := pgx.Connect(t.Context(), dsn("postgres"))
	require.NoError(t, err)
	postgres.mutex.Lock()
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+name+" TEMPLATE "+templateDatabase)
	postgres.mutex.Unlock()
	require.NoError(t, admin.Close(t.Context()))
	require.NoError(t, err)
	pool, err := pgxpool.New(t.Context(), dsn(name))
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, dsn("postgres"))
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
		require.NoError(t, admin.Close(ctx))
	})
	return &testDatabase{name: name, pool: pool}
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
