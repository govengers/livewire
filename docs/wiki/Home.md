# govengers/livewire

Filled in as the work packages of [docs/design.md](../design.md) land. Until then the design is the documentation.

## Root package `livewire` (WP2)

The registry is the single source of truth: `NewRegistry(&RegistryOptions{BasePath, Tenant, Observer})`, then `registry.Add(...)` with the five declaration kinds. A constructor never panics; a mistake in a declaration is returned by `Add`, which adds nothing when one declaration fails.

| Kind | Constructor | Reads | Writes |
|------|-------------|-------|--------|
| `Resource` | `NewResource[Item](name, path, &ResourceOptions[Item]{...})` | `GET path`, `GET path/{key}`, `GET path/distinct`; reserved parameters `filter`, `filterId`, `sort`, `limit`, `offset`, `after`, `field` | |
| `List` | `NewList[Item, Params](...)` | `GET path`, typed parameters, answers `ListResponse` | |
| `Object` | `NewObject[Item, Params](...)` | `GET path`, typed parameters | |
| `Mutation` | `NewMutation[Params, Request, Response](name, method, path, ...)` | | JSON body, `DecodeBody` refuses unknown fields (D40), checks client ids (D18) and field writes (D39) |
| `Upload` | `NewUpload[Params, Response](name, path, ...)` | | `POST`, binary body as `*File`, REST only (D17) |

The type parameters are inferred from the options struct, so call sites write `livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[part.Item]{...})`.

- Parameters: `path:"id"` and `query:"status"` tags, embedded structs flattened, `enum:"a,b"`; string, bool, int kinds, `uuid.UUID`, `time.Time` (RFC 3339, decoded in UTC), pointers, `[]string` for query lists. A `ParameterSource` feeds the decoder; `MapSource` is the one the socket frames use.
- Filterable reads: `Resource.ListQuery`, `DistinctQuery` and `ItemQuery` turn the reserved parameters into a `*query.ListQuery` for the loader. Unknown fields are 400 (`invalid_*`), guarded fields 403 `forbidden_field`; `Redact` strips guarded fields from the marshaled items. `Field.Nullable` is derived from pointer fields of the item type.
- Errors: one body `{code, message, fields?}`; `NotFound`, `Conflict`, `InProgress`, `Invalid(fields)`, `ForbiddenField(name)`; `ResponseFor(err)` maps those, `*query.Error` and the auth refusals, everything else to 500 `internal`.
- Wire: `RequestFrame`, `ResponseFrame`, `PushFrame`, `Diff`, the `Op*`, `Push*` and `Close*` constants; `ListState` computes list diffs by item id.
- Adapter contracts: `ChangeFeed`, `IdentitySource`, `Transactor`/`Transaction`, `FilterStore`, `IdempotencyStore`, `PresenceStore`, `Observer`/`Outcome`. `TransactionFrom(ctx)` and `IdempotencyKeyFrom(ctx)` are what handlers read.
