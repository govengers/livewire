# Testing

`livewiretest` holds the contract gates an application's test suite runs (D23, D70). Each drives the application as its clients do: `livewiretest.Application{Engine, Handler, SocketPath}` is the application's own router, with its authentication, its REST routes and its socket, served on a local test server, and an `Authenticate func(*http.Request)` makes every request and the socket's upgrade the fixture identity's (an API key header, a session cookie). Run them on the wiring `main` builds, on a real database: [examples/todo/cmd/todo](../../examples/todo/cmd/todo) runs `newApplication` on embedded PostgreSQL, one server per test binary and a database per test cloned from a migrated template.

## GeneratedIsCurrent

Renders every artifact of the registry in memory with the generator's options and fails for every committed file that is missing or differs, with the lines that differ. `GeneratedIsCurrentStrict` also fails on a file in the output directories that the registry no longer generates, for directories that belong to the generator.

<!-- snippet: examples/todo/cmd/todo/gates_test.go#generated-current -->
```go
func TestGeneratedClientsAreCurrent(t *testing.T) {
    t.Parallel()
    application := newTestApplication(t)
    livewiretest.GeneratedIsCurrentStrict(t, application.registry, generated.Options("../.."))
    contract, err := controller.Contract()
    require.NoError(t, err)
    livewiretest.GeneratedIsCurrentStrict(t, contract, generated.Options("../..")) // what cmd/generate writes from
}
```

- Catches: an API change without a regenerated client, a hand edit of a generated file, a leftover client file (strict).
- Cannot catch: a client the application does not generate with these options; whether the clients behave (their own tests and the end-to-end suite do).

## Parity

For every read kind of every declaration (presence reads when the engine serves them), the REST body must equal the socket query's payload and, for a live read, the subscribe's first push, byte for byte. `Reads` gives the parameters of the reads that need some; a read that needs them and has none fails the gate, as does an answer other than 200.

<!-- snippet: examples/todo/cmd/todo/gates_test.go#parity -->
```go
func TestRESTAndSocketAnswerTheSameBytes(t *testing.T) {
    t.Parallel()
    application := newTestApplication(t)
    todo := seedParity(t, application)
    livewiretest.Parity(t, &livewiretest.ParityOptions{
        Application:  application.gateApplication(),
        Authenticate: application.session(t, "alice"), // the browser's way; the gates below use an API key
        Reads:        reads(todo),
    })
}
```

- Catches: a REST route or socket path that answers differently, a middleware that rewrites one transport's bodies, a read that fails for the fixture identity.
- Cannot catch: reads of identities other than the fixture's, or data the fixture does not seed; give it rows for every read.

## Stale

Subscribes to every live read (presence aside, whose table is the library's), runs the writes one after another, and after each requires every subscription's state, its pushes applied, to reach its REST read within the timeout (5 s), naming the declaration that stays behind. It checks state, not re-run counts: a re-run that changes nothing pushes nothing.

<!-- snippet: examples/todo/cmd/todo/gates_test.go#stale -->
```go
func TestEveryWriteReachesTheSubscriptionsOfItsTables(t *testing.T) {
    t.Parallel()
    application := newTestApplication(t)
    key := application.apiKey(t, "alice")
    todos := application.client(t, "alice")
    todo := fixtureTodo(t, todos, "stale")
    ctx := context.Background()
    livewiretest.Stale(t, &livewiretest.StaleOptions{
        Application:  application.gateApplication(),
        Authenticate: bearer(key),
        Reads:        reads(todo)[:3], // the presence reads are the library's own table
        Writes: []livewiretest.Write{
            {Table: "todos", Commit: func(ctx context.Context) error {
                _, err := todos.CreateTodo(ctx, api.CreateTodoRequest{Title: "another"}, nil)
                return err
            }},
            {Table: "attachments", Commit: func(ctx context.Context) error {
                _, err := todos.UploadAttachment(ctx, todoclient.UploadAttachmentParameters{ID: todo.ID}, textFile("notes"), nil)
                return err
            }},
            {Table: "activity", Commit: func(context.Context) error {
                // the completion enqueues the job in the mutation's transaction; the job writes the activity
                if _, err := todos.UpdateTodo(ctx, todoclient.UpdateTodoParameters{ID: todo.ID}, api.UpdateTodoRequest{Done: new(true)}, nil); err != nil {
                    return err
                }
                jobstest.Drain(t, application.worker)
                return nil
            }},
        },
    })
}
```

- Every table a live declaration lists needs a write, and a write to such a table that changes no read of a declaration listing it fails: it would prove nothing.
- Catches: a loader that reads a table its declaration does not list (give a write of that table: it may change nothing, but what it changes must re-run), a table without `TriggerSQL`, a change feed that does not reach the engine.
- Cannot catch: a table no write writes. No gate can find a table a loader reads that neither its declaration nor a write names; give writes of every table the loaders read.

## TenantIsolation

Reads every filterable resource as two tenants over REST and fails when anything of one reaches the other (D42, DAT-017): a listed row of the other tenant (paging past a `CountCap`), an item or history read of a foreign row that answers anything but 404 or 403, an own item read that answers another row, a foreign version in an own history, an export row that is not its own, distinct values or counts that are not its own rows'. A resource of which a tenant reads no row fails too.

<!-- snippet: examples/todo/cmd/todo/gates_test.go#tenant-isolation -->
```go
func TestWorkspacesNeverSeeEachOthersTodos(t *testing.T) {
    t.Parallel()
    application := newTestApplication(t)
    fixtureTodo(t, application.client(t, "alice"), "north")
    fixtureTodo(t, application.client(t, "bob"), "south")
    livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
        Application: application.gateApplication(),
        Tenants: [2]livewiretest.Tenant{
            {Name: "north", Authenticate: bearer(application.apiKey(t, "alice"))},
            {Name: "south", Authenticate: bearer(application.apiKey(t, "bob"))},
        },
    })
}
```

- Rows are keyed by the item key (else `id`); a row without one fails. `Shared` names the rows both tenants may read on purpose (reference data).
- Cannot catch: typed lists and objects, whose loaders it does not know to call per tenant; writes; tenants beyond the two it is given.

## The end-to-end pattern

[test/e2e](../../test/e2e) (build tag `e2e`, `just test-e2e`) runs two engines on one embedded PostgreSQL, each with its own pool, `Listener` and server, as two replicas of one application, and drives them with the Go client and the generated TypeScript under bun: a write on one engine re-runs a subscription on the other, an idempotency key replays across them, an evicted filter is recovered, a re-created item starts a new history, a batch names each call in the history, presence crosses engines, every call leaves an audit row, the export streams with its trailer, the Go client reconnects across a deploy, and the TypeScript pair survives a re-login. An application proves its fan-out the same way: two `newApplication`s on one database, a subscription on one, a write on the other. Without bun the TypeScript parts skip, unless `LIVEWIRE_REQUIRE_TYPESCRIPT=1` (set in CI).

A job a mutation enqueued runs in a test with `jobstest.Drain(t, worker)` (govengers/jobs).

## Breaking changes of the API

`just openapi-breaking` runs `oasdiff breaking` (pinned, through `go run`) on the example's `openapi.yaml` against the one of the latest release tag (`v1` or later) before `HEAD`, or of a ref it is given (`just openapi-breaking v1.0.0`), and fails on an error-level change (D23). An application copies the recipe for its own document. Until a v1 tag exists there is nothing to compare with, so it is not part of `just check` or CI yet; once v1.0.0 is tagged, CI can run it.
