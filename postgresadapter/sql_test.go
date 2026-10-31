package postgresadapter_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
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
			"CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON \"c\" FOR EACH ROW EXECUTE FUNCTION livewire_notify();\n",
		postgresadapter.TriggerSQL(`a"b`, "c"))
	require.Contains(t, postgresadapter.PartitionedTriggerSQL("gadgets", "it's"), "livewire_notify('it''s')")
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
	store, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool, Retention: 400 * 24 * time.Hour})
	require.NoError(t, err)
	require.NoError(t, store.Write(t.Context(), &postgresadapter.AuditEntry{Mutation: "createPart", RequestID: "now", Status: 201}))
	_, err = database.pool.Exec(t.Context(), `INSERT INTO livewire_audit (at, mutation, request_id, status, duration_ms)
		VALUES (now() - interval '3 years', 'createPart', 'ancient', 201, 1)`)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), `CREATE TABLE livewire_audit_200001 PARTITION OF livewire_audit FOR VALUES FROM ('2000-01-01Z') TO ('2000-02-01Z')`)
	require.NoError(t, err)

	require.NoError(t, store.Maintain(t.Context()))
	require.NoError(t, store.Maintain(t.Context()), "maintenance is idempotent")

	var partitions []string
	rows, err := database.pool.Query(t.Context(), `SELECT class.relname FROM pg_inherits JOIN pg_class class ON class.oid = pg_inherits.inhrelid
		WHERE pg_inherits.inhparent = 'livewire_audit'::regclass ORDER BY 1`)
	require.NoError(t, err)
	partitions, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	require.Equal(t, []string{"livewire_audit_" + thisMonth.Format("200601"), "livewire_audit_" + thisMonth.AddDate(0, 1, 0).Format("200601"), "livewire_audit_default"}, partitions)

	var requestID, partition string
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT request_id, tableoid::regclass::text FROM livewire_audit").Scan(&requestID, &partition))
	require.Equal(t, "now", requestID, "the expired row is gone")
	require.Equal(t, "livewire_audit_"+thisMonth.Format("200601"), partition, "the month's row moved out of the default partition")
}

func TestAuditWriteFollowsTheEngineTransaction(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: database.pool})
	require.NoError(t, err)
	identity := &auth.Identity{Provider: "password", Subject: "alice"}
	entry := &postgresadapter.AuditEntry{
		Tenant: "north", Identity: identity, Mutation: "createPart", Parameters: map[string]string{"id": "7"},
		RequestID: "request-1", IdempotencyKey: "0193a8f0-0000-7000-8000-000000000001", Status: 201, Duration: 1500 * time.Millisecond,
	}

	rolledBack, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Identity: identity, Mutation: "createPart"})
	require.NoError(t, err)
	require.NoError(t, store.Write(livewire.WithTransaction(t.Context(), rolledBack.Handle()), entry))
	require.NoError(t, rolledBack.Rollback(t.Context()))
	var rows int
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT count(*) FROM livewire_audit").Scan(&rows))
	require.Zero(t, rows, "the row rolls back with the mutation")

	committed, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Identity: identity, Mutation: "createPart"})
	require.NoError(t, err)
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

	err = store.Write(context.Background(), &postgresadapter.AuditEntry{Parameters: func() {}})
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
