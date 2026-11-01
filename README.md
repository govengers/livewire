# livewire

Coding agents: start at [llms.txt](llms.txt) and [docs/design.md](docs/design.md).

Declare an API once in Go and serve it three ways: as REST routes, as a WebSocket with reads, live subscriptions and writes fed by PostgreSQL `LISTEN/NOTIFY`, and as generated clients (TypeScript with Vue composables, Go, a CLI) plus an OpenAPI 3.0.3 document. The registry is the single source of truth; govengers/auth decides who may do what, on both transports.

**v1 is being built on `feat/v1`** after the design in [docs/design.md](docs/design.md). The last release of the previous generation is `github.com/mxcd/livewire` v0.2.2; the migration table is in the design's section 11.

| Package | What it gives you | Depends on |
|---------|-------------------|------------|
| `livewire` | registry, declarations, params, errors, wire frames, the live engine on `net/http` | govengers/auth, gorilla/websocket |
| `livewire/query` | filter document model, validation, canonical form, SQL compilation, page and count builders | standard library |
| `livewire/ginadapter` | REST routes and the socket handler on gin, wide-event recording | gin, wide-event-logger |
| `livewire/postgresadapter` | LISTEN feed, trigger and table SQL, transactor, audit, filter, idempotency and presence stores | pgx/v5 |
| `livewire/prometheusadapter` | engine metrics on a `prometheus.Registerer` | client_golang |
| `livewire/generate` | TypeScript, OpenAPI, Go client and CLI emitters | standard library |
| `livewire/client` | the runtime the generated Go client and CLI stand on: REST with an API key, one socket for every subscription | gorilla/websocket |
| `livewire/livewiretest` | contract gates for application test suites | testing |

```sh
go get github.com/govengers/livewire
```

## License

MIT
