# livewire

[![CI](https://github.com/govengers/livewire/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/govengers/livewire/actions/workflows/ci.yml)
[![unit tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/livewire/badges/unit-tests.json)](https://github.com/govengers/livewire/actions/workflows/ci.yml)
[![e2e tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/livewire/badges/e2e-tests.json)](test/e2e)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/livewire/badges/coverage.json)](https://github.com/govengers/livewire/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/govengers/livewire.svg)](https://pkg.go.dev/github.com/govengers/livewire)
[![License](https://img.shields.io/github/license/govengers/livewire)](LICENSE)

Coding agents: start at [llms.txt](llms.txt) and [docs/wiki/Home.md](docs/wiki/Home.md); the tested reference application is [examples/todo](examples/todo).

Declare an API once in Go and serve it as:

- REST routes on gin,
- a WebSocket with reads, live subscriptions and writes, fed by PostgreSQL `LISTEN/NOTIFY`: a committed write re-runs every subscription that reads the table it changed,
- generated clients: TypeScript with Vue composables, a Go client and a command-line client,
- an OpenAPI 3.0.3 document.

The registry is the single source of truth. govengers/auth decides who may do what, the same way on both transports. Filterable resources come with filter documents, stored filters, distinct values, cursor pages, item history and a CSV export; every mutation runs in a transaction the engine owns, with an audit row and an idempotency record. Module `github.com/govengers/livewire`, Go 1.26, PostgreSQL 18, Apache 2.0; it succeeds `github.com/mxcd/livewire` v0.2.x ([migration](docs/wiki/Migration.md)).

```sh
go get github.com/govengers/livewire
```

## Example

A filterable, live resource and a create with an optimistic effect, from [example_application_test.go](example_application_test.go):

<!-- snippet: example_application_test.go#declare -->
```go
func declare(todos *todoRepository) (*livewire.Registry, error) {
    registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
    if err != nil {
        return nil, err
    }
    return registry, registry.Add( // a declaration mistake fails here, at startup (D43)
        livewire.NewResource("todos", "/todos", &livewire.ResourceOptions[todo]{
            Tables: []string{"todos"}, // a committed write to todos re-runs every live read of it
            Read:   authorization.IsUser(),
            Fields: query.Fields{
                {Name: "title", Type: query.TypeText, SQL: "todos.title", Filter: true, Sort: true, Search: true},
                {Name: "done", Type: query.TypeBoolean, SQL: "todos.done", Filter: true, Sort: true},
                {Name: "createdAt", Type: query.TypeTime, SQL: "todos.created_at", Filter: true, Sort: true},
            },
            DefaultSort: []query.SortTerm{{Field: "createdAt", Descending: true}},
            List:        todos.List,
        }),
        livewire.NewMutation("createTodo", http.MethodPost, "/todos", &livewire.MutationOptions[livewire.NoParameters, createTodo, todo]{
            Check: authorization.IsUser(), Status: http.StatusCreated,
            ClientID: livewire.UUIDv7, Effect: livewire.Upserts("todos"), // the client shows it at once
            Handle: todos.Create,
        }),
    )
}
```

The engine on PostgreSQL behind govengers/auth, served on gin:

<!-- snippet: example_application_test.go#serve -->
```go
func serve(ctx context.Context, pool *pgxpool.Pool, databaseURL string, guard *session.Guard, router gin.IRouter) error {
    registry, err := declare(&todoRepository{pool: pool})
    if err != nil {
        return err
    }
    transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
    if err != nil {
        return err
    }
    listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: databaseURL})
    if err != nil {
        return err
    }
    engine, err := livewire.NewEngine(&livewire.EngineOptions{
        Registry: registry, ChangeFeed: listener, Transactor: transactor,
        IdentitySource: guard, // re-validates the session of every socket (D6)
        AllowedOrigins: []string{"https://todo.example.com"},
    })
    if err != nil {
        return err
    }
    api := router.Group("", authginadapter.Middleware(guard.RequireAPI())) // the identity of every request
    if err := ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: engine}); err != nil {
        return err
    }
    api.GET("/api/v1/socket", ginadapter.SocketHandler(engine))
    return engine.Run(ctx) // the change feed and the ticks; on shutdown engine.Close(ctx) ends every socket
}
```

The clients, from a generator command the application runs after an API change:

<!-- snippet: example_application_test.go#generate -->
```go
options := &generate.Options{
    TypeScriptDirectory: "web/src/api", // types.ts, filters.ts, api.ts, runtime.ts, vue.ts
    OpenAPIFile:         "openapi.yaml",
    OpenAPI:             generate.OpenAPIOptions{Title: "Todo", Version: "1.0.0"},
}
```

`generate.Write(registry, options)` writes them; `livewiretest.GeneratedIsCurrent` fails the application's tests while a committed file is behind. The triggers and tables come from exported SQL in the application's own migrations (`postgresadapter.TriggerFunctionSQL`, then `postgresadapter.TriggerSQL("todos")`); nothing runs DDL at startup.

## Packages

| Import `github.com/govengers/livewire/...` | Holds | Depends on |
|---|---|---|
| (root) `livewire` | registry, the five declaration kinds, errors, wire frames, the engine on `net/http`, the adapter contracts | govengers/auth, gorilla/websocket, validator |
| `query` | fields, filter documents, canonical form, sort, pages, cursors, SQL compilation | standard library |
| `postgresadapter` | migration SQL, LISTEN feed, transactor, filter, idempotency, audit and presence stores, `ListPage`, `HistoryPage`, `Distinct`, `WriteCSV` | pgx/v5 |
| `ginadapter` | every REST route, the socket handler, wide-event recording | gin, wide-event-logger |
| `prometheusadapter` | the engine's metrics on the application's `prometheus.Registerer` | client_golang |
| `generate` | TypeScript, Vue, OpenAPI, Go client and CLI generators | standard library |
| `client` | the runtime of the generated Go client and CLI | gorilla/websocket |
| `livewiretest` | the contract gates of an application's test suite | testing |

## Identity

A client serves one identity for its whole life. The TypeScript `Client` (REST) and `LiveConnection` (socket) are made as a pair for the identity the session has (`createLivePair`) and never switch: another identity, a 4401 close or `end()` ends them for good, and every open request fails with an `EndedError`. Neither can see a session cookie, so the application ends the pair before the request that changes the session (login, logout, switch), makes the next pair once the session answers, and puts it into the reference it gave `livePlugin`; `useLive`, `useLiveList`, `useLiveStatus` and `useLivePresence` follow it. A Go `client.Client` serves the identity of its API key. Details: [Generated clients](docs/wiki/Generated-Clients.md#one-pair-per-identity).

## Documentation

- [docs/wiki](docs/wiki/Home.md): declarations, reads and filters, the live engine, Postgres, REST and metrics, the generated clients, testing, migration from v0.2.x.
- [examples/todo](examples/todo): a service wired as the standards want it (gin, govengers/auth, sqlc, goose, govengers/jobs), with the contract gates, the generated clients and the CLI in its tests.
- [docs/design.md](docs/design.md): the binding design, decisions D1 onwards.
- [pkg.go.dev](https://pkg.go.dev/github.com/govengers/livewire): the API.

The repository follows [govengers/standards](https://github.com/govengers/standards), profile `library`. `just check` runs lint, the unit tests, the two-engine end-to-end suite and govulncheck.

## License

Apache 2.0, see [LICENSE](LICENSE).
