package postgresadapter

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The SQL of this file is for the application's goose migrations (D22): nothing in this
// package runs DDL. Every text is a list of statements that one Exec without arguments runs;
// apply TriggerFunctionSQL before TriggerSQL, PartitionedTriggerSQL and PresenceSQL. The
// tables use uuidv7() and so need PostgreSQL 18. A migration that adds a trigger to an
// existing table sets its own lock_timeout (DAT-006).

// Channel is the NOTIFY channel the triggers send on and the Listener listens on.
const Channel = "livewire"

// TriggerFunctionSQL creates livewire_notify(), the trigger function of TriggerSQL,
// PartitionedTriggerSQL and PresenceSQL. It sends one notification per row change on
// Channel: `{"table", "op", "id", "partition"}` with the operation in lower case (insert,
// update, delete), the row's id column as text ("" without one) and the partition column's
// value ("" without one, D25). An update that changes the partition column notifies the new
// and the old value. The table is the root of a natively partitioned table (PARTITION BY),
// never the partition the row lives in, and its id must be unique across the partitions; an
// update that moves a row to another native partition runs as a delete and an insert and
// notifies those two. A TRUNCATE notifies one delete without id and partition, which reaches
// every subscription of the table. An id or partition value longer than 500 bytes is sent as
// "", so the payload stays below pg_notify's 8000 bytes: such a change reaches every
// subscription of the table, never fewer than it should.
const TriggerFunctionSQL = `CREATE OR REPLACE FUNCTION livewire_notify() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	old_row jsonb := CASE WHEN TG_OP IN ('UPDATE', 'DELETE') THEN to_jsonb(OLD) END;
	new_row jsonb := CASE WHEN TG_OP IN ('INSERT', 'UPDATE') THEN to_jsonb(NEW) END;
	partition_column text := CASE WHEN TG_NARGS > 0 THEN TG_ARGV[0] END;
	table_name text := (SELECT relname FROM pg_class WHERE oid = coalesce(pg_partition_root(TG_RELID), TG_RELID));
	operation text := CASE WHEN TG_OP = 'TRUNCATE' THEN 'delete' ELSE lower(TG_OP) END;
	row_id text := coalesce(coalesce(new_row, old_row)->>'id', '');
	old_id text := coalesce(old_row->>'id', '');
	row_partition text := coalesce(coalesce(new_row, old_row)->>partition_column, '');
	old_partition text := coalesce(old_row->>partition_column, '');
BEGIN
	PERFORM pg_notify('` + Channel + `', json_build_object('table', table_name, 'op', operation,
		'id', CASE WHEN octet_length(row_id) <= 500 THEN row_id ELSE '' END,
		'partition', CASE WHEN octet_length(row_partition) <= 500 THEN row_partition ELSE '' END)::text);
	IF TG_OP = 'UPDATE' AND old_partition <> row_partition THEN
		PERFORM pg_notify('` + Channel + `', json_build_object('table', table_name, 'op', 'update',
			'id', CASE WHEN octet_length(old_id) <= 500 THEN old_id ELSE '' END,
			'partition', CASE WHEN octet_length(old_partition) <= 500 THEN old_partition ELSE '' END)::text);
	END IF;
	RETURN NULL;
END
$$;
`

// TriggerSQL makes every table notify on each row change and on TRUNCATE (D22); the changes
// carry no partition, so they reach the subscriptions of every partition. A table name is one
// identifier in the search path, quoted as it is. On a natively partitioned table the row
// trigger reaches every partition and the changes name the table itself; the TRUNCATE
// trigger is installed on the table and on the partitions it has now, so a partition
// attached later notifies its own TRUNCATE only once TriggerSQL runs again, and a DETACH
// PARTITION notifies nothing.
func TriggerSQL(tables ...string) string {
	var statements strings.Builder
	for _, table := range tables {
		statements.WriteString(notifyTriggersSQL(table, ""))
	}
	return statements.String()
}

