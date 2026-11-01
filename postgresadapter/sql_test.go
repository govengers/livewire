package postgresadapter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

const quiet = 300 * time.Millisecond

func TestTriggerNotifiesEveryRowChange(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	listening := notifications(t, database)
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO parts (name) VALUES ('bolt') RETURNING id::text").Scan(&id))
	require.JSONEq(t, `{"table":"parts","op":"insert","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
	_, err := database.pool.Exec(t.Context(), "UPDATE parts SET name = 'nut' WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"parts","op":"update","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "DELETE FROM parts WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"parts","op":"delete","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
}

func TestTriggerNotifiesNothingOnRollback(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	listening := notifications(t, database)
	transaction, err := database.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = transaction.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	_, err = transaction.Exec(t.Context(), "INSERT INTO parts (name) VALUES ('bolt')")
	require.NoError(t, err)
	require.NoError(t, transaction.Rollback(t.Context()))
	require.Empty(t, nextPayload(t, listening, quiet))
}

func TestPartitionedTriggerCarriesThePartitionAndBothSidesOfAMove(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	listening := notifications(t, database)
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO gadgets (tenant_id, name) VALUES ('north', 'lamp') RETURNING id::text").Scan(&id))
	require.JSONEq(t, `{"table":"gadgets","op":"insert","id":"`+id+`","partition":"north"}`, nextPayload(t, listening, time.Second))
	_, err := database.pool.Exec(t.Context(), "UPDATE gadgets SET tenant_id = 'south' WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"gadgets","op":"update","id":"`+id+`","partition":"south"}`, nextPayload(t, listening, time.Second))
	require.JSONEq(t, `{"table":"gadgets","op":"update","id":"`+id+`","partition":"north"}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "UPDATE gadgets SET name = 'torch' WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"gadgets","op":"update","id":"`+id+`","partition":"south"}`, nextPayload(t, listening, time.Second))
	require.Empty(t, nextPayload(t, listening, quiet), "an update within one partition notifies once")
}

func TestTriggerSQLQuotesIdentifiersAndLiterals(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		"CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON \"a\"\"b\" FOR EACH ROW EXECUTE FUNCTION livewire_notify();\n"+
			"DO $livewire$ DECLARE relation regclass; BEGIN FOR relation IN SELECT '\"a\"\"b\"'::regclass UNION SELECT relid FROM pg_partition_tree('\"a\"\"b\"') LOOP\n"+
			"\tEXECUTE format('CREATE OR REPLACE TRIGGER livewire_notify_truncate AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION livewire_notify(%s)', relation, '');\n"+
			"END LOOP; END $livewire$;\n",
		postgresadapter.TriggerSQL(`a"b`))
	partitioned := postgresadapter.PartitionedTriggerSQL("gadgets", "it's")
	require.Contains(t, partitioned, "livewire_notify('it''s')")
	require.Contains(t, partitioned, "relation, quote_literal('it''s'))", "the truncate trigger carries the column too")
}

func TestTriggerNotifiesATruncateToEverySubscriptionOfTheTable(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	insertPart(t, database, "bolt")
	_, err := database.pool.Exec(t.Context(), "INSERT INTO gadgets (tenant_id, name) VALUES ('north', 'lamp')")
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "INSERT INTO orders (region, total) VALUES ('eu', 7)")
	require.NoError(t, err)
	listening := notifications(t, database)
	for _, table := range []string{"parts", "gadgets", "orders"} {
		_, err := database.pool.Exec(t.Context(), "TRUNCATE "+table+" CASCADE")
		require.NoError(t, err)
		require.JSONEq(t, `{"table":"`+table+`","op":"delete","id":"","partition":""}`, nextPayload(t, listening, time.Second))
	}
	_, err = database.pool.Exec(t.Context(), "TRUNCATE orders_eu")
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"orders","op":"delete","id":"","partition":""}`, nextPayload(t, listening, time.Second),
		"a partition truncated on its own names its table")
	require.Empty(t, nextPayload(t, listening, quiet), "one notification per truncated table")
}

func TestTriggersOfANativelyPartitionedTableNameTheTable(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	listening := notifications(t, database)
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO orders (region, total) VALUES ('eu', 7) RETURNING id::text").Scan(&id))
	require.JSONEq(t, `{"table":"orders","op":"insert","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second),
		"the table, not its partition orders_eu")
	_, err := database.pool.Exec(t.Context(), "UPDATE orders SET total = 8 WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"orders","op":"update","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))

	var versions []int
	rows, err := database.pool.Query(t.Context(), "SELECT version FROM orders_history WHERE id = $1 ORDER BY version", id)
	require.NoError(t, err)
	versions, err = pgx.CollectRows(rows, pgx.RowTo[int])
	require.NoError(t, err)
	require.Equal(t, []int{1, 2}, versions, "the versions of every partition go to the table's history")
}

