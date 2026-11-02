# livewire

[![CI](https://github.com/govengers/livewire/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/govengers/livewire/actions/workflows/ci.yml)
[![unit tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/livewire/badges/unit-tests.json)](https://github.com/govengers/livewire/actions/workflows/ci.yml)
[![e2e tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/livewire/badges/e2e-tests.json)](test/e2e)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/livewire/badges/coverage.json)](https://github.com/govengers/livewire/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/govengers/livewire.svg)](https://pkg.go.dev/github.com/govengers/livewire)
[![License](https://img.shields.io/github/license/govengers/livewire)](LICENSE)

Coding agents: start at [llms.txt](llms.txt) and [docs/wiki/Home.md](docs/wiki/Home.md); the tested reference application is [examples/todo](examples/todo).

Declare an API once in Go and serve it as REST routes on gin, as a WebSocket with reads, live subscriptions and writes fed by PostgreSQL `LISTEN/NOTIFY` (a committed write re-runs every subscription that reads the table it changed), as generated clients (TypeScript with Vue composables, Go, a CLI) and as an OpenAPI 3.0.3 document.

The registry is the single source of truth; govengers/auth decides who may do what, the same way on both transports. Filterable resources come with filter documents, stored filters, distinct values, cursor pages, item history and a CSV export. Every mutation runs in a transaction the engine owns, with an idempotency record and an audit row when the engine has the `IdempotencyStore` and the `AuditStore`. Module `github.com/govengers/livewire`, Go 1.26, PostgreSQL 18, Apache 2.0; it succeeds `github.com/mxcd/livewire` v0.2.x ([migration](docs/wiki/Migration.md)).

```sh
go get github.com/govengers/livewire
```

## Example

From [example_application_test.go](example_application_test.go), a live, filterable resource and a create:

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
            ClientID: livewire.UUIDv7, Effect: livewire.Upserts("todos"), // a socket call shows it in the lists at once
            Handle: todos.Create,
        }),
    )
}
```

The engine on the `Transactor` and `Listener` of `postgresadapter`, behind govengers/auth on gin:

<!-- snippet: example_application_test.go#serve -->
```go
engine, err := livewire.NewEngine(&livewire.EngineOptions{
    Registry: registry, ChangeFeed: listener, Transactor: transactor,
    IdentitySource: guard, // re-validates the session of every socket (D6)
    AllowedOrigins: []string{"https://todo.example.com"},
    // IdempotencyStore, AuditStore, FilterStore, PresenceStore: each feature is off without its store
})
if err != nil {
    return err
}
api := router.Group("", authginadapter.Middleware(guard.RequireAPI())) // the identity of every request
if err := ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: engine}); err != nil {
    return err
}
api.GET("/api/v1/socket", ginadapter.SocketHandler(engine))
err = engine.Run(ctx) // the change feed and the ticks, until ctx ends
closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
defer cancel()
return errors.Join(err, engine.Close(closing)) // every socket ends with 1001
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

`generate.Write(registry, options)` writes them, and `livewiretest.GeneratedIsCurrent` fails the tests while a committed file is behind. Triggers and tables come from exported SQL in the application's migrations (`postgresadapter.TriggerFunctionSQL`, then `TriggerSQL("todos")`).

The packages, what each depends on and which wiki page covers it: [docs/wiki/Home.md](docs/wiki/Home.md#packages).

## Identity

A client serves one identity for its whole life. The TypeScript `Client` (REST) and `LiveConnection` (socket) are a pair (`createLivePair`) that never switches: another identity, a 4401 close or `end()` ends both, and every open request fails with an `EndedError`. Neither sees the session cookie, so the application ends the pair before the request that changes the session (login, logout, switch), then makes the next one and puts it into the ref it gave `livePlugin`, which the Vue composables follow ([details](docs/wiki/Generated-Clients.md#one-pair-per-identity)). A Go `client.Client` serves one API key.

## Documentation

- [docs/wiki](docs/wiki/Home.md): declarations, reads and filters, the live engine, Postgres, REST and metrics, the generated clients, testing, migration from v0.2.x.
- [examples/todo](examples/todo): a service on gin, govengers/auth, sqlc, goose and govengers/jobs, with the gates, the generated clients and the CLI in its tests.
- [docs/design.md](docs/design.md): the binding design, decisions D1 onwards.
- [pkg.go.dev](https://pkg.go.dev/github.com/govengers/livewire): the API.

The repository follows [govengers/standards](https://github.com/govengers/standards), profile `library`. `just check` runs lint, the unit tests, the two-engine end-to-end suite and govulncheck.

## License

Apache 2.0, see [LICENSE](LICENSE).
