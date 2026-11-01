package postgresadapter_test

import (
	"context"
	"testing"

	"github.com/govengers/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
)

// sqlcDBTX is the DBTX interface sqlc generates for pgx/v5 with :copyfrom and :batch
// queries; the handle must satisfy it.
type sqlcDBTX interface {
	Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
	Query(context.Context, string, ...interface{}) (pgx.Rows, error)
	QueryRow(context.Context, string, ...interface{}) pgx.Row
	CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
}

// jobsExecutor is github.com/govengers/jobs/postgresadapter.Executor (D41).
type jobsExecutor interface {
	QueryRow(ctx context.Context, statement string, arguments ...any) pgx.Row
}

var (
	_ sqlcDBTX             = postgresadapter.DBTX(nil)
	_ jobsExecutor         = postgresadapter.DBTX(nil)
	_ postgresadapter.DBTX = (*pgxpool.Pool)(nil)
	_ postgresadapter.DBTX = pgx.Tx(nil)
	_ livewire.Transactor  = (*postgresadapter.Transactor)(nil)
)

func newTransactor(t *testing.T, pool *pgxpool.Pool) *postgresadapter.Transactor {
	t.Helper()
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
	require.NoError(t, err)
	return transactor
}

// transactionSettings is what a transaction sees of its settings and mode.
type transactionSettings struct {
	Tenant, Actor, Isolation, ReadOnly string
}

func settings(t *testing.T, database postgresadapter.DBTX) transactionSettings {
	t.Helper()
	var seen transactionSettings
	require.NoError(t, database.QueryRow(t.Context(), `SELECT coalesce(current_setting('livewire.tenant', true), ''), coalesce(current_setting('livewire.actor', true), ''),
		current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(&seen.Tenant, &seen.Actor, &seen.Isolation, &seen.ReadOnly))
	return seen
}

func TestTransactorSetsTheSettingsForTheTransactionOnly(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	single, err := pgxpool.New(t.Context(), database.dsn()+"?pool_max_conns=1") // every transaction on one session
	require.NoError(t, err)
	t.Cleanup(single.Close)
	transactor := newTransactor(t, single)
	identity := &auth.Identity{Provider: "github", Subject: "42", Impersonator: &auth.Identity{Provider: "password", Subject: "admin"}}

	write, err := transactor.Begin(t.Context(), livewire.TransactionDetails{
		Tenant: "north", Identity: identity, Mutation: "createPart", Parameters: map[string]int{"x": 1}, RequestID: "request-1",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = write.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	handle := write.Handle().(postgresadapter.DBTX)
	seen := settings(t, handle)
	require.JSONEq(t, `{"actor":"github:42","realActor":"password:admin","mutation":"createPart","requestId":"request-1"}`, seen.Actor)
	seen.Actor = ""
	require.Equal(t, transactionSettings{Tenant: "north", Isolation: "read committed", ReadOnly: "off"}, seen)
	require.NoError(t, write.Commit(t.Context()))

	seen = settings(t, single)
	require.Equal(t, []string{"", ""}, []string{seen.Tenant, seen.Actor}, "SET LOCAL ends with the transaction")

	read, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Tenant: "south", ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = read.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	handle = read.Handle().(postgresadapter.DBTX)
	require.Equal(t, transactionSettings{Tenant: "south", Isolation: "repeatable read", ReadOnly: "on"}, settings(t, handle))
	_, err = handle.Exec(t.Context(), "INSERT INTO parts (name) VALUES ('bolt')")
	var postgresError *pgconn.PgError
	require.ErrorAs(t, err, &postgresError)
	require.Equal(t, "25006", postgresError.Code, "a read transaction refuses writes")
	require.NoError(t, read.Rollback(t.Context()))
	require.Error(t, read.Commit(t.Context()), "a transaction ends once")
	require.Error(t, read.Rollback(t.Context()))
}

func TestTheHandleRunsEveryQueryMethodButCannotEndTheTransaction(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	transaction, err := newTransactor(t, database.pool).Begin(t.Context(), livewire.TransactionDetails{Mutation: "createPart"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = transaction.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	handle := transaction.Handle()
	_, commits := handle.(interface{ Commit(context.Context) error })
	require.False(t, commits)
	_, isTransaction := handle.(pgx.Tx)
	require.False(t, isTransaction)

	ctx := livewire.WithTransaction(t.Context(), handle)
	queries := postgresadapter.TransactionFrom(ctx)
	require.NotNil(t, queries)
	copied, err := queries.CopyFrom(ctx, pgx.Identifier{"parts"}, []string{"name"}, pgx.CopyFromRows([][]any{{"bolt"}, {"nut"}}))
	require.NoError(t, err)
	require.EqualValues(t, 2, copied)
	batch := &pgx.Batch{}
	batch.Queue("UPDATE parts SET rank = 1")
	require.NoError(t, queries.SendBatch(ctx, batch).Close())
	rows, err := queries.Query(ctx, "SELECT name FROM parts WHERE rank = 1 ORDER BY name")
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{"bolt", "nut"}, names)
	require.NoError(t, transaction.Rollback(t.Context()))

	var remaining int
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT count(*) FROM parts").Scan(&remaining))
	require.Zero(t, remaining)
	require.Nil(t, postgresadapter.TransactionFrom(t.Context()), "outside an engine unit of work there is none")
}

func TestTransactorFailures(t *testing.T) {
	t.Parallel()
	_, err := postgresadapter.NewTransactor(nil)
	require.Error(t, err)
	_, err = postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{})
	require.Error(t, err)

	database := newDatabase(t)
	transactor := newTransactor(t, database.pool)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = transactor.Begin(cancelled, livewire.TransactionDetails{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = transactor.Begin(t.Context(), livewire.TransactionDetails{Tenant: "bad\x00tenant"})
	require.Error(t, err, "a setting PostgreSQL refuses fails the begin")
	transaction, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Tenant: "north"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = transaction.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	require.NoError(t, transaction.Rollback(t.Context()))
	require.Zero(t, database.pool.Stat().AcquiredConns(), "the failed begins returned their connections")
}

func TestWritesAreReadCommittedWhateverTheDefault(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	repeatable, err := pgxpool.New(t.Context(), database.dsn()+"?default_transaction_isolation=repeatable%20read")
	require.NoError(t, err)
	t.Cleanup(repeatable.Close)
	write, err := newTransactor(t, repeatable).Begin(t.Context(), livewire.TransactionDetails{Mutation: "createPart"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = write.Rollback(context.Background()) }) // before repeatable.Close, or a failed assertion hangs it; ErrTxClosed after the rollback below
	require.Equal(t, "read committed", settings(t, write.Handle().(postgresadapter.DBTX)).Isolation,
		"a write sees the record a concurrent call committed (D16)")
	require.NoError(t, write.Rollback(t.Context()))
}