func TestTriggerSendsAnOversizedPartitionAsNone(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	listening := notifications(t, database)
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO gadgets (tenant_id, name) VALUES (repeat(chr(1), 8000), 'lamp') RETURNING id::text").Scan(&id),
		"a value past pg_notify's limit does not fail the write")
	require.JSONEq(t, `{"table":"gadgets","op":"insert","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
	_, err := database.pool.Exec(t.Context(), "UPDATE gadgets SET tenant_id = repeat(chr(1), 500) WHERE id = $1", id)
	require.NoError(t, err)
	payload := nextPayload(t, listening, time.Second)
	require.Contains(t, payload, strings.Repeat(`\u0001`, 500), "500 bytes still travel, escaped to six times their size")
	require.JSONEq(t, `{"table":"gadgets","op":"update","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
}

func TestHistoryStampsOverlappingWritersInOrder(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	id := insertPart(t, database, "bolt")
	earlier, err := database.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = earlier.Rollback(context.Background()) }) // committed below; ErrTxClosed then
	_, err = earlier.Exec(t.Context(), "SELECT 1")                   // the transaction's now() is taken
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)
	_, err = database.pool.Exec(t.Context(), "UPDATE parts SET name = 'nut' WHERE id = $1", id)
	require.NoError(t, err)
	_, err = earlier.Exec(t.Context(), "UPDATE parts SET name = 'washer' WHERE id = $1", id)
	require.NoError(t, err)
	require.NoError(t, earlier.Commit(t.Context()))

	var ordered bool
	require.NoError(t, database.pool.QueryRow(t.Context(), `SELECT bool_and(valid_from > previous_from AND previous_to = valid_from)
		FROM (SELECT valid_from, lag(valid_from) OVER versions AS previous_from, lag(valid_to) OVER versions AS previous_to
			FROM parts_history WHERE id = $1 WINDOW versions AS (ORDER BY version)) AS stamped
		WHERE previous_from IS NOT NULL`, id).Scan(&ordered))
	require.True(t, ordered, "each version starts after the one before and where it ended")
}

// historyRow is one version of parts_history as the test reads it.
type historyRow struct {
	Version   int
	Operation string
	Name      string
	Open      bool
	Actor     *string
	RealActor *string
	Mutation  *string
	RequestID *string
}

func readHistory(t *testing.T, database *testDatabase, id string) []historyRow {
	t.Helper()
	rows, err := database.pool.Query(t.Context(), `SELECT version, operation, data->>'name', valid_to IS NULL, actor, real_actor, mutation, request_id
		FROM parts_history WHERE id = $1 ORDER BY version`, id)
	require.NoError(t, err)
	history, err := pgx.CollectRows(rows, pgx.RowToStructByPos[historyRow])
	require.NoError(t, err)
	return history
}

