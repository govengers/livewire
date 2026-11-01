# Postgres

`postgresadapter` is livewire's PostgreSQL side on pgx/v5: the SQL for the application's migrations, the change feed, the transactor, the stores, and the helpers a repository's loaders call. PostgreSQL 18 (the tables default to `uuidv7()`).

## Migrations

Nothing runs DDL at startup (D22): livewire exports its SQL and the application's goose migrations execute it. The todo app runs it as one Go migration:

<!-- snippet: examples/todo/internal/database/database.go#schema -->
```go
// livewireSchema is livewire's SQL: the trigger function, the triggers that notify the change
// feed, partitioned by the workspace, the tenant (D25, D42), the history of todos (D34) and
// the tables of the stored filters, the idempotency records, the audit and the presences.
func livewireSchema(ctx context.Context, transaction *sql.Tx) error {
    history, err := livewirepostgres.HistorySQL("todos")
    if err != nil {
        return err
    }
    return execute(
        livewirepostgres.TriggerFunctionSQL,
        livewirepostgres.PartitionedTriggerSQL("todos", "workspace"),
        livewirepostgres.PartitionedTriggerSQL("attachments", "workspace"),
        livewirepostgres.PartitionedTriggerSQL("activity", "workspace"),
        history,
        livewirepostgres.FiltersSQL,
        livewirepostgres.IdempotencySQL,
        livewirepostgres.AuditSQL,
        livewirepostgres.PresenceSQL,
    )(ctx, transaction)
}
```

| SQL | Creates | Needed for |
|---|---|---|
| `TriggerFunctionSQL` | the function `livewire_notify()` | every trigger below; run it first |
| `TriggerSQL(tables...)` | a row and a TRUNCATE trigger per table | every table a live declaration lists |
| `PartitionedTriggerSQL(table, column)` | the same, carrying the column's value as the partition (D25) | a multi-tenant table: a change re-runs only its tenant's subscriptions |
| `HistorySQL(table)` | `<table>_history` and the trigger that appends a version on every write | a `Versioned` resource; the table needs a uuid column `id` |
| `FiltersSQL` | `livewire_filters` | `EngineOptions.FilterStore` |
| `IdempotencySQL` | `livewire_idempotency` | `EngineOptions.IdempotencyStore` |
| `AuditSQL` | `livewire_audit`, partitioned by month, and `livewire_audit_maintain` | `EngineOptions.AuditStore` |
| `PresenceSQL` | `livewire_presence` and its triggers | `EngineOptions.PresenceStore`; after `TriggerFunctionSQL` |

- A trigger notifies `{table, op, id, partition}` on the channel `livewire` (`postgresadapter.Channel`), one per row. A TRUNCATE notifies one change without an id, which reaches every subscription of the table. Two applications on one database receive each other's changes; the engine ignores tables it does not subscribe.
- A natively partitioned table reports its root table. A partition attached later notifies its own TRUNCATE only once `TriggerSQL` ran again; a DETACH notifies nothing.
- Leave tables with large binary columns out of `HistorySQL`: every version stores the whole row as jsonb.

### Deploy order of a changed SQL

A livewire release that changes the SQL of a store says how to move (D72 is the one so far): first deploy the code that reads and writes both forms to every replica, then run the migration, then roll the replicas. `IdempotencySQL` of the development line before v1.0.0 stored the response as `jsonb`; v1.0.0 stores `bytea`, so a replay answers the first answer's exact bytes. An application that ran the earlier SQL deploys v1.0.0 everywhere first, whose store handles both columns, then migrates with a migration of its own, from [postgresadapter/idempotency_test.go](../../postgresadapter/idempotency_test.go):

<!-- snippet: postgresadapter/idempotency_test.go#idempotency-upgrade -->
```go
// idempotencyUpgradeSQL is the application's migration of D72, run once every replica runs a
// store that reads and writes both columns: the response becomes the answer's bytes.
const idempotencyUpgradeSQL = `SET LOCAL lock_timeout = '5s';
ALTER TABLE livewire_idempotency ALTER COLUMN response TYPE bytea USING convert_to(response::text, 'UTF8');`
```

- The `ALTER` rewrites the table under an ACCESS EXCLUSIVE lock, so mutations wait for it; the table holds one `Window` of records (default 24 h), and the lock timeout keeps a busy table from stalling the deploy.
- It changes the result type of the store's prepared statements: every pooled connection that ran them fails its next idempotent call once with SQLSTATE 0A000 (`cached plan must not change result type`), and pgx prepares the statement again after it (D74). Roll the replicas after the migration, or accept one 500 per connection, which a retry with the same key repairs.

