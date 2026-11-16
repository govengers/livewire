-- The trigger SQL of livewire v1.0.1 (livewire_notify(), livewire_history() and the presence
-- update trigger), which TriggerUpgradeSQL upgrades (D88).
CREATE OR REPLACE FUNCTION livewire_notify() RETURNS trigger LANGUAGE plpgsql AS $$
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
	PERFORM pg_notify('livewire', json_build_object('table', table_name, 'op', operation,
		'id', CASE WHEN octet_length(row_id) <= 500 THEN row_id ELSE '' END,
		'partition', CASE WHEN octet_length(row_partition) <= 500 THEN row_partition ELSE '' END)::text);
	IF TG_OP = 'UPDATE' AND old_partition <> row_partition THEN
		PERFORM pg_notify('livewire', json_build_object('table', table_name, 'op', 'update',
			'id', CASE WHEN octet_length(old_id) <= 500 THEN old_id ELSE '' END,
			'partition', CASE WHEN octet_length(old_partition) <= 500 THEN old_partition ELSE '' END)::text);
	END IF;
	RETURN NULL;
END
$$;
CREATE OR REPLACE FUNCTION livewire_history() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE OR REPLACE TRIGGER livewire_notify_update AFTER UPDATE ON livewire_presence
	FOR EACH ROW WHEN ((to_jsonb(OLD) - 'updated_at') IS DISTINCT FROM (to_jsonb(NEW) - 'updated_at'))
	EXECUTE FUNCTION livewire_notify('tenant');
-- The row triggers v1.0.1's TriggerSQL, PartitionedTriggerSQL and PresenceSQL created on the
-- test fixture's tables and the test's documents; their TRUNCATE triggers have this form.
CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON parts FOR EACH ROW EXECUTE FUNCTION livewire_notify();
CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON gadgets FOR EACH ROW EXECUTE FUNCTION livewire_notify('tenant_id');
CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON orders FOR EACH ROW EXECUTE FUNCTION livewire_notify();
CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR UPDATE OR DELETE ON documents FOR EACH ROW EXECUTE FUNCTION livewire_notify('tenant');
CREATE OR REPLACE TRIGGER livewire_notify AFTER INSERT OR DELETE ON livewire_presence FOR EACH ROW EXECUTE FUNCTION livewire_notify('tenant');