func TestHistoryAppendsVersionsWithTheActorOfTheTransaction(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: database.pool})
	require.NoError(t, err)
	admin := &auth.Identity{Provider: "password", Subject: "admin"}
	impersonated := &auth.Identity{Provider: "password", Subject: "alice", Impersonator: admin}

	transaction, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Identity: impersonated, Mutation: "createPart", RequestID: "request-1"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = transaction.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	handle := transaction.Handle().(postgresadapter.DBTX)
	var id string
	require.NoError(t, handle.QueryRow(t.Context(), "INSERT INTO parts (name) VALUES ('bolt') RETURNING id::text").Scan(&id))
	require.NoError(t, transaction.Commit(t.Context()))

	_, err = database.pool.Exec(t.Context(), "UPDATE parts SET name = 'nut' WHERE id = $1", id)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "UPDATE parts SET name = 'nut' WHERE id = $1", id)
	require.NoError(t, err) // changes nothing, appends nothing
	_, err = database.pool.Exec(t.Context(), "DELETE FROM parts WHERE id = $1", id)
	require.NoError(t, err)

	history := readHistory(t, database, id)
	require.Len(t, history, 3)
	require.Equal(t, historyRow{
		Version: 1, Operation: "insert", Name: "bolt", Actor: new("password:alice"), RealActor: new("password:admin"),
		Mutation: new("createPart"), RequestID: new("request-1"),
	}, history[0])
	require.Equal(t, historyRow{Version: 2, Operation: "update", Name: "nut"}, history[1])
	require.Equal(t, historyRow{Version: 3, Operation: "delete", Name: "nut"}, history[2])
	require.False(t, history[2].Open, "a delete is a closed version")

	_, err = database.pool.Exec(t.Context(), "INSERT INTO parts (id, name) VALUES ($1, 'washer')", id)
	require.NoError(t, err)
	history = readHistory(t, database, id)
	require.Equal(t, historyRow{Version: 4, Operation: "insert", Name: "washer", Open: true}, history[3])
}

func TestAuditMaintainPartitionsByMonthAndDropsExpiredRows(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), `INSERT INTO livewire_audit (at, mutation, request_id, status, duration_ms) VALUES
		('2026-03-10T08:00:00Z', 'createPart', 'march', 201, 1), ('2023-03-10T08:00:00Z', 'createPart', 'ancient', 201, 1)`)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), `CREATE TABLE livewire_audit_200001 PARTITION OF livewire_audit FOR VALUES FROM ('2000-01-01Z') TO ('2000-02-01Z')`)
	require.NoError(t, err)

	for range 2 { // maintenance is idempotent
		_, err = database.pool.Exec(t.Context(), "SELECT livewire_audit_maintain(interval '400 days', '2026-03-15T12:00:00Z')")
		require.NoError(t, err)
	}
	require.Equal(t, map[string]string{
		"livewire_audit_202603": "202603", "livewire_audit_202604": "202604", "livewire_audit_default": "DEFAULT",
	}, auditPartitions(t, database.pool))
	var requestID, partition string
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT request_id, tableoid::regclass::text FROM livewire_audit").Scan(&requestID, &partition))
	require.Equal(t, "march", requestID, "the expired row is gone")
	require.Equal(t, "livewire_audit_202603", partition, "the month's row moved out of the default partition")

	store, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool, Retention: 400 * 24 * time.Hour})
	require.NoError(t, err)
	require.NoError(t, store.Maintain(t.Context()), "the store runs it at now()")
}