## The transactor

The engine owns the transaction of every unit of work (D31, D45). `postgresadapter.NewTransactor` begins a read as REPEATABLE READ READ ONLY and a write as READ COMMITTED READ WRITE, whatever the server's default, and sets with `SET LOCAL`:

- `livewire.tenant`: the tenant of the request (`""` with one tenant), on reads and writes;
- `livewire.actor`: on writes, `{"actor", "realActor", "mutation", "requestId"}`, which the history trigger records.

A mutation runs as: the checks and the body, `Begin`, the `Idempotency-Key` reserved (a finished key answers its recorded response, one in flight 409 `in_progress`), the handler, the audit row, the idempotency record, the commit, then `OutcomeAfterCommit`. A refused or failed call rolls everything back, and its key is free again.

The context of a loader or handler carries a query handle of that transaction, never its commit handle. A repository takes it and falls back to its pool outside an engine unit of work:

<!-- snippet: examples/todo/internal/repository/todo.go#transaction -->
```go
// database is the engine's transaction in ctx, or the pool outside an engine unit of work.
func (r *TodoRepository) database(ctx context.Context) postgresadapter.DBTX {
    if transaction := postgresadapter.TransactionFrom(ctx); transaction != nil {
        return transaction
    }
    return r.pool
}
```

`postgresadapter.DBTX` is a superset of sqlc's pgx/v5 `DBTX` and of govengers/jobs' `Executor`, so `sqlc.New(r.database(ctx))` and a job enqueued in the mutation's transaction work on it (D41, D53):

<!-- snippet: examples/todo/internal/repository/todo.go#enqueue -->
```go
// Enqueue enqueues jobs in the transaction of ctx, so they commit with the change (JOB-007).
func (r *TodoRepository) Enqueue(ctx context.Context, preparedJobs ...*jobs.PreparedJob) error {
    if _, err := r.jobStore.Enqueue(ctx, r.database(ctx), preparedJobs...); err != nil {
        return fmt.Errorf("enqueue: %w", err)
    }
    return nil
}
```

<!-- snippet: examples/todo/internal/service/todo.go#completion-job -->
```go
if change.Completes(*current) {
    completed := model.TodoCompletedKind.New(model.TodoCompletedPayload{
        TodoID: saved.ID, Workspace: member.Workspace, Title: saved.Title, Username: member.Username,
    }, nil)
    if err := s.todos.Enqueue(ctx, completed); err != nil {
        return nil, err
    }
    wideevent.FromContext(ctx).Bool("todo.completed", true)
}
```

A handler that calls an external system inside the transaction holds it open for that time; call it before, after, or from a job.

## Change feed and stores

<!-- snippet: examples/todo/cmd/todo/application.go#stores -->
```go
func newStores(pool *pgxpool.Pool) (*stores, error) {
    var created stores
    var failures [6]error
    created.transactor, failures[0] = postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
    created.filters, failures[1] = postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: pool})
    created.idempotency, failures[2] = postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: pool})
    created.audit, failures[3] = postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: pool})
    created.presences, failures[4] = postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: pool})
    created.listener, failures[5] = postgresadapter.NewListener(&postgresadapter.ListenerOptions{
        // the change feed's dedicated connection, configured like the pool's
        Connect: func(ctx context.Context) (*pgx.Conn, error) {
            return pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
        },
    })
    return &created, errors.Join(failures[:]...)
}
```

