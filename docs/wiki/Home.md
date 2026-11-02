# govengers/livewire

Declare an API once in Go; serve it as REST, as a live WebSocket and to generated clients. Module `github.com/govengers/livewire`, Go 1.26, PostgreSQL 18, Apache 2.0.

## Model

A **registry** (`livewire.NewRegistry`) holds every **declaration** of an API by name. There are five kinds: a **resource** is a filterable list with item, distinct and history reads and an export; a **list** and an **object** are reads with typed parameters; a **mutation** is a write with a JSON body; an **upload** is a binary write. A read is **live** when it names the `Tables` its loader reads. The **engine** (`livewire.NewEngine`) runs every read and write for both transports: the REST adapter calls `Engine.Read`, `Call`, `Batch`, `Upload` and `Export`, the socket's ops run the same code, so a REST body is the socket's payload byte for byte. A **change feed** (the Postgres `Listener`) tells the engine which table changed; the engine re-runs the subscriptions of that table and pushes what changed. The **generator** turns the registry into the TypeScript client, the OpenAPI document, the Go client and the CLI.

Authorization is govengers/auth: `auth.IdentityFrom(ctx)` is the identity, `authorization.Check` the check type. livewire never logs; every outcome goes to the registry's `Observer`.

## Packages

| Import `github.com/govengers/livewire/...` | Use it for | Imports besides the standard library | Page |
|---|---|---|---|
| (root) `livewire` | the registry, declarations, errors, the engine on `net/http`, the adapter contracts | `query`, govengers/auth, gorilla/websocket, go-playground/validator, google/uuid | [Declarations](Declarations.md), [Reads-and-Filters](Reads-and-Filters.md), [Live-Engine](Live-Engine.md) |
| `query` | fields, filter documents, SQL compilation | none | [Reads-and-Filters](Reads-and-Filters.md) |
| `postgresadapter` | migrations, change feed, transactor, stores, list, history and export helpers | the root, `query`, pgx/v5, google/uuid | [Postgres](Postgres.md) |
| `ginadapter` | the REST routes and the socket on gin, wide events | the root, gin, wide-event-logger | [REST-and-Metrics](REST-and-Metrics.md) |
| `prometheusadapter` | the engine's metrics | the root, client_golang | [REST-and-Metrics](REST-and-Metrics.md) |
| `generate` | the TypeScript, OpenAPI, Go client and CLI generators | the root, `query`, google/uuid | [Generated-Clients](Generated-Clients.md) |
| `client` | the runtime of the generated Go client and CLI | the root, `query`, gorilla/websocket, google/uuid | [Generated-Clients](Generated-Clients.md) |
| `livewiretest` | the contract gates | the root, `query`, `generate`, gorilla/websocket | [Testing](Testing.md) |

## Wiring order in main

[examples/todo](../../examples/todo) does each step; [cmd/todo/application.go](../../examples/todo/cmd/todo/application.go) is the order.

1. Open the pool and run the goose migrations, which execute livewire's SQL ([Postgres](Postgres.md#migrations)).
2. Build the repositories: sqlc on the engine's transaction, the pool outside one.
3. Build livewire's Postgres adapters: `Transactor`, `Listener` and the stores the features need.
4. Build the services, then the registry with its `Tenant` function and its `Observer` (`prometheusadapter.Metrics.Observe` and one wide event per socket outcome).
5. `livewire.NewEngine` with the registry, the adapters, the `IdentitySource` (`session.Guard` for sessions, or one that also resolves API keys, as the todo app's `authentication` package does) and the allowed origins.
6. The govengers/jobs worker with the maintenance schedules of the stores.
7. The gin router: the authentication middleware, `ginadapter.RegisterRoutes`, `ginadapter.SocketHandler` behind the same middleware.
8. Run `engine.Run`, the worker and the servers in one errgroup; on SIGTERM withdraw readiness, shut the server down, then `engine.Close(ctx)` ends every socket with 1001.

## Pages

- [Declarations](Declarations.md): the five kinds, fields, column checks, client ids, optimistic effects, tenant.
- [Reads-and-Filters](Reads-and-Filters.md): filter documents, stored filters, distinct, cursor mode, history, export.
- [Live-Engine](Live-Engine.md): subscriptions, re-runs, identity re-validation, presence, limits, the Observer.
- [Postgres](Postgres.md): migrations and deploy order, the transactor, the stores and their sweeps, `ListPage` and friends, Row Level Security.
- [REST-and-Metrics](REST-and-Metrics.md): the routes, uploads, the export trailer, wide events, Prometheus.
- [Generated-Clients](Generated-Clients.md): `generate.Write`, TypeScript and Vue, OpenAPI, the Go client, the CLI.
- [Testing](Testing.md): the four gates and the end-to-end pattern.
- [Migration](Migration.md): from `github.com/mxcd/livewire` v0.2.x.

Reference: [README](../../README.md) (overview), [design](../design.md) (decisions), [pkg.go.dev](https://pkg.go.dev/github.com/govengers/livewire) (API).