// auditPartitions is every partition of livewire_audit with the UTC month its bounds span,
// "YYYYMM" when they are the first of a month and of the next, else the bound expression.
func auditPartitions(t *testing.T, database postgresadapter.DBTX) map[string]string {
	t.Helper()
	rows, err := database.Query(t.Context(), `SELECT class.relname, CASE WHEN class.relpartbound IS NULL THEN '' ELSE pg_get_expr(class.relpartbound, class.oid) END,
			coalesce(to_char(bounds.lower AT TIME ZONE 'UTC', 'YYYYMM'), ''),
			coalesce(bounds.lower AT TIME ZONE 'UTC' = date_trunc('month', bounds.lower AT TIME ZONE 'UTC')
				AND bounds.upper AT TIME ZONE 'UTC' = date_trunc('month', bounds.lower AT TIME ZONE 'UTC') + interval '1 month', false)
		FROM pg_inherits JOIN pg_class class ON class.oid = pg_inherits.inhrelid
		LEFT JOIN LATERAL (SELECT (regexp_match(pg_get_expr(class.relpartbound, class.oid), 'FROM \(''([^'']+)''\) TO \(''([^'']+)''\)'))[1]::timestamptz AS lower,
			(regexp_match(pg_get_expr(class.relpartbound, class.oid), 'FROM \(''([^'']+)''\) TO \(''([^'']+)''\)'))[2]::timestamptz AS upper) AS bounds ON true
		WHERE pg_inherits.inhparent = 'livewire_audit'::regclass`)
	require.NoError(t, err)
	partitions := map[string]string{}
	for rows.Next() {
		var name, expression, month string
		var wholeMonth bool
		require.NoError(t, rows.Scan(&name, &expression, &month, &wholeMonth))
		switch {
		case expression == "DEFAULT":
			partitions[name] = expression
		case wholeMonth:
			partitions[name] = month
		default:
			partitions[name] = expression
		}
	}
	require.NoError(t, rows.Err())
	return partitions
}

func TestAuditMaintainCutsUTCMonthsWhateverTheSessionTimeZone(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	for _, timeZone := range []string{"Europe/Berlin", "America/New_York", "Pacific/Kiritimati"} {
		t.Run(timeZone, func(t *testing.T) {
			connection, err := pgx.Connect(t.Context(), database.dsn()+"?timezone="+timeZone)
			require.NoError(t, err)
			t.Cleanup(func() { _ = connection.Close(context.Background()) }) // the database is dropped next; the close error carries nothing
			_, err = connection.Exec(t.Context(), "DROP TABLE IF EXISTS livewire_audit_202603, livewire_audit_202604, livewire_audit_202612, livewire_audit_202701")
			require.NoError(t, err)
			// Around the March DST switch in Europe and America, and across the year's end.
			for _, asOf := range []string{"2026-03-31T23:30:00Z", "2026-12-31T23:30:00Z"} {
				_, err = connection.Exec(t.Context(), "SELECT livewire_audit_maintain(interval '100 years', $1::timestamptz)", asOf)
				require.NoError(t, err)
			}
			require.Equal(t, map[string]string{
				"livewire_audit_202603": "202603", "livewire_audit_202604": "202604",
				"livewire_audit_202612": "202612", "livewire_audit_202701": "202701", "livewire_audit_default": "DEFAULT",
			}, auditPartitions(t, connection))
		})
	}
}

func TestAuditMaintainHoldsUnderConcurrentWrites(t *testing.T) {
	t.Parallel()
	t.Run("the function at a fixed month", func(t *testing.T) {
		t.Parallel()
		database := newDatabase(t)
		maintain := func() error {
			_, err := database.pool.Exec(t.Context(), "SELECT livewire_audit_maintain(interval '400 days', '2026-03-15T12:00:00Z')")
			return err
		}
		writeConcurrently(t, database, `INSERT INTO livewire_audit (at, mutation, request_id, status, duration_ms)
			VALUES ('2026-03-15T12:00:00Z', 'createPart', 'concurrent', 201, 1)`, maintain)
		var stranded int
		require.NoError(t, database.pool.QueryRow(t.Context(), `SELECT count(*) FROM livewire_audit_default`).Scan(&stranded))
		require.Zero(t, stranded, "no row of the month stays in the default partition")
	})
	t.Run("the store on a repeatable read default", func(t *testing.T) {
		t.Parallel()
		database := newDatabase(t)
		repeatable, err := pgxpool.New(t.Context(), database.dsn()+"?default_transaction_isolation=repeatable%20read")
		require.NoError(t, err)
		t.Cleanup(repeatable.Close)
		store, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: repeatable})
		require.NoError(t, err)
		writeConcurrently(t, database, `INSERT INTO livewire_audit (mutation, request_id, status, duration_ms) VALUES ('createPart', 'concurrent', 201, 1)`,
			func() error { return store.Maintain(t.Context()) })
	})
}

