# livewire v1 design

Status: agreed 31.10.2026 after seven grill rounds with MaPa. Binding for the v1.0.0 implementation. Decisions are numbered; a later change gets a new number, never a rewrite.

livewire declares an API once in Go and serves it as REST, as a WebSocket with reads, subscriptions and writes, and as generated clients (TypeScript, Go, CLI) plus an OpenAPI 3.0.3 document. It is the contract between database, backend and UI for every govengers app. Successor of `github.com/mxcd/livewire` v0.2.x; module `github.com/govengers/livewire`, profile `library`, govengers/standards apply.

## 1. Decisions

| ID | Decision |
|----|----------|
| D1 | The core depends on `github.com/govengers/auth`: `authorization.Check` is the check type, `auth.IdentityFrom(ctx)` the identity, `auth.ResponseFor` the error mapping. No auth hooks of its own. |
| D2 | Reads and writes on both transports: REST routes and socket ops `query`, `subscribe`, `unsubscribe`, `call`, `batch`, `join`, `leave`, `ping`. |
| D3 | Core on `net/http` and `context`; gin in `ginadapter`, pgx in `postgresadapter`, Prometheus in `prometheusadapter` (LIB-007, LIB-011). The core declares `ChangeFeed`, `FilterStore`, `IdempotencyStore`; `postgresadapter` implements them. |
| D4 | No logging. An `Observer` callback reports outcomes; the ginadapter records REST outcomes on the request's wide event like auth's ginadapter does. |
| D5 | Every component is `New...(options *...Options) (*X, error)`; constructors validate, copy, never write the caller's options (DES-001). |
| D6 | Socket identity is re-validated through `session.Guard.Identify` (auth v1.3.0) or any `IdentitySource` on every subscribe, call, re-run and a tick (default 60 s). The option is required. |
| D7 | Close code 4401 only when the identity is gone (no session, expired, revoked). A changed identity re-runs subscriptions under the new rights; a subscription the new identity may not read ends with an `error` push `forbidden`. |
| D8 | Static gate first, data second: a declaration's `Check` runs before parameters are resolved against the database and before a body is read; data-dependent authorization lives in the loader or handler (AUTH-005). No `Context` hook, no per-declaration middleware. |
| D9 | Origin allowlist is a required engine option; the request's own host is always allowed. |
| D10 | Per-socket limits: 64 subscriptions, 16 in-flight requests, 8 MiB queued bytes, 64 KiB frame. Per engine: bounded query semaphore shared by loads and identity checks (default 32), optional sockets per owner. |
| D11 | Filterable resources declare `Fields` (name, type, SQL expression, filterable, sortable, searchable, nullable, enum values), `DefaultSort`, `Scope`, `MaximumLimit`, `CountCap`. The `query` package validates and compiles filter documents; the app's repository executes the page and count SQL with pgx and hydrates rows by id with sqlc. |
| D12 | Filter document: `and`, `or`, `not`, per-type operators (databridge's set), `search`, relative times (`-7d`), limits: depth 8, nodes 200, list values 500, string 1 KiB, document 16 KiB. |
| D13 | Stored filters are library-owned: table `livewire_filters`, content-addressed dedup per owner, resource and context, routes `POST /filters`, `GET /filters`, `DELETE /filters/{id}`, `filterId` on every filterable read, inline `filter` registered on subscribe. Owner key is `provider:subject`. Unnamed filters unused for 30 days are swept. Owner-only visibility. |
| D14 | Every list answers `ListResponse{limit, offset, total, items}` on REST and as socket snapshot; diffs carry `total`; `limit` is clamped; `CountCap` makes `total` at most cap+1. |
| D15 | `distinct` is a read on a filterable resource: one filterable scalar field, under scope and filter, `[{value, count}]`, capped (default 1000), subscribable. |
| D16 | Idempotency keys on every mutation: the clients send `Idempotency-Key` (UUID v7); the server reserves the key and records the response in `livewire_idempotency` inside the mutation's transaction (D31), so a replay is exactly-once; a repeat answers the recorded response, a repeat of an in-flight key answers 409 `in_progress`. |
| D17 | `NewUpload` declares a binary write: REST only, multipart or raw body, declared content types and size limit, typed params, JSON response. |
| D18 | Creates may accept a client-generated `id`: `ClientID(UUIDv7)` or `ClientID(UUIDv4)` per create, validated by version; without the option no `id` is accepted. |
| D19 | Generated artifacts: TypeScript (`types.ts`, `api.ts`, `filters.ts`, runtime, `vue.ts`), OpenAPI 3.0.3 with `x-websocket`, a Go client package importing the app's DTO package, a CLI on `github.com/urfave/cli/v3`. |
| D20 | CLI and Go client authenticate with an API key only (flag or environment variable). |
| D21 | `useLive` keeps its localStorage snapshot, keyed by identity (`config.identity()`), cleared on identity change. `useLiveList` holds filter, sort and page state. Vue only, no Quasar. |
| D22 | Trigger and table DDL ship as exported SQL for the app's goose migrations; nothing runs DDL at startup. |
| D23 | Contract gates: `livewiretest.GeneratedIsCurrent`, `livewiretest.Parity` (REST body equals socket snapshot), `livewiretest.Stale` (a write to a declared table re-runs the subscription), and an `oasdiff breaking` recipe against the previous tag's spec. |
| D24 | Engine tests run on embedded-postgres; the e2e suite runs two engines on one database plus the TypeScript runtime under bun, build tag `e2e`. |
| D25 | Partitioned fan-out stays: a `Partition` function names a subscription's partition, partitioned triggers carry the row's column. |
| D26 | OpenAPI 3.0.3 (`nullable: true`, no `anyOf null`), served by the app if it wants to; the generator only writes the file. |
| D27 | Module path `github.com/govengers/livewire`, release v1.0.0 with a migration table from mxcd v0.2.x; mxcd/livewire archived with a notice. |
| D28 | Docs: README (overview, ~100 lines), `llms.txt`, `docs/wiki/*.md`, `examples/todo` with a test. Lean. |
| D29 | Names are spelled out (NAM-001): `generate` not `gen`, `resource` not `res`, `subscription` not `sub`; DTO is written `Item` or `Type` in exported names. |
| D30 | Wide events for socket frames: the Observer outcome carries op, target, identity, duration and error; the app emits one wide event per frame if it wants one. |
| D31 | The engine owns the mutation transaction through a `Transactor` the core declares and `postgresadapter` implements: begin, `SET LOCAL livewire.actor` (effective and real identity, mutation, request id), transaction in the context (`livewire.TransactionFrom(ctx)`), handler, audit row, idempotency record, commit, answer. Repositories take the transaction from the context and fall back to the pool. A handler that calls an external system inside holds the transaction open; do it before, after or in a job. |
| D32 | Action audit: one row per mutation in `livewire_audit` (actor, real actor, mutation, params, key, status, duration), written in the transaction of D31, partitioned by month, retention sweep configurable. |
| D33 | Bulk calls: `POST /batch` and socket op `batch` run several mutations in one transaction of D31, all or nothing, one idempotency key, one audit row per call, one response per call. Uploads are excluded. |
| D34 | Versioning: a resource declares `Versioned: true`; `postgresadapter.HistorySQL(table)` creates `<table>_history` (row as jsonb, version, operation, valid_from, valid_to, actor, real_actor, mutation, request_id) and a trigger that appends on insert, update and delete, reading the actor of D31. The live table stays mutable; history is insert-only; INT-002's field changes are the diff of consecutive versions; `GET /{resource}/{id}/history` is a live, filterable read; the item carries `version`. Standards exception: the audit entry is written by a trigger in the mutation's transaction, not by a sqlc query in the repository. |
| D35 | Export: `GET /{resource}/export?format=csv&filterId=...&sort=...` streams every row under scope and filter, columns from the declared fields or `ExportColumns`, chunked by keyset. CSV in the core (`encoding/csv`); `xlsx` in `livewire/xlsxadapter` on excelize, last work package, first to cut. REST only, a read for authorization, Observer outcome with the row count. |
| D36 | Optimistic effects: `Effect: livewire.Upserts("parts")` or `livewire.Removes("parts")` on a mutation; the runtime applies the response or the removal by id to those live lists at once with a `pending` marker, the next push reconciles (server wins), an error rolls back. |
| D37 | Presence: socket ops `join {target, key, state}` and `leave`; rows in `livewire_presence` (socket, owner, display, target, key, state, updated_at), heartbeat 30 s, sweep after 90 s, changes through the change feed, `presence` is a subscribable target per record, `useLivePresence` in `vue.ts`. Postgres only. Collaborative editing of the same values is v2 and builds on this. |
| D38 | Cursor mode: a filterable resource may opt into keyset pagination over its sort fields plus `id`, opaque `after`, `ListResponse.next`, `total` still capped; offset stays the default; `useLiveList({ mode: 'cursor' })` tiles pages with `loadMore()`. |
| D39 | Column authorization: `query.Field` has `Read` and `Write` checks (`Write` defaults to `Read`, nil inherits the resource). A refused `Read` strips the field from every item on REST, socket, export and history and refuses filter, sort, search and distinct on it with 403 `forbidden_field`; re-evaluated on every identity re-validation. A mutation with `Resource` set applies the resource's `Write` checks to its body by JSON name; `FieldChecks` adds checks to any mutation. Generated types mark guarded fields optional, OpenAPI tags them `x-livewire-guarded`. Typed-params lists name `Guarded []GuardedField{Name, Read}` for the same effect. |
| D40 | Every mutation body is decoded with unknown JSON fields rejected (400 `invalid_request` naming the field). |
| D41 | No change log, no consumers in livewire (cut after Q26-Q31): business reactions to data changes go through `github.com/govengers/jobs` (the org's Postgres job queue, JOB-001..006; cross-service through `govengers/events`), enqueued inside the transaction of D31 with `jobs/postgresadapter.Store.Enqueue(ctx, executor, ...)` where the executor is the query handle `livewire/postgresadapter.TransactionFrom(ctx)` returns (D45: that handle therefore satisfies both sqlc's `DBTX` and jobs' `Executor`); an `AfterCommit` observer outcome per mutation (name, params, actor, status) serves at-most-once reactions like cache invalidation on the replica that ran it. A workflow engine is a jobs worker, not a livewire feature (MaPa, 01.11.2026: jobs, not river). The `examples/todo` app enqueues a job in a mutation to show the wiring. |
| D42 | Tenant as a first-class engine concept (MaPa, 31.10.2026): `RegistryOptions.Tenant func(ctx, parameters any) (string, error)` resolves the tenant once per request or subscription after the static check (path parameter, header or identity; nil parameters for filterable resource reads); the engine puts it in the context (`livewire.TenantFrom(ctx)`), uses it as the default partition (D25), passes it to `Scope`, and sets `SET LOCAL livewire.tenant` in every engine transaction, reads included, so an app may enforce isolation with Row Level Security policies (`USING (tenant_id = current_setting('livewire.tenant')::uuid)`). Filters, presence, audit and idempotency rows carry the tenant; a stored filter is scoped to owner and tenant. `livewiretest.TenantIsolation` runs every filterable resource as two tenants and fails when rows cross (DAT-017). `Tenant` nil is single-tenant and changes nothing. |
| D43 | Declarations are values: `NewResource`, `NewList`, `NewObject`, `NewMutation`, `NewUpload` return the declaration only and never panic; a declaration mistake surfaces as the error of `Registry.Add`, which `main` checks, so the registry stays one expression (section 3). Filed as a DES-001 exception in `.govengers.yml`. |
| D44 | Request bodies are validated in the core decoder with go-playground/validator on the `validate` tag (not gin's `binding` tag: the core is transport-neutral and the socket never passes gin); a tag the validator cannot parse is a declaration error surfaced by `Add`. Field errors are reported by JSON path. Section 3's "binding/validator tags" wording is superseded. |
| D45 | The transaction in the context is a query handle, never the commit handle: `livewire.TransactionFrom(ctx)` returns the adapter's query-only value (`postgresadapter.TransactionFrom(ctx)` types it as sqlc's `DBTX`), only the engine holds Commit and Rollback. Every engine unit of work gets a transaction, reads included (so `livewire.tenant` is set for RLS on reads, D42); outside an engine unit of work the handle is nil and a repository falls back to its pool. Section 7's snippet reads `queries := r.queries.WithTx(postgresadapter.TransactionFrom(ctx))`. |
| D46 | Socket targets are the declaration name with an optional kind suffix: `name` (list or object), `name.item`, `name.distinct`, `name.presence`; `join` and `leave` name the declaration and carry the record under the `key` parameter. The REST adapter maps its routes to the same `ReadRequest{Target, Kind, Source}`, so both transports run one read pipeline. |
| D47 | Batch wire format: `POST /batch` and the socket op `batch` carry a JSON array of `{target, params, body}` and answer `[{status, payload}]` in order; one transaction, all or nothing; a refusal names the failing call under `calls.<index>` in `fields`. Uploads are excluded. |
| D48 | The engine's own test suite runs on in-memory fakes of the adapter contracts (fast, deterministic, race-checked); the engine against embedded Postgres (real transactions, LISTEN feed, advisory locks) is the `postgresadapter` suite of WP4 plus the two-engine e2e of WP8. D24's "engine tests on embedded-postgres" is satisfied there, not in WP3. |
| D49 | One Observer: the registry's (`RegistryOptions.Observer`) receives every engine outcome; `EngineOptions` carries none. The migration table's `Observer` on EngineOptions is superseded. Outcome kinds grew by open, close, revalidate, feed and heartbeat so background failures reach the application. |
| D50 | FilterStore and PresenceStore calls run outside the read transaction: a filter lookup marks the filter used (a write a READ ONLY transaction cannot do) and presence rows are the library's own, keyed by the explicit tenant. Every other read runs inside the engine's read transaction (D45). |
| D51 | Identity re-validation runs on subscribe, query, call, batch, join, leave, every re-run and the RevalidateInterval tick; `ping` and `unsubscribe` are exempt (they carry no data, and the tick closes a revoked socket within the interval). Section 6's "every op" reads with this exemption. |
| D52 | The engine's query semaphore (`QueryConcurrency`) bounds socket work only: subscribe, query, call, batch, re-runs and identity checks started from sockets. REST calls into `Engine.Read`, `Call`, `Batch` and `Upload` are bounded by the HTTP server and the connection pool, so a burst of socket re-runs can never take the connections REST requests need (the intent behind D10). |
| D53 | The postgresadapter surface as WP4 built it; it supersedes the repository snippets of D45 and sections 7 and 8. A repository builds its queries per call on the handle: `database := postgresadapter.TransactionFrom(ctx)`, the pool when it is nil, then `sqlc.New(database)` (sqlc's `WithTx` takes a `pgx.Tx`, which the query-only handle is not; `postgresadapter.DBTX` is a superset of sqlc's pgx/v5 DBTX). `postgresadapter.ListPage[Item](ctx, database DBTX, *query.ResourceSQL, *query.ListQuery, Hydrate[Item]) (*Page[Item], error)` with `Hydrate[Item] func(ctx, DBTX, []PageRow) (map[uuid.UUID]Item, error)` (`PageRow{ID, Derived}`, `PageIDs` for `= ANY`) puts the items in page order; `Page{Items, Total, Next *query.Cursor}` carries the cursor unencoded, a cursor-mode resource encodes it. Inside an engine unit of work `ListPage`, `Distinct` and `WriteCSV` run on the engine's transaction whatever DBTX they are given, so section 8's pool argument reads the engine's snapshot under `livewire.tenant`; outside one `ListPage` and `WriteCSV` open a REPEATABLE READ READ ONLY transaction of their own, while `Distinct`, a single SELECT with its own snapshot, runs on the DBTX as given. Triggers and the Listener share one NOTIFY channel, `postgresadapter.Channel` (`livewire`), not configurable: two apps on one database receive each other's changes, which the engine ignores for tables it does not subscribe. |
| D54 | Filter caps (D13): `FilterStore` keeps at most `MaximumUnnamedPerOwner` (default 1000) unnamed filters per owner and tenant by replacing the owner's least recently used one, never by refusing, since a refusal would lock an active user's inline subscribes out until the 30-day sweep, and a client sends an evicted filter inline again: a 404 on an unnamed `filterId` means register the filter inline again (the WP6 runtime does); at most `MaximumNamedPerOwner` (default 200) named filters, a further one refused with 400 `invalid_request` at `filter`. The Saves of one owner run under an advisory lock, so both bounds are exact. |
| D55 | Cursor mode reaches the response (WP5; supersedes the `List` loader shape of section 3 and section 8 and D53's `postgresadapter.Page`): `ResourceOptions.List` returns `(*livewire.Page[Item], error)` with `Page{Items, Total, Next *query.Cursor}`, the type `postgresadapter.ListPage` returns, so a loader is `return postgresadapter.ListPage(ctx, r.pool, partSQL, listQuery, hydrate)`. The engine answers `Next.Encode()` as `ListResponse.next` for a resource with `Cursor` set and ignores `Next` otherwise; `Resource.LoadList` returns the loader's page. |
| D56 | History read (WP5, completes D34): `ResourceOptions.History func(ctx, *query.ListQuery) (*Page[Version[Item]], error)` on a `Versioned` resource whose `Item` key is its uuid field `id` (the column `<table>_history` keys versions by; a key on another field would let an item the caller may read name another row's history, so `Add` refuses it) serves `GET {ItemPath}/history` and the socket target `name.history`. `Version[Item]{id, operation, validFrom, validTo, actor, realActor, mutation, requestId, data}` is one row of `<table>_history`, `id` the version number (the key a live history is diffed by), `data` the item in its JSON shape. The query is the reserved filter, sort and page over `HistoryFields()` (the history table's columns; default sort by version, offset paging only, the resource's `CountCap`) scoped to `id = key`; a history's inline and stored filters parse over `HistoryFields()` on every path (query, subscribe, time dependency, `POST /filters`) and belong to the target `name.history`. The item read runs first under the caller, so `Scope` and ownership decide who sees a history; `data` passes the column checks (D39). A deleted item has no history on this route: its item read is 404, so the read answers 404 and a live history ends with the `not_found` error push of the delete; `operation` filters insert and update only. Who deleted an item is the audit's and the application's own history route's to answer, which can authorize a deleted row as it sees fit. The subscription re-runs on the resource's `Tables`: the history trigger writes every version in the transaction of the table's write, whose NOTIFY therefore covers it, so the history table needs no trigger of its own; `postgresadapter.HistoryPage` serves the loader. |
| D57 | Export entry point (WP5, completes D35): `ResourceOptions.Export func(ctx, *query.ListQuery, columns query.Fields, io.Writer) (rows int, error)` serves `GET {Path}/export`; the repository passes the arguments to `postgresadapter.WriteCSV`. `Engine.Export(ctx, ExportRequest{Target, Source, Writer})` authorizes and resolves like a list read (Check, reserved parameters, stored filter, tenant), refuses a `format` other than `csv` with 400, and runs the loader in a read-only engine transaction with the export columns whose `Read` passes; nothing is written before the loader writes, so a refusal still answers an error body. `OutcomeExport` carries the written rows in `Outcome.Rows`. |
| D58 | A batch names each call to its transaction (WP5, completes D33 and D34): a `Transaction` may implement `livewire.CallDescriber`; the engine calls `DescribeCall(ctx, TransactionDetails)` before each call of a batch with that call's mutation, parameters, identity and the batch's request id, and the postgresadapter's transaction sets `livewire.actor` again (SET LOCAL), so the history rows of each call carry its own mutation. The outer transaction still begins with an empty `Mutation`; a failing `DescribeCall` fails the batch. |
| D59 | What the REST adapter and the metrics need from the core (WP5): the stored filter routes of D13 run in the engine, `SaveFilter(ctx, body)` (a `FilterRequest{resource, context, name, filter}`, unknown members refused, the resource's static `Check`, the document parsed and column-checked like a read, 201 with a `FilterItem`), `ListFilters(ctx, resource)` (a `ListResponse` of the caller's named filters) and `DeleteFilter(ctx, id)`, each under the tenant of a resource read and the owner; `BatchPath` (`/batch`) and `FiltersPath` (`/filters`) are the library's routes below `BasePath`, which `Registry.Add` refuses to a declaration. Presence reads have a REST route, `PresencePath()` (`{Path}/presence?key=...`) on a resource with an item read and on an object. For the metrics the Observer gets `OutcomeEnd` (a published subscription ended: unsubscribed, by an error push or with its socket) and `Outcome.QueryWait` (the unit's wait for query slots, D52; zero on REST), and `Outcome.Target` is a closed set: a registered name with its read kind after a dot (`parts.item`, on REST too), `batch`, `filters` (both reserved names), or `""` for whatever else a request named (an unknown target, a subscription id), so a metric label on it stays bounded. `Engine.Registry()` names the registry an adapter mounts; `BatchTarget` and `FiltersTarget` are the two reserved targets as constants. Operations are a closed set too: an operation the socket does not know is reported as `other`. |
| D60 | The adapter surfaces (WP5; supersede section 2's and section 11's `RegisterRoutes(router, registry)`): `ginadapter.RegisterRoutes(router gin.IRoutes, &ginadapter.Options{Engine, MaximumBodyBytes}) error` mounts every route below `BasePath` (the engine names its registry, so the two cannot disagree; options validated and copied per DES-001; a route gin refuses is returned, not panicked), `ginadapter.SocketHandler(engine) gin.HandlerFunc` is the socket. A mutation's and an upload's static `Check` runs before the body is read (D8): a refused request reads no body and is answered by the engine, which runs the same check and reports the outcome. Mutations, batches and uploads (once the file is read) run on a context the client's disconnect does not cancel, so a write that started commits and is audited as what it did; presence routes are mounted only when the engine serves presence (`Engine.ServesPresence`); JSON bodies are bounded by `MaximumBodyBytes` (default 1 MiB), uploads by their `MaximumBytes`, a raw upload with a Content-Length streaming to the handler, a multipart part or a body without a length read into memory up to that bound. An export declares the trailer `Livewire-Export-Rows` (`ginadapter.ExportRowsTrailer`) with its headers and sets it, the written rows, only when it finished: a client takes a CSV without it as cut. A failure after the first byte also aborts the response (HTTP/1.1: the connection is closed under it; else a panic with `http.ErrAbortHandler`, which resets an HTTP/2 stream only behind a recovery middleware that lets it through; gin.Recovery swallows it and the stream ends cleanly, without the trailer). Outcomes are recorded on the request's wide event as `livewire.target`, `livewire.kind`, `livewire.read`, `livewire.status`, `livewire.code`, `livewire.rows`. `prometheusadapter.New(&Options{Registerer, Namespace})` registers the collectors on the application's registry and `Metrics.Observe` is the Observer that feeds them. |

## 2. Packages

| Package | Holds | Depends on |
|---------|-------|-----------|
| `livewire` | registry, declarations, params, errors, wire frames, diff, engine (socket handler on `net/http`), `ChangeFeed`, `FilterStore`, `IdempotencyStore`, `IdentitySource`, `Observer` | govengers/auth, gorilla/websocket, validator |
| `livewire/query` | field model, filter document parse/validate/canonical/hash, compile to SQL text and arguments, page and count builders, sort, relative time | stdlib |
| `livewire/ginadapter` | `RegisterRoutes(router, registry)`, socket handler as `gin.HandlerFunc`, wide-event recording | gin, wide-event-logger |
| `livewire/postgresadapter` | pgx LISTEN feed, trigger SQL, filter and idempotency stores, migration SQL constants | pgx/v5 |
| `livewire/prometheusadapter` | collectors for sockets, subscriptions, re-runs, queue drops, query wait | client_golang |
| `livewire/generate` | TypeScript, OpenAPI, Go client, CLI emitters; `runtime/` holds the TypeScript runtime sources | stdlib |
| `livewire/livewiretest` | contract gate helpers for app test suites | testing |
| `examples/todo` | one app: gin, auth password login, sqlc, goose, embedded-postgres, generated client, CLI | |
| `test/e2e` | two engines, one database, bun-driven runtime | |

## 3. Declarations

```go
registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
registry.Add(
    livewire.NewResource[part.Item]("parts", "/parts", &livewire.ResourceOptions[part.Item]{
        Tables: []string{"parts"},
        Read:   authorization.IsUser(),
        Fields: []query.Field{
            {Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
            {Name: "type", Type: query.TypeEnum, SQL: "parts.type", Enum: part.Types, Filter: true, Sort: true},
            {Name: "name", Type: query.TypeText, SQL: "parts.name", Filter: true, Sort: true, Search: true},
            {Name: "createdAt", Type: query.TypeTime, SQL: "parts.created_at", Sort: true, Nullable: false},
        },
        DefaultSort:  []query.SortTerm{{Field: "createdAt", Descending: true}},
        Scope:        partScope,           // func(ctx) (query.Predicate, error)
        MaximumLimit: 500,
        Item:         &livewire.ItemKey{Parameter: "id", Field: "id"},
        List:         partRepository.ListParts,   // func(ctx, *query.ListQuery) ([]part.Item, int, error)
        Get:          partRepository.GetPart,     // func(ctx, *query.ListQuery) (*part.Item, error)
        Distinct:     partRepository.DistinctParts, // func(ctx, field string, *query.ListQuery) ([]livewire.DistinctValue, error)
    }),
    livewire.NewList[Stat, StatParams]("stats", "/stats", &livewire.ListOptions{Tables: ..., Read: ..., Load: ...}),   // typed params, ListResponse
    livewire.NewObject[Dashboard, DashboardParams]("dashboard", "/dashboard", &livewire.ObjectOptions{...}),
    livewire.NewMutation[CreatePart, part.Item]("createPart", http.MethodPost, "/parts", &livewire.MutationOptions{
        Check: authorization.HasPermission("parts", auth.AccessWrite), Status: http.StatusCreated,
        ClientID: livewire.UUIDv7, Handle: partService.Create,
    }),
    livewire.NewUpload[UploadParams, part.Attachment]("uploadAttachment", "/parts/{id}/attachments", &livewire.UploadOptions{
        ContentTypes: []string{"image/jpeg", "image/png", "audio/webm"}, MaximumBytes: 20 << 20, Handle: ...,
    }),
)
```

- Paths use `{name}` (OpenAPI style); adapters translate.
- Params structs use `path:"id"` and `query:"status"` tags, embedded structs flattened; unsupported fields fail at `Add`.
- `NewResource` is the filterable kind: list, item, distinct routes, `filter`/`filterId`/`sort`/`limit`/`offset` parameters, stored filters. `NewList` and `NewObject` are typed-params reads. All three are live when `Tables` is set.
- `Check` is static (role, permission); the loader sees the identity in `ctx` and applies ownership. `Scope` narrows a filterable resource's rows and runs before filter and count on every read and re-run.
- Mutations: `Handle func(ctx, *P, *Req) (*Resp, error)`, body validated with `validate` tags (D44), `Idempotency-Key` read into `livewire.IdempotencyKeyFrom(ctx)`.
- Errors: a handler returns `*auth.Error` kinds or `livewire.NotFound`, `livewire.Conflict`, `livewire.InProgress`, `livewire.Invalid(fields)`, `livewire.ForbiddenField(name)`; `livewire.ResponseFor` maps `*query.Error` to 400 with its code and everything unknown to 500 without leaking text. One body `{code, message, fields?}`; closed code set: invalid_request, invalid_filter, invalid_sort, invalid_page, invalid_cursor, forbidden_field, unauthenticated, forbidden, not_found, conflict, in_progress, internal.

## 4. Wire protocol (socket)

Request `{id, op, target, params, body?, idempotencyKey?}`; response `{id, ok, status, payload | error, subscription?, filterId?}`; push `{sub, seq, kind, payload}` with kinds `snapshot`, `diff`, `replace`, `error`. A payload is byte-for-byte the REST body. Diff: `{upserts, removes, order?, total?}`. Ops: `query` (one read), `subscribe`, `unsubscribe`, `call` (a mutation; uploads are REST only), `ping`. Close codes: 1000 normal, 1013 too slow, 4401 identity gone.

## 5. Live engine

- One `ChangeFeed` per replica (`postgresadapter.NewListener`): LISTEN with reconnect; every successful connect, the first included, triggers a resync of all subscriptions.
- A change names table, op, id, partition. Subscriptions index by table and partition; a change marks matching ones dirty; a dirty subscription re-runs once, changes during a re-run collapse into one follow-up.
- Re-run: identity via `IdentitySource` (cached per socket for the tick interval), `Check`, `Scope`, load, then snapshot on first push or resync, else diff (lists) or replace (objects). 5xx retries with backoff, 4xx ends with an `error` push.
- Time-dependent subscriptions (relative times) re-run on a safety tick (default 30 s).
- Query semaphore gates loads and identity checks. A panicking loader costs that subscription one retry, never the process.
- `Engine.Close(ctx)` ends every socket with 1001 and waits for goroutines.

## 6. Security model

| Concern | Mechanism |
|---------|-----------|
| Who | `auth.Identity` from the upgrade request (behind `Guard.RequireAPI` or an API-key resolver); re-validated by `IdentitySource` on every op, re-run and tick (D6). |
| Whether | `Check` (static) then loader or handler (data-dependent). Same code on both transports. |
| Impersonation | checks see the effective identity; `Observer` outcomes carry `identity.Real()`; `Real(...)` checks work unchanged. |
| Revocation | 4401 when gone; rights change re-runs (D7). `Engine.RevalidateAll()` and `RevalidateOwner(key)` for push-driven revocation. |
| CSRF on socket writes | strict Origin allowlist (D9); cookie sockets without an allowed Origin are refused at upgrade. |
| Abuse | per-socket and per-engine limits (D10), filter document limits (D12), `MaximumLimit`, `CountCap`, distinct cap, upload size. |
| Leakage | only declared fields filter or sort; 5xx text never leaves; `Reason` of refusals stays in the Observer. |
| Replay | idempotency records are owner-scoped: a key of another owner is a fresh key. |

## 7. The mutation transaction

```
engine.call(mutation)
  identity, checks (static, field write checks D39, unknown fields D40)
  transaction := Transactor.Begin(ctx)            // SET LOCAL livewire.actor = {effective, real, mutation, requestId}
  reserve idempotency key (owner, key)           // unique violation: replay the recorded response, or 409 in_progress
  response, err := handler(ctx with transaction, params, body)
  audit row (D32)
  record idempotency response
  commit
  Observer AfterCommit outcome; live subscriptions learn of the change through the NOTIFY trigger
```

Repositories:

```go
func (r *Repository) CreatePart(ctx context.Context, p CreatePartParameters) (*part.Item, error) {
    database := postgresadapter.TransactionFrom(ctx) // the engine's transaction, reads included (D45, D53)
    if database == nil {
        database = r.pool // outside an engine unit of work
    }
    row, err := sqlc.New(database).InsertPart(ctx, toInsert(p))
    return toItem(row), mapError(err)
}
```

`postgresadapter.Transactor` begins the transaction and keeps the commit handle for the engine; the context carries a query-only handle (D45). A service that needs a reaction (send mail, start a workflow) enqueues a job on the same transaction.

## 8. Filterable reads in the app

```go
func (r *Repository) ListParts(ctx context.Context, listQuery *query.ListQuery) ([]part.Item, int, error) {
    page, err := postgresadapter.ListPage(ctx, r.pool, partSQL, listQuery, func(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]part.Item, error) {
        found, err := sqlc.New(database).ListPartsByIDs(ctx, postgresadapter.PageIDs(rows))
        return toItemsByID(found), err
    })
    if err != nil {
        return nil, 0, err
    }
    return page.Items, page.Total, nil
}
```

`postgresadapter.ListPage` (D53): one `REPEATABLE READ` read-only snapshot (the engine's transaction inside an engine unit of work, even when the pool is passed, else one of its own), the page query (ids, derived columns), the count (capped), the hydration by ids in page order. `partSQL` is a `query.ResourceSQL{From, IDColumn, Derived}`.

## 9. Generated artifacts

- TypeScript: `types.ts`, `filters.ts` (per-resource field unions and condition types), `api.ts` (REST functions, `live` targets, `call` wrappers), `runtime.ts` (fetch, `ApiError`, `NetworkError`, `LiveConnection`, idempotency keys, UUID v7), `vue.ts` (`useLive`, `useLiveList`, `useLiveStatus`). Type-checked with `strict`, `exactOptionalPropertyTypes`, `noUncheckedIndexedAccess`; runtime unit-tested under bun.
- OpenAPI 3.0.3: every route, `ListResponse` schemas, filter document schema per resource, `x-websocket: {target, ops}` on live operations, security scheme from options.
- Go client: package `<app>client`, imports the app's public DTO package, `Client{BaseURL, APIKey, HTTP}`; one method per declaration; `Subscribe` returns a channel of typed pushes; filter types mirror `filters.ts`.
- CLI: urfave/cli v3, one command per declaration, params as flags, body from `--body` JSON or stdin, `--filter` JSON, `subscribe` streams NDJSON, `--api-key` or `<APP>_API_KEY`.
- All output carries `Code generated by livewire. DO NOT EDIT.` and is committed; `livewiretest.GeneratedIsCurrent` fails the app's tests on drift.

## 10. Work packages

| WP | Delivers | Gate |
|----|----------|------|
| WP0 | repo skeleton: module rename, `.govengers.yml`, `.golangci.yml`, justfile, org CI, docs stubs | `just check` green on empty packages |
| WP1 | `query` package: fields, document, validate, canonical, hash, compile, page/count builders, sort, relative time | table tests, 90 % coverage |
| WP2 | core registry and declarations, params, errors, wire frames, diff, `ListResponse`, client IDs | unit tests |
| WP3 | engine on net/http: sockets, ops, subscriptions, re-run, limits, identity source, observer, revalidation | embedded-postgres tests, race |
| WP4 | `postgresadapter`: listener, trigger SQL, transactor, audit, filter store, idempotency store, history SQL, `ListPage`, cursor mode, export | embedded-postgres tests |
| WP5 | `ginadapter`, `prometheusadapter`, uploads, batch, presence | tests |
| WP6 | `generate`: TypeScript + runtime + vue, OpenAPI 3.0.3 | tsc gate, bun unit tests |
| WP7 | `generate`: Go client, CLI | compiled against `examples/todo` |
| WP8 | `livewiretest`, `examples/todo`, `test/e2e` (two engines, bun runtime) | e2e green in CI |
| WP9 | docs, llms.txt, migration table, v1.0.0 | dual review |
| WP10 | `xlsxadapter` | optional, cut first |

Each WP: own branch, `just check` green, dual review (Codex and Fable) before merge.

## 11. Migration from mxcd/livewire v0.2.x

| v0.2 | v1 |
|------|----|
| `livewire.Registry{}` + hooks | `NewRegistry(options)`; `Context` hook gone (resolve in the loader), `Errors`/`RenderError` gone (`auth.ResponseFor`) |
| `NewList(name, path, tables, read, load)` | `NewList[T, P](name, path, &ListOptions{...})`; returns `ListResponse` |
| `NewObject` | unchanged shape, options struct |
| `NewMutation(...)` | `NewMutation[Req, Resp](name, method, path, &MutationOptions{...})`; params struct in options |
| `Registry.Mount(group)` | `ginadapter.RegisterRoutes(router, registry)` |
| `NewEngine(&EngineOptions{DSN})` + `Run` | `NewEngine(&EngineOptions{Registry, ChangeFeed, IdentitySource, AllowedOrigins, Observer})`, `Run(ctx)`, `Close(ctx)` |
| `InstallTriggers` | `postgresadapter.TriggerSQL(tables...)` in a goose migration |
| `CloseWhere` | `RevalidateOwner`, `RevalidateAll` |
| `gen.Generate` | `generate.Write(registry, &generate.Options{...})` |
| `useLive(target, params, {cache})` | same, cache keyed by identity |