// PartitionedTriggerSQL is TriggerSQL for one table whose changes carry the value of column
// as their partition (D25), so a change re-runs only the subscriptions of that partition and
// those without one.
func PartitionedTriggerSQL(table, column string) string {
	return notifyTriggersSQL(table, literal(column))
}

// FiltersSQL creates livewire_filters, the stored filters of D13 (FilterStore). An unnamed
// filter is unique per owner, tenant, resource, context and document hash; a name is unique
// per owner, tenant and resource.
const FiltersSQL = `CREATE TABLE livewire_filters (
	id uuid PRIMARY KEY DEFAULT uuidv7(),
	owner text NOT NULL CHECK (owner <> ''),
	tenant text NOT NULL DEFAULT '',
	resource text NOT NULL CHECK (resource <> ''),
	context text NOT NULL DEFAULT '' CHECK (char_length(context) <= 200),
	name text CHECK (name <> '' AND char_length(name) <= 200),
	document jsonb NOT NULL,
	document_hash text NOT NULL CHECK (document_hash <> ''),
	created_at timestamptz NOT NULL DEFAULT now(),
	last_used_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX livewire_filters_unnamed ON livewire_filters (owner, tenant, resource, context, document_hash) WHERE name IS NULL;
CREATE UNIQUE INDEX livewire_filters_named ON livewire_filters (owner, tenant, resource, name) WHERE name IS NOT NULL;
CREATE INDEX livewire_filters_unused ON livewire_filters (last_used_at) WHERE name IS NULL;
`

// IdempotencySQL creates livewire_idempotency, the recorded answers of D16
// (IdempotencyStore). Status is 0 while a call is in flight; response is the answered body
// as bytes (D72), so a replay answers the very bytes of the first answer. A table the earlier
// jsonb column created is upgraded after every replica runs this store, which reads and writes
// both, by an ALTER in the application's migration that takes an ACCESS EXCLUSIVE lock (D72).
const IdempotencySQL = `CREATE TABLE livewire_idempotency (
	owner text NOT NULL CHECK (owner <> ''),
	tenant text NOT NULL DEFAULT '',
	key uuid NOT NULL,
	mutation text NOT NULL CHECK (mutation <> ''),
	status integer NOT NULL DEFAULT 0,
	response bytea,
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (owner, tenant, key)
);
CREATE INDEX livewire_idempotency_created_at ON livewire_idempotency (created_at);
`