func TestAuditWriteFollowsTheEngineTransaction(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: database.pool})
	require.NoError(t, err)
	identity := &auth.Identity{Provider: "password", Subject: "alice"}
	entry := &livewire.AuditEntry{
		Tenant: "north", Identity: identity, Mutation: "createPart", Parameters: map[string]string{"id": "7"},
		RequestID: "request-1", IdempotencyKey: "0193a8f0-0000-7000-8000-000000000001", Status: 201, Duration: 1500 * time.Millisecond,
	}

	rolledBack, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Identity: identity, Mutation: "createPart"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rolledBack.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	require.NoError(t, store.Write(livewire.WithTransaction(t.Context(), rolledBack.Handle()), entry))
	require.NoError(t, rolledBack.Rollback(t.Context()))
	var rows int
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT count(*) FROM livewire_audit").Scan(&rows))
	require.Zero(t, rows, "the row rolls back with the mutation")

	committed, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Identity: identity, Mutation: "createPart"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = committed.Rollback(context.Background()) }) // a failed assertion must not leave it holding a connection; ErrTxClosed once ended
	require.NoError(t, store.Write(livewire.WithTransaction(t.Context(), committed.Handle()), entry))
	require.NoError(t, committed.Commit(t.Context()))
	var tenant, actor, realActor, mutation, requestID, key string
	var parameters json.RawMessage
	var status, duration int
	require.NoError(t, database.pool.QueryRow(t.Context(), `SELECT tenant, actor, real_actor, mutation, request_id, idempotency_key::text, parameters, status, duration_ms
		FROM livewire_audit`).Scan(&tenant, &actor, &realActor, &mutation, &requestID, &key, &parameters, &status, &duration))
	require.Equal(t, []any{"north", "password:alice", "password:alice", "createPart", "request-1", entry.IdempotencyKey, 201, 1500},
		[]any{tenant, actor, realActor, mutation, requestID, key, status, duration})
	require.JSONEq(t, `{"id":"7"}`, string(parameters))

	err = store.Write(context.Background(), &livewire.AuditEntry{Parameters: func() {}})
	require.Error(t, err, "parameters that are no JSON are refused")
}

func TestAuditStoreOptions(t *testing.T) {
	t.Parallel()
	_, err := postgresadapter.NewAuditStore(nil)
	require.Error(t, err)
	database := newDatabase(t)
	_, err = postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool, Retention: -time.Second})
	require.Error(t, err)
}

func TestAMoveBetweenNativePartitionsIsADeleteAndAnInsert(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO orders (region, total) VALUES ('eu', 7) RETURNING id::text").Scan(&id))
	listening := notifications(t, database)
	_, err := database.pool.Exec(t.Context(), "UPDATE orders SET region = 'us' WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"orders","op":"delete","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
	require.JSONEq(t, `{"table":"orders","op":"insert","id":"`+id+`","partition":""}`, nextPayload(t, listening, time.Second))
	rows, err := database.pool.Query(t.Context(), "SELECT operation || ' ' || (data->>'region') FROM orders_history WHERE id = $1 ORDER BY version", id)
	require.NoError(t, err)
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{"insert eu", "delete eu", "insert us"}, versions, "as HistorySQL documents it")

	moved := historyPage(t, database, "orders", id, "")
	require.Equal(t, 3, moved.Total, "a partition move stays in its life: the row keeps its whole history")
	require.Equal(t, []livewire.ChangeOperation{livewire.ChangeInsert, livewire.ChangeDelete, livewire.ChangeInsert},
		[]livewire.ChangeOperation{moved.Items[0].Operation, moved.Items[1].Operation, moved.Items[2].Operation})
	deletes := historyPage(t, database, "orders", id, `{"where":{"field":"operation","op":"eq","value":"delete"}}`)
	require.Equal(t, 1, deletes.Total, "the delete of a move is a version the filter reaches")
}

