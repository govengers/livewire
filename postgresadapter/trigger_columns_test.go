package postgresadapter_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/postgresadapter"
)

// documentsSQL is a live table with a json and a large bytea column, partitioned by tenant.
var documentsSQL = documentsTableSQL + postgresadapter.PartitionedTriggerSQL("documents", "tenant")

const documentsTableSQL = "CREATE TABLE documents (id uuid PRIMARY KEY DEFAULT uuidv7(), tenant text NOT NULL, body json NOT NULL, content bytea NOT NULL);\n"

// refusedByJSONB is a json value that jsonb refuses: it holds the escape of the NUL character.
const refusedByJSONB = `{"text": "\u0000"}`

func TestTriggerConvertsNoColumnButTheIDAndThePartition(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), documentsSQL)
	require.NoError(t, err)
	listening := notifications(t, database)

	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO documents (tenant, body, content) VALUES ('north', $1, $2) RETURNING id::text",
		refusedByJSONB, make([]byte, 4<<20)).Scan(&id), "a json value jsonb refuses fails no write")
	require.JSONEq(t, `{"table":"documents","op":"insert","id":"`+id+`","partition":"north"}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "UPDATE documents SET body = '[]', tenant = 'south' WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"documents","op":"update","id":"`+id+`","partition":"south"}`, nextPayload(t, listening, time.Second))
	require.JSONEq(t, `{"table":"documents","op":"update","id":"`+id+`","partition":"north"}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "UPDATE documents SET body = $2 WHERE id = $1", id, refusedByJSONB)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"documents","op":"update","id":"`+id+`","partition":"south"}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "DELETE FROM documents WHERE id = $1", id)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"documents","op":"delete","id":"`+id+`","partition":"south"}`, nextPayload(t, listening, time.Second))
}

