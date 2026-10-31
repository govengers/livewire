# govengers/livewire

Filled in as the work packages of [docs/design.md](../design.md) land. Until then the design is the documentation.

## Root package `livewire` (WP2)

The registry is the single source of truth: `NewRegistry(&RegistryOptions{BasePath, Tenant, Observer})`, then `registry.Add(...)` with the five declaration kinds. A constructor never panics (D43); a mistake in a declaration is returned by `Add`, which adds nothing and changes no declaration when one fails. A declaration belongs to one registry.

| Kind | Constructor | Reads | Writes |
|------|-------------|-------|--------|
| `Resource` | `NewResource[Item](name, path, &ResourceOptions[Item]{...})` | `GET path`, `GET path/{key}`, `GET path/distinct`; reserved parameters `filter`, `filterId`, `sort`, `limit`, `offset`, `after`, `field` | |
| `List` | `NewList[Item, Parameters](...)` | `GET path`, typed parameters, answers `ListResponse` | |
| `Object` | `NewObject[Item, Parameters](...)` | `GET path`, typed parameters | |
| `Mutation` | `NewMutation[Parameters, Request, Response](name, method, path, ...)` | | JSON body: `DecodeBody` refuses unknown fields at any depth (D40), checks a create's client id (D18), the field write checks (D39) and the `validate` tags |
| `Upload` | `NewUpload[Parameters, Response](name, path, ...)` | | `POST`, binary body as `*File`, REST only (D17) |

The type parameters are inferred from the options struct, so call sites write `livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[part.Item]{...})`. Options are copied; live declarations may name a `Partition` resolver (D25).

- Parameters: `path:"id"` and `query:"status"` tags, embedded structs (by value) flattened, `enum:"a,b"`; string, bool, int kinds, `uuid.UUID`, `time.Time` (RFC 3339, decoded in UTC), pointers, `[]string` for query lists. A repeated scalar query key is 400. A `ParameterSource` feeds the decoder; `MapSource` is the one the socket frames use.
- Filterable reads: `Resource.ListQuery`, `DistinctQuery` and `ItemQuery` turn the reserved parameters into a `*query.ListQuery` for the loader. The whole request is parsed first (unknown fields, bad page or cursor are 400 `invalid_*`), then the named fields are checked once through `query.Fields.Visible` (403 `forbidden_field`); `RedactItems` and `RedactItem` strip guarded fields from the marshaled items and refuse any other shape. `Field.Nullable` is derived from pointer fields of the item type with encoding/json's field rules.
- Errors: one body `{code, message, fields?}` with fixed texts; `NotFound`, `Conflict`, `InProgress`, `Invalid(fields)`, `ForbiddenField(name)`; `ResponseFor(err)` lets the outermost known error decide, maps `*query.Error` and the auth refusals (by kind, into the closed code set), everything else to 500 `internal`.
- Wire: `RequestFrame`, `ResponseFrame`, `PushFrame`, `ListChange` (the wire's `diff`), the `Operation*`, `Push*` and `Close*` constants; `ListState` computes list changes by item id and refuses duplicate or non-scalar ids.
- Tenant (D42): `WithTenant`/`TenantFrom`, `OwnerOf(ctx)` gives the `Owner{Key, Tenant}` every stored row carries.
- Adapter contracts: `ChangeFeed`, `IdentitySource` (auth's unauthenticated or forbidden refusal means the identity is gone), `Transactor`/`Transaction` (the engine holds Commit and Rollback, the handler's context carries `Transaction.Handle()` through `TransactionFrom(ctx)`), `FilterStore`, `IdempotencyStore` (non-blocking reservation per owner, key and mutation), `PresenceStore`, `Observer`/`Outcome`. `IdempotencyKeyFrom(ctx)` is what handlers read.