func TestHistoryClosesTheOldIDOnAnIDChange(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	id := insertPart(t, database, "bolt")
	var changed string
	require.NoError(t, database.pool.QueryRow(t.Context(), "UPDATE parts SET id = uuidv7() WHERE id = $1 RETURNING id::text", id).Scan(&changed))
	require.Equal(t, []historyRow{{Version: 1, Operation: "insert", Name: "bolt"}}, readHistory(t, database, id), "the old id's version is closed")
	require.Equal(t, []historyRow{{Version: 1, Operation: "update", Name: "bolt", Open: true}}, readHistory(t, database, changed))
}

func TestHistoryNeverStampsBeforeThePreviousVersion(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	id := insertPart(t, database, "bolt")
	_, err := database.pool.Exec(t.Context(), "UPDATE parts_history SET valid_from = now() + interval '1 hour' WHERE id = $1", id) // a clock that stepped back
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "UPDATE parts SET name = 'nut' WHERE id = $1", id)
	require.NoError(t, err)
	var ordered bool
	require.NoError(t, database.pool.QueryRow(t.Context(), `SELECT previous.valid_to = next.valid_from AND next.valid_from >= previous.valid_from
		FROM parts_history previous JOIN parts_history next ON next.id = previous.id AND next.version = previous.version + 1
		WHERE previous.id = $1`, id).Scan(&ordered))
	require.True(t, ordered, "the next version starts no earlier than the previous one, where it ended")
}

func TestHistorySQLRefusesANameItCannotSuffix(t *testing.T) {
	t.Parallel()
	_, err := postgresadapter.HistorySQL(strings.Repeat("t", 55))
	require.NoError(t, err, "55 bytes and _history are 63")
	_, err = postgresadapter.HistorySQL(strings.Repeat("t", 56))
	require.ErrorContains(t, err, "63 bytes")
	_, err = postgresadapter.HistorySQL(strings.Repeat("t", 63))
	require.Error(t, err, "a legal table name whose history PostgreSQL would cut back to the table's name")
}

func TestAuditMaintainDropsNothingCurrentForAFutureAsOf(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), `CREATE TABLE livewire_audit_209912 PARTITION OF livewire_audit FOR VALUES FROM ('2099-12-01Z') TO ('2100-01-01Z')`)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), `INSERT INTO livewire_audit (at, mutation, request_id, status, duration_ms) VALUES (now(), 'createPart', 'today', 201, 1)`)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "SELECT livewire_audit_maintain(interval '1 day', '2100-06-01T00:00:00Z')")
	require.NoError(t, err)
	var kept bool
	var rows int
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT to_regclass('livewire_audit_209912') IS NOT NULL, (SELECT count(*) FROM livewire_audit)").Scan(&kept, &rows))
	require.True(t, kept, "the cutoff is least(as_of, now()) - retention")
	require.Equal(t, 1, rows, "today's row is not older than the cutoff")
}

func TestAuditMaintainWaitsForAWriterAndAReaderWithoutDeadlock(t *testing.T) {
	t.Parallel()
	for name, shape := range map[string]struct{ first, second string }{
		// The writer holds the table, not yet the default partition, and inserts after Maintain began.
		"writer": {
			"LOCK TABLE ONLY livewire_audit IN ROW EXCLUSIVE MODE",
			"INSERT INTO livewire_audit (at, mutation, request_id, status, duration_ms) VALUES ('2026-03-15T12:00:00Z', 'createPart', 'held', 201, 1)",
		},
		// The reader holds the table and reads every partition after Maintain began.
		"reader": {"SELECT count(*) FROM ONLY livewire_audit", "SELECT count(*) FROM livewire_audit"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			database := newDatabase(t)
			_, err := database.pool.Exec(t.Context(), `CREATE TABLE livewire_audit_200001 PARTITION OF livewire_audit FOR VALUES FROM ('2000-01-01Z') TO ('2000-02-01Z')`)
			require.NoError(t, err)
			holder, err := database.pool.Begin(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = holder.Rollback(context.Background()) }) // committed below; ErrTxClosed then
			_, err = holder.Exec(t.Context(), shape.first)
			require.NoError(t, err)

			maintained := make(chan error, 1)
			go func() { // creates this and next month and drops 200001
				_, err := database.pool.Exec(t.Context(), "SELECT livewire_audit_maintain(interval '400 days', '2026-03-15T12:00:00Z')")
				maintained <- err
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := database.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT FROM pg_locks WHERE NOT granted AND relation = 'livewire_audit'::regclass AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))").Scan(&waiting)
				return err == nil && waiting
			}, 5*time.Second, time.Millisecond, "Maintain waits for the table")
			_, err = holder.Exec(t.Context(), shape.second)
			require.NoError(t, err, "the holder goes on")
			require.NoError(t, holder.Commit(t.Context()))
			select {
			case err := <-maintained:
				require.NoError(t, err, "Maintain went on after the holder, no deadlock")
			case <-time.After(10 * time.Second):
				t.Fatal("Maintain did not end")
			}
		})
	}
}