// AuditSQL creates livewire_audit, one row per mutation (D32, AuditStore), partitioned by
// UTC month on at. Rows of a month without its partition land in livewire_audit_default;
// livewire_audit_maintain(retention, as_of default now()), which AuditStore.Maintain calls,
// creates the partitions of the UTC month of as_of and the next one, moving their rows out of
// the default partition, drops the partitions whose month ended before least(as_of, now()) -
// retention and deletes the default partition's rows older than that. When it creates or
// drops a partition it holds livewire_audit in ACCESS EXCLUSIVE mode until it ends, so audit
// writes and reads wait meanwhile; its lock_timeout of 5 s bounds how long it waits for that
// lock, not how long it holds it. Call it at READ COMMITTED, as AuditStore.Maintain does:
// under REPEATABLE READ its snapshot predates its lock, so it misses rows committed while it
// waited. The session's TimeZone changes nothing.
const AuditSQL = `CREATE TABLE livewire_audit (
	id uuid NOT NULL DEFAULT uuidv7(),
	at timestamptz NOT NULL DEFAULT now(),
	tenant text NOT NULL DEFAULT '',
	actor text,
	real_actor text,
	mutation text NOT NULL,
	request_id text NOT NULL,
	idempotency_key uuid,
	parameters jsonb,
	status integer NOT NULL,
	duration_ms integer NOT NULL,
	PRIMARY KEY (at, id)
) PARTITION BY RANGE (at);
CREATE TABLE livewire_audit_default PARTITION OF livewire_audit DEFAULT;
CREATE FUNCTION livewire_audit_maintain(retention interval, as_of timestamptz DEFAULT now()) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
	cutoff timestamptz := least(as_of, now()) - retention; -- a future as_of drops nothing current
	month_start timestamp; -- the first of a month in UTC
	month_end timestamp;
	partition_name text;
	missing text[] := '{}';
	expired text[];
BEGIN
	PERFORM pg_advisory_xact_lock(hashtextextended('livewire_audit_maintain', 0));
	PERFORM set_config('lock_timeout', '5s', true);
	FOR month_offset IN 0..1 LOOP
		partition_name := 'livewire_audit_' || to_char(date_trunc('month', as_of AT TIME ZONE 'UTC') + make_interval(months => month_offset), 'YYYYMM');
		IF to_regclass(quote_ident(partition_name)) IS NULL THEN
			missing := missing || partition_name;
		END IF;
	END LOOP;
	SELECT coalesce(array_agg(class.relname), '{}') INTO expired
		FROM pg_inherits JOIN pg_class class ON class.oid = pg_inherits.inhrelid
		WHERE pg_inherits.inhparent = 'livewire_audit'::regclass AND class.relname ~ '^livewire_audit_[0-9]{6}$'
			AND (to_date(right(class.relname, 6), 'YYYYMM')::timestamp + interval '1 month') AT TIME ZONE 'UTC' <= cutoff;
	IF cardinality(missing) > 0 OR cardinality(expired) > 0 THEN
		-- One lock, before anything else: a writer then routes its row only once the partitions
		-- are final, and neither a writer nor a reader holding the table can deadlock with us.
		LOCK TABLE livewire_audit IN ACCESS EXCLUSIVE MODE;
	END IF;
	FOREACH partition_name IN ARRAY missing LOOP
		month_start := to_date(right(partition_name, 6), 'YYYYMM')::timestamp;
		month_end := month_start + interval '1 month';
		EXECUTE format('CREATE TABLE %I (LIKE livewire_audit INCLUDING DEFAULTS INCLUDING CONSTRAINTS)', partition_name);
		EXECUTE format('WITH moved AS (DELETE FROM livewire_audit_default WHERE at >= $1 AND at < $2 RETURNING *) INSERT INTO %I SELECT * FROM moved', partition_name)
			USING month_start AT TIME ZONE 'UTC', month_end AT TIME ZONE 'UTC';
		EXECUTE format('ALTER TABLE livewire_audit ATTACH PARTITION %I FOR VALUES FROM (%L) TO (%L)',
			partition_name, month_start AT TIME ZONE 'UTC', month_end AT TIME ZONE 'UTC');
	END LOOP;
	FOREACH partition_name IN ARRAY expired LOOP
		EXECUTE format('DROP TABLE %I', partition_name);
	END LOOP;
	DELETE FROM livewire_audit_default WHERE at < cutoff;
END
$$;
`

// PresenceSQL creates livewire_presence, the presences of D37 (PresenceStore), and its
// triggers: a join and a leave notify on Channel with the tenant as partition, an update
// only when a column other than updated_at changed, so a heartbeat reaches no subscription.
const PresenceSQL = `CREATE TABLE livewire_presence (
	tenant text NOT NULL DEFAULT '',
	target text NOT NULL,
	key text NOT NULL,
	socket text NOT NULL,
	owner text NOT NULL,
	display text NOT NULL DEFAULT '',
	state jsonb NOT NULL DEFAULT 'null',
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant, target, key, socket)
);
CREATE INDEX livewire_presence_socket ON livewire_presence (socket);
CREATE INDEX livewire_presence_updated_at ON livewire_presence (updated_at);
CREATE TRIGGER livewire_notify AFTER INSERT OR DELETE ON livewire_presence
	FOR EACH ROW EXECUTE FUNCTION livewire_notify('tenant');
CREATE TRIGGER livewire_notify_update AFTER UPDATE ON livewire_presence
	FOR EACH ROW WHEN ((to_jsonb(OLD) - 'updated_at') IS DISTINCT FROM (to_jsonb(NEW) - 'updated_at'))
	EXECUTE FUNCTION livewire_notify('tenant');
`