func TestTriggerWritesTheIDAndThePartitionAsTheRowsJSONBDoes(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), `CREATE TABLE readings (id bigint PRIMARY KEY, at timestamptz NOT NULL, value float8);
CREATE TABLE settings (name text PRIMARY KEY, value json);
`+postgresadapter.PartitionedTriggerSQL("readings", "at")+postgresadapter.PartitionedTriggerSQL("settings", "missing"))
	require.NoError(t, err)
	listening := notifications(t, database)

	var readAt string
	require.NoError(t, database.pool.QueryRow(t.Context(),
		"INSERT INTO readings VALUES (7, '2026-11-16 12:30:00.25+00', 1.5) RETURNING to_jsonb(readings)->>'at'").Scan(&readAt))
	require.JSONEq(t, `{"table":"readings","op":"insert","id":"7","partition":"`+readAt+`"}`, nextPayload(t, listening, time.Second))
	_, err = database.pool.Exec(t.Context(), "INSERT INTO settings VALUES ('theme', $1)", refusedByJSONB)
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"settings","op":"insert","id":"","partition":""}`, nextPayload(t, listening, time.Second),
		"a table without an id column and a partition column it does not have send none")
}

func TestHistoryAppendsNothingForAnUpdateWhoseRowConvertsAsBefore(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	history, err := postgresadapter.HistorySQL("prices")
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "CREATE TABLE prices (id uuid PRIMARY KEY DEFAULT uuidv7(), amount numeric NOT NULL);\n"+history)
	require.NoError(t, err)

	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO prices (amount) VALUES (1.0) RETURNING id::text").Scan(&id))
	for _, update := range []string{
		"UPDATE prices SET amount = amount WHERE id = $1", // the same bytes
		"UPDATE prices SET amount = 1.00 WHERE id = $1",   // other bytes, the same jsonb
		"UPDATE prices SET amount = 2 WHERE id = $1",
	} {
		_, err := database.pool.Exec(t.Context(), update, id)
		require.NoError(t, err)
	}
	rows, err := database.pool.Query(t.Context(), "SELECT operation || ' ' || (data->>'amount') FROM prices_history WHERE id = $1 ORDER BY version", id)
	require.NoError(t, err)
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{"insert 1.0", "update 2"}, versions)
}

// earlierTriggersSQL is livewire v1.0.1's trigger SQL, the form TriggerUpgradeSQL upgrades.
//
//go:embed testdata/v1.0.1-triggers.sql
var earlierTriggersSQL string

// snippet:begin trigger-upgrade
// triggerUpgradeSQL is the application's migration of D88, in a goose migration of its own:
// the trigger functions and the presence trigger in this release's form.
const triggerUpgradeSQL = "SET LOCAL lock_timeout = '5s';\n" + postgresadapter.TriggerUpgradeSQL

// snippet:end trigger-upgrade

func TestTriggerUpgradeSQLUpgradesTheEarlierTriggersInAMigration(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), documentsTableSQL+earlierTriggersSQL)
	require.NoError(t, err)
	insert := func() (id string, err error) {
		err = database.pool.QueryRow(t.Context(), "INSERT INTO documents (tenant, body, content) VALUES ('north', $1, '') RETURNING id::text", refusedByJSONB).Scan(&id)
		return id, err
	}
	_, err = insert()
	require.ErrorContains(t, err, "unsupported Unicode escape sequence", "the earlier function converts the row")

	for range 2 { // idempotent
		require.NoError(t, pgx.BeginFunc(t.Context(), database.pool, func(transaction pgx.Tx) error { // as goose runs a migration
			_, err := transaction.Exec(t.Context(), triggerUpgradeSQL)
			return err
		}))
	}
	rows, err := database.pool.Query(t.Context(), `SELECT tgrelid::regclass::text || ' ' || tgname || ' ' || substring(pg_get_triggerdef(oid) FROM 'EXECUTE FUNCTION (.*)$')
		FROM pg_trigger WHERE tgname LIKE 'livewire_notify%' AND tgparentid = 0 AND tgtype & 1 = 1 ORDER BY 1`)
	require.NoError(t, err)
	triggers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{
		"documents livewire_notify livewire_notify_tenant('id', 'tenant')",
		"gadgets livewire_notify livewire_notify_tenant_id('id', 'tenant_id')",
		"livewire_presence livewire_notify livewire_notify_tenant('', 'tenant')",
		"livewire_presence livewire_notify_update livewire_notify_tenant('', 'tenant')",
		"orders livewire_notify livewire_notify('id', '')",
		"parts livewire_notify livewire_notify('id', '')",
	}, triggers, "every row trigger reads its table's columns by name")

	listening := notifications(t, database)
	id, err := insert()
	require.NoError(t, err)
	require.JSONEq(t, `{"table":"documents","op":"insert","id":"`+id+`","partition":"north"}`, nextPayload(t, listening, time.Second))
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO gadgets (tenant_id, name) VALUES ('south', 'lamp') RETURNING id::text").Scan(&id))
	require.JSONEq(t, `{"table":"gadgets","op":"insert","id":"`+id+`","partition":"south"}`, nextPayload(t, listening, time.Second))
	var definition string
	require.NoError(t, database.pool.QueryRow(t.Context(),
		"SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = 'livewire_presence'::regclass AND tgname = 'livewire_notify_update'").Scan(&definition))
	require.NotContains(t, definition, "to_jsonb", "the presence update trigger compares columns")
	require.Contains(t, definition, "old.state")
	history := readHistory(t, database, insertPart(t, database, "bolt"))
	require.Equal(t, []historyRow{{Version: 1, Operation: "insert", Name: "bolt", Open: true}}, history, "the history function still appends")

	_, err = database.pool.Exec(t.Context(), "DROP TABLE livewire_presence")
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), postgresadapter.TriggerUpgradeSQL)
	require.NoError(t, err, "an installation without presences upgrades too")
}

func TestTriggerUpgradeSQLLetsAWarmSessionWriteAfterTheIDsTypeChanged(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), "CREATE TABLE counters (id integer PRIMARY KEY);\n"+postgresadapter.TriggerSQL("counters"))
	require.NoError(t, err)
	warm, err := pgx.Connect(t.Context(), database.dsn())
	require.NoError(t, err)
	t.Cleanup(func() { _ = warm.Close(context.Background()) }) // the database is dropped next; the close error carries nothing
	insert := func(id int) error {
		_, err := warm.Exec(t.Context(), "INSERT INTO counters VALUES ($1)", id)
		return err
	}
	require.NoError(t, insert(1))

	_, err = database.pool.Exec(t.Context(), "ALTER TABLE counters ALTER COLUMN id TYPE bigint")
	require.NoError(t, err)
	require.ErrorContains(t, insert(2), "does not match that when preparing the plan", "the session's plan of NEW.id has the earlier type")
	_, err = database.pool.Exec(t.Context(), postgresadapter.TriggerUpgradeSQL)
	require.NoError(t, err)
	require.NoError(t, insert(3), "the replaced function is compiled anew")
}

func TestTriggerFetchesNoLargeColumnOfTheRow(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	// STORAGE EXTERNAL keeps the bytes uncompressed in the TOAST table, so reading them fetches its blocks.
	_, err := database.pool.Exec(t.Context(), `CREATE TABLE files (id uuid PRIMARY KEY DEFAULT uuidv7(), tenant text NOT NULL, n integer NOT NULL DEFAULT 0, content bytea STORAGE EXTERNAL NOT NULL);
CREATE TABLE blobs (id uuid PRIMARY KEY DEFAULT uuidv7(), n integer NOT NULL DEFAULT 0, content bytea STORAGE EXTERNAL NOT NULL);
`+postgresadapter.PartitionedTriggerSQL("files", "tenant")+postgresadapter.TriggerSQL("blobs"))
	require.NoError(t, err)
	for _, insert := range []string{"INSERT INTO files (tenant, content) VALUES ('north', $1)", "INSERT INTO blobs (content) VALUES ($1)"} {
		_, err = database.pool.Exec(t.Context(), insert, make([]byte, 1<<20))
		require.NoError(t, err)
	}

	for _, update := range []string{"UPDATE files SET n = n + 1", "UPDATE files SET tenant = 'south'", "UPDATE blobs SET n = n + 1"} {
		require.NoError(t, pgx.BeginFunc(t.Context(), database.pool, func(transaction pgx.Tx) error {
			// The blocks this transaction fetched from the table's TOAST table so far.
			fetched := func() (blocks int64) {
				table := strings.Fields(update)[1]
				require.NoError(t, transaction.QueryRow(t.Context(),
					"SELECT pg_stat_get_xact_blocks_fetched(reltoastrelid) FROM pg_class WHERE oid = $1::regclass", table).Scan(&blocks))
				return blocks
			}
			before := fetched()
			_, err := transaction.Exec(t.Context(), update)
			require.NoError(t, err)
			require.Equal(t, before, fetched(), "%s fetches none of the row's out-of-line bytes", update)
			return nil
		}))
	}
}