func TestAuditWriteStoresTheIdempotencyKeyCanonically(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	const key = "0193a8f0-0000-7000-8000-00000000000b"
	for _, spelling := range []string{"urn:uuid:" + key, "{" + strings.ToUpper(key) + "}", ""} {
		require.NoError(t, store.Write(t.Context(), &livewire.AuditEntry{Mutation: "createPart", RequestID: spelling, IdempotencyKey: spelling, Status: 201}), spelling)
	}
	rows, err := database.pool.Query(t.Context(), "SELECT coalesce(idempotency_key::text, 'none') FROM livewire_audit ORDER BY request_id")
	require.NoError(t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{"none", key, key}, keys)
	require.Error(t, store.Write(t.Context(), &livewire.AuditEntry{Mutation: "createPart", IdempotencyKey: "nope"}))
}

func TestTriggerSQLKeepsNamesThatHoldItsDollarQuote(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), `CREATE TABLE "x$livewire$y" (id uuid PRIMARY KEY DEFAULT uuidv7());
		CREATE TABLE plain (id uuid PRIMARY KEY DEFAULT uuidv7(), "c$livewire$" text NOT NULL)`)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), postgresadapter.TriggerSQL("x$livewire$y"))
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), postgresadapter.PartitionedTriggerSQL("plain", "c$livewire$"))
	require.NoError(t, err)
	listening := notifications(t, database)
	_, err = database.pool.Exec(t.Context(), `TRUNCATE "x$livewire$y"`)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"x$livewire$y","op":"delete","id":"","partition":""}`, nextPayload(t, listening, time.Second))
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), `INSERT INTO plain ("c$livewire$") VALUES ('north') RETURNING id::text`).Scan(&id))
	require.JSONEq(t, `{"table":"plain","op":"insert","id":"`+id+`","partition":"north"}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "TRUNCATE plain")
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"plain","op":"delete","id":"","partition":""}`, nextPayload(t, listening, time.Second))
}

func writeConcurrently(t *testing.T, database *testDatabase, statement string, maintain func() error) {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	var writers sync.WaitGroup
	var written atomic.Int64
	var mutex sync.Mutex
	var failures []error
	for range 4 {
		writers.Go(func() {
			for ctx.Err() == nil {
				_, err := database.pool.Exec(ctx, statement)
				switch {
				case err == nil:
					written.Add(1)
				case ctx.Err() == nil:
					mutex.Lock()
					failures = append(failures, err)
					mutex.Unlock()
				}
			}
		})
	}
	defer func() {
		stop()
		writers.Wait()
	}()
	require.Eventually(t, func() bool { return written.Load() > 50 }, 5*time.Second, time.Millisecond, "the writers write into the default partition")
	maintained := maintain()
	before := written.Load()
	require.Eventually(t, func() bool { return written.Load() > before+50 }, 5*time.Second, time.Millisecond, "the writers go on into the new partition")
	stop()
	writers.Wait()
	require.NoError(t, maintained, "the move and the attach hold against concurrent inserts")
	require.Empty(t, failures, "no writer failed, the ones queued behind the attach included")
}

// historyPage reads id's history of table through HistoryPage, filtered by filter ("" for
// none), oldest first.
func historyPage(t *testing.T, database *testDatabase, table, id, filter string) *livewire.Page[livewire.Version[map[string]any]] {
	t.Helper()
	page, err := readHistoryPage(t.Context(), database, table, id, filter)
	require.NoError(t, err)
	return page
}

func readHistoryPage(ctx context.Context, database *testDatabase, table, id, filter string) (*livewire.Page[livewire.Version[map[string]any]], error) {
	var document *query.Document
	if filter != "" {
		var err error
		if document, err = query.Parse(livewire.HistoryFields(), []byte(filter)); err != nil {
			return nil, err
		}
	}
	scope := func(builder *query.Builder) { builder.WriteString("id = ").Argument(id) }
	listQuery, err := query.Compile(livewire.HistoryFields(), document, []query.SortTerm{{Field: "id"}}, query.Page{Limit: 10}, scope)
	if err != nil {
		return nil, err
	}
	return postgresadapter.HistoryPage(ctx, database.pool, table, listQuery, func(row []byte) (*map[string]any, error) {
		var data map[string]any
		return &data, json.Unmarshal(row, &data)
	})
}

// names are the names the versions of a page left.
func names(page *livewire.Page[livewire.Version[map[string]any]]) []string {
	result := []string{}
	for _, version := range page.Items {
		result = append(result, string(version.Operation)+":"+fmt.Sprint((*version.Data)["name"]))
	}
	return result
}

func TestHistoryLifeStartsWheneverARowArrivesUnderAnID(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	exec := func(statement string, arguments ...any) {
		t.Helper()
		_, err := database.pool.Exec(t.Context(), statement, arguments...)
		require.NoError(t, err)
	}
	truncated := uuid.NewString()
	exec("INSERT INTO parts (id, name) VALUES ($1, 'alices-secret')", truncated)
	exec("UPDATE parts SET name = 'alices-other' WHERE id = $1", truncated)
	exec("TRUNCATE parts")
	exec("INSERT INTO parts (id, name) VALUES ($1, 'mallorys')", truncated)
	require.Equal(t, []string{"insert:mallorys"}, names(historyPage(t, database, "parts", truncated, "")), "a re-create after a TRUNCATE")

	dead, moving := uuid.NewString(), uuid.NewString()
	exec("INSERT INTO parts (id, name) VALUES ($1, 'alices-secret')", dead)
	exec("DELETE FROM parts WHERE id = $1", dead)
	exec("INSERT INTO parts (id, name) VALUES ($1, 'mallorys')", moving)
	exec("UPDATE parts SET id = $1 WHERE id = $2", dead, moving)
	require.Equal(t, []string{"update:mallorys"}, names(historyPage(t, database, "parts", dead, "")), "an id change onto a deleted id")

	away, target := uuid.NewString(), uuid.NewString()
	exec("INSERT INTO parts (id, name) VALUES ($1, 'alices-secret')", away)
	exec("UPDATE parts SET id = $1 WHERE id = $2", target, away)
	exec("INSERT INTO parts (id, name) VALUES ($1, 'mallorys')", away)
	require.Equal(t, []string{"insert:mallorys"}, names(historyPage(t, database, "parts", away, "")), "an id moved away and re-created")
	require.Equal(t, []string{"update:alices-secret"}, names(historyPage(t, database, "parts", target, "")), "the new id's history starts at the change")
}

func TestHistoryPageReadsALongHistoryInLinearTime(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	id := uuid.NewString()
	_, err := database.pool.Exec(t.Context(), `INSERT INTO parts_history (id, version, operation, data, valid_from, life)
		SELECT $1, version, 'update', '{"name":"bolt"}', now(), 1 FROM generate_series(1, 5000) AS version`, id)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	page, err := readHistoryPage(ctx, database, "parts", id, "")
	require.NoError(t, err, "5000 versions read well within the deadline; a scope re-run per version takes seconds")
	require.Equal(t, 5000, page.Total)
}
