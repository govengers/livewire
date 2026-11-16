package postgresadapter_test

import (
	_ "embed"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/postgresadapter"
)

// documentsSQL is a live table with a json and a large bytea column, partitioned by tenant.
var documentsSQL = `CREATE TABLE documents (id uuid PRIMARY KEY DEFAULT uuidv7(), tenant text NOT NULL, body json NOT NULL, content bytea NOT NULL);
` + postgresadapter.PartitionedTriggerSQL("documents", "tenant")

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

	var at string
	require.NoError(t, database.pool.QueryRow(t.Context(),
		"INSERT INTO readings VALUES (7, '2026-11-16 12:30:00.25+00', 1.5) RETURNING to_jsonb(readings)->>'at'").Scan(&at))
	require.JSONEq(t, `{"table":"readings","op":"insert","id":"7","partition":"`+at+`"}`, nextPayload(t, listening, time.Second))
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

func TestTriggerUpgradeSQLUpgradesTheEarlierTriggersInAMigration(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	_, err := database.pool.Exec(t.Context(), earlierTriggersSQL+documentsSQL)
	require.NoError(t, err)
	insert := func() error {
		_, err := database.pool.Exec(t.Context(), "INSERT INTO documents (tenant, body, content) VALUES ('north', $1, '')", refusedByJSONB)
		return err
	}
	require.ErrorContains(t, insert(), "unsupported Unicode escape sequence", "the earlier function converts the row")

	for range 2 { // idempotent
		require.NoError(t, pgx.BeginFunc(t.Context(), database.pool, func(transaction pgx.Tx) error { // as goose runs a migration
			_, err := transaction.Exec(t.Context(), postgresadapter.TriggerUpgradeSQL)
			return err
		}))
	}
	require.NoError(t, insert())
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