// historyFunctionSQL is the trigger function of HistorySQL: it closes the open version of
// the row and appends the next one with the actor of the transaction (D31). Both are stamped
// with clock_timestamp() once the row is locked, never earlier than the previous version, so
// a transaction that began before another one's write still writes the later version.
const historyFunctionSQL = `CREATE OR REPLACE FUNCTION livewire_history() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	history text := (SELECT format('%I.%I', namespace.nspname, class.relname || '_history')
		FROM pg_class class JOIN pg_namespace namespace ON namespace.oid = class.relnamespace
		WHERE class.oid = coalesce(pg_partition_root(TG_RELID), TG_RELID));
	current_row jsonb := to_jsonb(CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END);
	actor jsonb := nullif(current_setting('livewire.actor', true), '')::jsonb;
	row_id uuid := (current_row->>'id')::uuid;
	next_version integer;
	stamp timestamptz;
	last_life integer;
	last_operation text;
	last_transaction xid8;
	current_life integer;
	starts boolean := TG_OP = 'INSERT' OR (TG_OP = 'UPDATE' AND (to_jsonb(OLD)->>'id') IS DISTINCT FROM (current_row->>'id'));
BEGIN
	IF TG_OP = 'UPDATE' AND to_jsonb(OLD) = to_jsonb(NEW) THEN
		RETURN NULL;
	END IF;
	IF TG_OP = 'UPDATE' AND (to_jsonb(OLD)->>'id') IS DISTINCT FROM (current_row->>'id') THEN
		EXECUTE format('UPDATE %s SET valid_to = greatest(clock_timestamp(), valid_from) WHERE id = $1 AND valid_to IS NULL', history)
			USING (to_jsonb(OLD)->>'id')::uuid;
	END IF;
	EXECUTE format('SELECT coalesce(max(version), 0) + 1, greatest(clock_timestamp(), max(valid_from), max(valid_to)) FROM %s WHERE id = $1', history)
		INTO next_version, stamp USING row_id;
	-- The life of the version: a row arriving under an id (an insert, or an update that changes
	-- the id onto it) starts the id's next life, unless it is an insert whose id's last version
	-- is a delete of this very transaction, the two halves of a partition move; every other
	-- version continues the life of the one before.
	EXECUTE format('SELECT life, operation, transaction_id FROM %s WHERE id = $1 ORDER BY version DESC LIMIT 1', history)
		INTO last_life, last_operation, last_transaction USING row_id;
	current_life := coalesce(last_life, 1);
	IF starts AND last_life IS NOT NULL AND NOT (TG_OP = 'INSERT' AND last_operation = 'delete' AND last_transaction = pg_current_xact_id()) THEN
		current_life := last_life + 1;
	END IF;
	EXECUTE format('UPDATE %s SET valid_to = $2 WHERE id = $1 AND valid_to IS NULL', history) USING row_id, stamp;
	EXECUTE format('INSERT INTO %s (id, version, operation, data, valid_from, valid_to, actor, real_actor, mutation, request_id, life)
		VALUES ($1, $2, $3, $4, $9, CASE WHEN $3 = ''delete'' THEN $9 END, $5, $6, $7, $8, $10)', history)
		USING row_id, next_version, lower(TG_OP), current_row,
			actor->>'actor', actor->>'realActor', actor->>'mutation', actor->>'requestId', stamp, current_life;
	RETURN NULL;
END
$$;
`

