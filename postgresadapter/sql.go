package postgresadapter

import (
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
// value ("" without one). An update that moves a row to another partition notifies both.
const TriggerFunctionSQL = `CREATE OR REPLACE FUNCTION livewire_notify() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	old_row jsonb := CASE WHEN TG_OP IN ('UPDATE', 'DELETE') THEN to_jsonb(OLD) END;
	new_row jsonb := CASE WHEN TG_OP IN ('INSERT', 'UPDATE') THEN to_jsonb(NEW) END;
	partition_column text := CASE WHEN TG_NARGS > 0 THEN TG_ARGV[0] END;
BEGIN
	PERFORM pg_notify('` + Channel + `', json_build_object(
		'table', TG_TABLE_NAME, 'op', lower(TG_OP), 'id', coalesce(coalesce(new_row, old_row)->>'id', ''),
		'partition', coalesce(coalesce(new_row, old_row)->>partition_column, ''))::text);
	IF TG_OP = 'UPDATE' AND (old_row->>partition_column) IS DISTINCT FROM (new_row->>partition_column) THEN
		PERFORM pg_notify('` + Channel + `', json_build_object(
			'table', TG_TABLE_NAME, 'op', 'update', 'id', coalesce(old_row->>'id', ''),
			'partition', coalesce(old_row->>partition_column, ''))::text);
	END IF;
	RETURN NULL;
END
$$;
`

// TriggerSQL makes every table notify on each row change (D22); the changes carry no
// partition, so they reach the subscriptions of every partition. A table name is one
// identifier in the search path, quoted as it is.
func TriggerSQL(tables ...string) string {
	var statements strings.Builder
	for _, table := range tables {
		statements.WriteString("CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON " +
			identifier(table) + " FOR EACH ROW EXECUTE FUNCTION livewire_notify();\n")
	}
	return statements.String()
}

// PartitionedTriggerSQL is TriggerSQL for one table whose changes carry the value of column
// as their partition (D25), so a change re-runs only the subscriptions of that partition and
// those without one.
func PartitionedTriggerSQL(table, column string) string {
	return "CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON " +
		identifier(table) + " FOR EACH ROW EXECUTE FUNCTION livewire_notify(" + literal(column) + ");\n"
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
// (IdempotencyStore). Status is 0 while a call is in flight.
const IdempotencySQL = `CREATE TABLE livewire_idempotency (
	owner text NOT NULL CHECK (owner <> ''),
	tenant text NOT NULL DEFAULT '',
	key uuid NOT NULL,
	mutation text NOT NULL CHECK (mutation <> ''),
	status integer NOT NULL DEFAULT 0,
	response jsonb,
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (owner, tenant, key)
);
CREATE INDEX livewire_idempotency_created_at ON livewire_idempotency (created_at);
`

// AuditSQL creates livewire_audit, one row per mutation (D32, AuditStore), partitioned by
// month on at. Rows of a month without its partition land in livewire_audit_default;
// livewire_audit_maintain(retention), which AuditStore.Maintain calls, creates the partitions
// of this and the next month (moving their rows out of the default partition), drops the
// partitions that ended before now() - retention and deletes the default partition's rows
// older than that.
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
CREATE FUNCTION livewire_audit_maintain(retention interval) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
	month_start timestamptz;
	partition_name text;
	child record;
BEGIN
	PERFORM pg_advisory_xact_lock(hashtextextended('livewire_audit_maintain', 0));
	PERFORM set_config('lock_timeout', '5s', true);
	FOR month_offset IN 0..1 LOOP
		month_start := date_trunc('month', now(), 'UTC') + make_interval(months => month_offset);
		partition_name := 'livewire_audit_' || to_char(month_start AT TIME ZONE 'UTC', 'YYYYMM');
		CONTINUE WHEN to_regclass(quote_ident(partition_name)) IS NOT NULL;
		EXECUTE format('CREATE TABLE %I (LIKE livewire_audit INCLUDING DEFAULTS INCLUDING CONSTRAINTS)', partition_name);
		EXECUTE format('WITH moved AS (DELETE FROM livewire_audit_default WHERE at >= $1 AND at < $2 RETURNING *) INSERT INTO %I SELECT * FROM moved', partition_name)
			USING month_start, month_start + interval '1 month';
		EXECUTE format('ALTER TABLE livewire_audit ATTACH PARTITION %I FOR VALUES FROM (%L) TO (%L)',
			partition_name, month_start, month_start + interval '1 month');
	END LOOP;
	FOR child IN
		SELECT class.relname FROM pg_inherits JOIN pg_class class ON class.oid = pg_inherits.inhrelid
		WHERE pg_inherits.inhparent = 'livewire_audit'::regclass AND class.relname ~ '^livewire_audit_[0-9]{6}$'
	LOOP
		IF (to_date(right(child.relname, 6), 'YYYYMM')::timestamp AT TIME ZONE 'UTC') + interval '1 month' <= now() - retention THEN
			EXECUTE format('DROP TABLE %I', child.relname);
		END IF;
	END LOOP;
	DELETE FROM livewire_audit_default WHERE at < now() - retention;
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
// the row and appends the next one with the actor of the transaction (D31).
const historyFunctionSQL = `CREATE OR REPLACE FUNCTION livewire_history() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
	history text := format('%I.%I', TG_TABLE_SCHEMA, TG_TABLE_NAME || '_history');
	current_row jsonb := to_jsonb(CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END);
	actor jsonb := nullif(current_setting('livewire.actor', true), '')::jsonb;
	row_id uuid := (current_row->>'id')::uuid;
	next_version integer;
BEGIN
	IF TG_OP = 'UPDATE' AND to_jsonb(OLD) = to_jsonb(NEW) THEN
		RETURN NULL;
	END IF;
	EXECUTE format('SELECT coalesce(max(version), 0) + 1 FROM %s WHERE id = $1', history) INTO next_version USING row_id;
	EXECUTE format('UPDATE %s SET valid_to = now() WHERE id = $1 AND valid_to IS NULL', history) USING row_id;
	EXECUTE format('INSERT INTO %s (id, version, operation, data, valid_from, valid_to, actor, real_actor, mutation, request_id)
		VALUES ($1, $2, $3, $4, now(), CASE WHEN $3 = ''delete'' THEN now() END, $5, $6, $7, $8)', history)
		USING row_id, next_version, lower(TG_OP), current_row,
			actor->>'actor', actor->>'realActor', actor->>'mutation', actor->>'requestId';
	RETURN NULL;
END
$$;
`

// HistorySQL creates `<table>_history` and the trigger that appends a version on every
// insert, update and delete of table (D34): the row as jsonb in data, the version (1, 2, ...
// per id), the operation, valid_from and valid_to (NULL for the current version; a delete is
// a closed version of the last row), and the actor, real actor, mutation and request id the
// Transactor set in livewire.actor (D31; NULL outside an engine transaction). An update that
// changes nothing appends nothing. Table needs a uuid column id. The trigger function
// livewire_history() is created or replaced with it. Applications only insert into history
// through the trigger; the trigger itself closes the previous version.
func HistorySQL(table string) string {
	return historyFunctionSQL + `CREATE TABLE ` + identifier(table+"_history") + ` (
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
	PRIMARY KEY (id, version)
);
CREATE OR REPLACE TRIGGER livewire_history AFTER INSERT OR UPDATE OR DELETE ON ` + identifier(table) + `
	FOR EACH ROW EXECUTE FUNCTION livewire_history();
`
}

func identifier(name string) string { return pgx.Identifier{name}.Sanitize() }

func literal(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
