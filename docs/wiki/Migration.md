# Migrating from mxcd/livewire v0.2.x

v1 is a new module, `github.com/govengers/livewire`, with the same idea and a new API: declarations with options structs, a core on `net/http` with gin, pgx and Prometheus in adapters, govengers/auth for every identity and check, an engine-owned transaction for every write, and generated Go and command-line clients next to the TypeScript one. The license changes from MIT to Apache 2.0. This table maps every v0.2.2 concept; **breaking** marks a change a client or the database sees, not only the Go code.

## Module and packages

| v0.2 | v1 |
|---|---|
| `github.com/mxcd/livewire`, one package on gin | `github.com/govengers/livewire` (root on `net/http`), `ginadapter`, `postgresadapter`, `prometheusadapter`, `query`, `generate`, `client`, `livewiretest` |
| `github.com/mxcd/livewire/gen` | `github.com/govengers/livewire/generate` |
| `livewire.Check` (`func(ctx) error`) | `authorization.Check` of govengers/auth, the same shape; `auth.IdentityFrom(ctx)` is the identity |

## Registry

| v0.2 | v1 |
|---|---|
| `&livewire.Registry{}` | `livewire.NewRegistry(&livewire.RegistryOptions{BasePath, Tenant, Observer})` |
| `Registry.Add(items ...any)`, panicking on a duplicate | `Registry.Add(declarations ...livewire.Declaration) error`: every mistake is returned, nothing is added then |
| `Registry.Context` hook | gone (D8). The tenant is `RegistryOptions.Tenant` ([Declarations](Declarations.md#tenant)); ownership and other data checks go into the loader, the handler or `Scope` |
| `Registry.Errors` | gone: return `livewire.NotFound()`, `Conflict()`, `Invalid(fields)`, `ForbiddenField(name)`, `InProgress()` or an `*auth.Error`; `livewire.ResponseFor` maps any error |
| `Registry.RenderError` | gone: every REST refusal is `ResponseFor`'s status and body |
| `Registry.Mount(group)` | `ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine: engine})`; the base path is `RegistryOptions.BasePath` |
| `Registry.Resource(name)` | `Registry.Lookup(name)` |

## Declarations

| v0.2 | v1 |
|---|---|
| `NewList(name, path, tables, read, load)` | `NewList(name, path, &livewire.ListOptions[Item, Parameters]{Tables, Read, Load, Guarded, Partition})`. **Breaking:** the body is `{limit, offset, total, items}`, not the bare array |
| `NewObject(name, path, tables, read, load)` | `NewObject(name, path, &livewire.ObjectOptions[Item, Parameters]{Tables, Read, Load, Guarded, Partition})` |
| (none) | `NewResource`: a filterable list with item, distinct and history reads, an export, stored filters and cursor pages ([Reads-and-Filters](Reads-and-Filters.md)) |
| `NewMutation(name, method, path, check, handle)` | `NewMutation(name, method, path, &livewire.MutationOptions[Parameters, Request, Response]{Check, Handle, Status, ClientID, Resource, FieldChecks, Effect})`; also on the socket (`call`, `batch`) |
| `mutation.WithStatus(code)` | `MutationOptions.Status` |
| `.Use(middleware...)` on a declaration | gone (D8): `Check` is the static gate on both transports, data checks are in the loader or handler; route middleware goes on the gin group |
| (none) | `NewUpload`: a binary write, REST only |
| `livewire.NoParams`, `NoBody`, `NoContent` | `livewire.NoParameters`, `NoBody`, `NoContent` |
| `path:"id"`, `query:"status"`, `enum:"a,b"` tags; `?status=a&status=b` or `a,b` | unchanged |
| a bad parameters struct panics at declaration | `Registry.Add` returns it |
| `binding` tags (gin's validator) | **breaking:** `validate` tags (go-playground/validator, D44); field errors are named by JSON path |
| unknown body members ignored | **breaking:** refused with 400 `invalid_request` naming the member (D40) |
| (none) | column checks (`query.Field.Read`, `Write`, `FieldChecks`, `Guarded`), client ids (`ClientID`), optimistic effects (`Upserts`, `Removes`) ([Declarations](Declarations.md)) |

## Errors

| v0.2 | v1 |
|---|---|
| `livewire.Errorf(status, code, format, ...)`, `*livewire.Error{Status, Code, Message, Fields}` | the constructors above; `*livewire.Error` has no exported fields, `Status()` and `Body()` read it |
| any code a handler chose | **breaking:** a closed set: `invalid_request`, `invalid_filter`, `invalid_sort`, `invalid_page`, `invalid_cursor`, `forbidden_field`, `unauthenticated`, `forbidden`, `not_found`, `conflict`, `in_progress`, `internal`; the message is fixed per code |
| `unauthorized` | **breaking:** `unauthenticated` |

## Engine and database

| v0.2 | v1 |
|---|---|
| `NewEngine(&EngineOptions{Registry, DSN})` returning `*Engine` | `NewEngine(&livewire.EngineOptions{Registry, ChangeFeed, IdentitySource, Transactor, AllowedOrigins, ...}) (*Engine, error)`; the feed is `postgresadapter.NewListener(&ListenerOptions{DSN})` ([Live-Engine](Live-Engine.md)) |
| `EngineOptions.Revalidate` (optional) | `EngineOptions.IdentitySource` (required, `session.Guard`), asked on every op, re-run and tick (D6); a gone identity closes 4401 |
| `EngineOptions.CheckOrigin` (same host by default) | `AllowedOrigins`, or `SameHostOnly` for the same host only (D9) |
| `EngineOptions.Partition(params)` | a declaration's `Partition` option; the tenant is the default partition (D25, D42) |
| `EngineOptions.Channel`, `livewire.DefaultChannel` | **breaking for a custom channel:** one fixed channel, `postgresadapter.Channel` (`livewire`) |
| `EngineOptions.OnChange` | gone: react to a write with a govengers/jobs job enqueued in the mutation's transaction, or with the `OutcomeAfterCommit` outcome (D41) |
| `go engine.Run(ctx)` | `engine.Run(ctx) error` in an errgroup, `engine.Close(ctx)` on shutdown (every socket gets 1001) |
| `api.GET("/ws", engine.Handler())` (a gin handler) | `ginadapter.SocketHandler(engine)` on gin, `engine.Handler()` on `net/http`; the Go client expects `{BasePath}/socket` by default |
| `Engine.CloseWhere(match)` | `Engine.RevalidateOwner(livewire.OwnerKey(identity))`, `RevalidateAll()` |
| `InstallTriggers(ctx, db, channel, tables...)` on every start | **breaking:** `postgresadapter.TriggerFunctionSQL` and `TriggerSQL(tables...)` in a goose migration; nothing runs DDL at startup (D22) |
| `InstallPartitionedTriggers(ctx, db, channel, column, tables...)` | `PartitionedTriggerSQL(table, column)` per table |
| loaders and handlers on their own pool or transaction | the engine's transaction (D31): `postgresadapter.TransactionFrom(ctx)`, the pool outside one ([Postgres](Postgres.md#the-transactor)) |
| (none) | stored filters, idempotency, audit, presence, history: their SQL and stores in `postgresadapter` |

The triggers keep their names: `TriggerFunctionSQL` replaces v0.2's `livewire_notify()` and `TriggerSQL` its `livewire_notify` triggers (both `CREATE OR REPLACE`). Run `TriggerSQL` or `PartitionedTriggerSQL` once for every table v0.2 installed a trigger on, since the trigger arguments changed (v0.2 passed the channel), and remove the `InstallTriggers` call. A change now names its operation in lower case.

## Wire protocol

| v0.2 | v1 |
|---|---|
| request `{id, op, target, params, subscription}` | `{id, op, target, params, body, idempotencyKey}`; an `unsubscribe` names the subscription as its `target` |
| ops `subscribe`, `unsubscribe`, `ping` | also `query`, `call`, `batch`, `join`, `leave` (D2) |
| response `{id, ok, subscription, error}` | `{id, ok, status, payload, error, subscription, filterId}` |
| push `{sub, seq, kind, payload}`, kinds `snapshot`, `diff`, `replace`, `error` | unchanged; **breaking:** a list snapshot is a `ListResponse`, a diff `{upserts, removes, order, total}` carries the total |
| close 4401 (`CloseUnauthorized`) | 4401 (`CloseIdentityGone`); also 1001 going away and 1013 too slow |
| OpenAPI 3.1 | **breaking:** OpenAPI 3.0.3 (`nullable: true`), with `x-websocket`, `x-livewire-guarded`, `x-livewire-effect` |

## Generator and TypeScript

| v0.2 | v1 |
|---|---|
| `gen.Generate(registry, gen.Options{TSDir, OpenAPIFile, Title, Version, BasePath, SecuritySchemes, AmbientParams, Enums})` | `generate.Write(registry, &generate.Options{TypeScriptDirectory, OpenAPIFile, OpenAPI: generate.OpenAPIOptions{Title, Version, Description, Servers, SecuritySchemes}, GoClient, CommandLine, Enums, Presence})`; the base path comes from the registry |
| `SecuritySchemes map[string]any` | `[]generate.SecurityScheme{Name, Type, Scheme, In, ParameterName, ...}` |
| `gen.Enum(values...)`, `gen.EnumValues` | `generate.EnumOf(values...)`, `generate.Enum` |
| `AmbientParams`, `config.ambient()` | gone: a call names its path parameters; a tenant from the session is the registry's `Tenant` |
| `api/openapi.json` | the file is JSON under a YAML header and must not be named `.json` |
| the global `config` (`baseUrl`, `wsUrl`, `headers`, `onUnauthorized`, `onError`) and `connection` | **breaking:** no global state. `createLivePair({ identity, client: { baseURL, headers, onUnauthorized }, connection: { url } })` per identity, provided with `app.use(livePlugin(pairRef))` ([Generated-Clients](Generated-Clients.md#one-pair-per-identity)) |
| `addTodo(body)` | `createTodo(client, body)`: every REST function takes the `Client` |
| `useLive(live.todos, params, { cache })` | the same call, under the plugin's pair; the cache is kept per identity and adds `loading`, `pending`, `filterId`, `retry()` |
| `useLiveStatus()`: `connecting`, `live`, `offline` | **breaking:** `idle`, `connecting`, `open`, `reconnecting`, `ended` |
| `ApiError`, `NetworkError` | also `EndedError` (an ended client or connection); `ApiError.code` is from the closed set |
| `config.onError(error, { method, path })` | gone: catch at the call; `LiveOptions.onError` gets what an application callback threw |
| (none) | `useLiveList`, `useLivePresence`, `prepareCall`, optimistic effects, the Go client, the CLI, `livewiretest` |

## Order of the move

1. Change the module path and the imports; replace `gen` with `generate`.
2. Turn the registry and every declaration into options structs; move the `Context` hook's tenant into `RegistryOptions.Tenant` and its other checks into the loaders.
3. Replace `binding` with `validate` tags; check the clients send no unknown members.
4. Add a goose migration with livewire's SQL ([Postgres](Postgres.md#migrations)); remove `InstallTriggers`.
5. Build the engine with the Postgres adapters and `session.Guard`; mount the routes with `ginadapter`.
6. Regenerate the clients; replace the global `config` with a pair per identity; adapt the frontend to `ListResponse` bodies, the error codes and the statuses.
7. Add the contract gates to the application's tests ([Testing](Testing.md)).