// HistorySQL creates `<table>_history` and the trigger that appends a version on every
// insert, update and delete of table (D34): the row as jsonb in data, the version (1, 2, ...
// per id), the operation, valid_from and valid_to (NULL for the current version; a delete is
// a closed version of the last row), and the actor, real actor, mutation and request id the
// Transactor set in livewire.actor (D31; NULL outside an engine transaction). An update that
// changes nothing appends nothing, a TRUNCATE appends nothing either; an update that changes
// the id closes the old id's version and starts the new id's history. Table needs a uuid
// column id. For a natively partitioned table the history is the table's, whichever
// partition the row lives in; the id must be unique across the partitions, which PostgreSQL
// cannot enforce there and livewire assumes everywhere (Change.ID, item routes). An update
// that moves a row to another partition runs as a delete and an insert, so it appends a
// delete and an insert version, not an update. The trigger function livewire_history() is
// created or replaced with it. Applications only insert into history through the trigger;
// the trigger itself closes the previous version. A table name whose `_history` would pass
// PostgreSQL's 63 bytes is refused.
//
// Every version carries the life of its id (life, from 1) and the transaction that wrote it
// (transaction_id). A row that arrives under an id, by an insert or by an update that
// changes its id onto it, starts that id's next life: a re-create after a delete or a
// TRUNCATE, a ClientID create of an old id (D18) and an id change onto an id that lived
// before never continue the old item's history. Only the insert half of a partition move,
// whose id's last version is a delete of the same transaction, continues the life, so a
// moved row keeps its whole history; a delete and a re-create of one id inside one
// transaction cannot be told from a move and stay one life too. An id change starts the new
// id's history at the change; the old versions stay under the old id. Every other version
// continues the life of the one before. HistoryPage reads the current life only (D56).
func HistorySQL(table string) (string, error) {
	if len(table)+len(historySuffix) > maximumIdentifierBytes {
		return "", fmt.Errorf("postgresadapter: HistorySQL(%q): %q is longer than PostgreSQL's %d bytes", table, table+historySuffix, maximumIdentifierBytes)
	}
	return historyFunctionSQL + `CREATE TABLE ` + identifier(table+historySuffix) + ` (
	id uuid NOT NULL,
	version integer NOT NULL,
	operation text NOT NULL CHECK (operation IN ('insert', 'update', 'delete')),
	data jsonb NOT NULL,
	valid_from timestamptz NOT NULL,
	valid_to timestamptz,
	actor text,
	real_actor text,
	mutation text,
	request_id text,
	life integer NOT NULL,
	transaction_id xid8 NOT NULL DEFAULT pg_current_xact_id(),
	PRIMARY KEY (id, version)
);
CREATE OR REPLACE TRIGGER livewire_history AFTER INSERT OR UPDATE OR DELETE ON ` + identifier(table) + `
	FOR EACH ROW EXECUTE FUNCTION livewire_history();
`, nil
}

const (
	historySuffix = "_history"
	// maximumIdentifierBytes is PostgreSQL's NAMEDATALEN - 1: a longer name is cut.
	maximumIdentifierBytes = 63
)

// notifyTriggersSQL is the row trigger of table and the TRUNCATE trigger of table and of
// each of its present partitions (statement triggers are not cloned to partitions);
// argument is the partition column's literal or "". The DO block's dollar quote is one
// neither name contains, so no name ends it early.
func notifyTriggersSQL(table, argument string) string {
	truncateArgument := "''"
	if argument != "" {
		truncateArgument = "quote_literal(" + argument + ")"
	}
	quote := "$livewire$"
	for strings.Contains(table+argument, quote) {
		quote = quote[:len(quote)-1] + "_$"
	}
	return "CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON " + identifier(table) +
		" FOR EACH ROW EXECUTE FUNCTION livewire_notify(" + argument + ");\n" +
		"DO " + quote + " DECLARE relation regclass; BEGIN FOR relation IN SELECT " + literal(identifier(table)) + "::regclass UNION SELECT relid FROM pg_partition_tree(" + literal(identifier(table)) + ") LOOP\n" +
		"\tEXECUTE format('CREATE OR REPLACE TRIGGER livewire_notify_truncate AFTER TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION livewire_notify(%s)', relation, " +
		truncateArgument + ");\nEND LOOP; END " + quote + ";\n"
}

func identifier(name string) string { return pgx.Identifier{name}.Sanitize() }

func literal(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