- The `Listener` holds one dedicated connection (`DSN`, or `Connect` for a pool's configuration or a token login), reconnects with backoff, pings after `PingInterval` (30 s) of silence and reports failures to `OnError`.
- `FilterStore`: at most `MaximumUnnamedPerOwner` (1000) unnamed filters per owner and tenant, the least recently used replaced, and `MaximumNamedPerOwner` (200) named ones; `Sweep` removes unnamed filters unused for 30 days.
- `IdempotencyStore`: `Sweep` removes the records older than `Window` (24 h).
- `AuditStore`: one row per call whose handler ran, in its transaction; a failed call's row after the rollback. `Maintain` creates this and next month's partitions and drops those older than `Retention` (365 days); run it at least monthly.
- `PresenceStore`: the presences; the engine heartbeats and sweeps them itself.

The engine runs no maintenance, so one replica does: the todo app schedules it as govengers/jobs jobs, which run once per period across replicas:

<!-- snippet: examples/todo/internal/controller/job.go#schedules -->
```go
// Schedules are the maintenance jobs every worker enqueues once per period with every other
// worker (JOB-009); pass them as WorkerOptions.Schedules.
func Schedules() []jobs.Schedule {
    return []jobs.Schedule{
        {Name: model.FilterSweepKind.Name(), Next: jobs.Every(sweepInterval), Job: model.FilterSweepKind.New(struct{}{}, nil)},
        {Name: model.IdempotencySweepKind.Name(), Next: jobs.Every(sweepInterval), Job: model.IdempotencySweepKind.New(struct{}{}, nil)},
        {Name: model.AuditMaintainKind.Name(), Next: jobs.Every(maintainInterval), Job: model.AuditMaintainKind.New(struct{}{}, nil)},
    }
}
```

## Lists, history, distinct and export

A resource's loaders call the helpers with its `query.ResourceSQL{From, IDColumn, Derived}`. Inside an engine unit of work every helper runs on the engine's transaction, whatever `DBTX` it gets, so it reads the engine's snapshot under `livewire.tenant`. Outside one `ListPage`, `HistoryPage` and `WriteCSV` open a REPEATABLE READ READ ONLY transaction of their own.

`ListPage` runs the page query (ids and derived columns), the capped count and the hydration on one snapshot and returns the items in page order with the next cursor:

<!-- snippet: examples/todo/internal/repository/todo.go#list -->
```go
// List reads one page of todos under the query's scope, filter, sort and page.
func (r *TodoRepository) List(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[model.Todo], error) {
    page, err := postgresadapter.ListPage(ctx, r.pool, todoSQL, listQuery, hydrateTodos)
    if err != nil {
        return nil, fmt.Errorf("list todos: %w", err)
    }
    return page, nil
}
```

<!-- snippet: examples/todo/internal/repository/todo.go#hydrate -->
```go
// hydrateTodos reads a page's todos by id (postgresadapter.Hydrate).
func hydrateTodos(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]model.Todo, error) {
    found, err := sqlc.New(database).ListTodosByIDs(ctx, postgresadapter.PageIDs(rows))
    if err != nil {
        return nil, err
    }
    todos := make(map[uuid.UUID]model.Todo, len(found))
    for _, row := range found {
        todos[row.ID] = *toTodo(row)
    }
    return todos, nil
}
```

<!-- snippet: examples/todo/internal/repository/todo.go#distinct -->
```go
// Distinct counts the values of one field under the query.
func (r *TodoRepository) Distinct(ctx context.Context, field string, listQuery *query.ListQuery) ([]livewire.DistinctValue, error) {
    declared, found := TodoFields().Lookup(field)
    if !found {
        return nil, fmt.Errorf("distinct todos: the field %q is not declared", field)
    }
    values, err := postgresadapter.Distinct(ctx, r.pool, todoSQL, listQuery, declared)
    if err != nil {
        return nil, fmt.Errorf("distinct todos: %w", err)
    }
    return values, nil
}
```

`HistoryPage` reads a version page of `<table>_history` ([History](Reads-and-Filters.md#history)); `WriteCSV` streams an export by keyset in chunks of `ChunkSize` (1000) rows ([Export](Reads-and-Filters.md#export)). A cursor value the column refuses (SQLSTATE 22P02, a forged enum value) is `invalid_cursor`. Database failures come back wrapped with `%w`: `errors.As` finds `*pgconn.PgError`.

## Row Level Security

Every engine transaction sets `livewire.tenant`, so a policy can enforce tenant isolation in the database, under the resource's `Scope` (D42). From [postgresadapter/transactor_test.go](../../postgresadapter/transactor_test.go), which proves it:

<!-- snippet: postgresadapter/transactor_test.go#tenant-policy -->
```go
// tenantPolicySQL is a migration's Row Level Security policy on the tenant every engine
// transaction sets (D42): a role the policy applies to reads and writes its tenant's rows only,
// and outside an engine transaction, with no tenant set, none.
const tenantPolicySQL = `ALTER TABLE gadgets ENABLE ROW LEVEL SECURITY;
CREATE POLICY gadgets_tenant ON gadgets USING (tenant_id = current_setting('livewire.tenant', true));`
```

- A policy binds the application's role only when that role neither owns the table nor is a superuser (or the table has `FORCE ROW LEVEL SECURITY`).
- Outside an engine transaction no tenant is set, so a job or a migration under the policy's role sees no rows; give it its own role or set the tenant itself.
- `livewiretest.TenantIsolation` checks the outcome through the API, policy or scope ([Testing](Testing.md#tenantisolation)).
