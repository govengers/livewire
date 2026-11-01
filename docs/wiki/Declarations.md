# Declarations

Every part of an API is a declaration in one registry. Constructors return the declaration and never panic; `Registry.Add` checks every declaration it gets and returns the first mistake, adding nothing then, so `main` fails at startup on a wiring mistake (design D43).

<!-- snippet: examples/todo/internal/controller/registry.go#new-registry -->
```go
registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: BasePath, Tenant: actor.Workspace, Observer: observer})
```

`BasePath` prefixes every REST route; `Tenant` resolves the tenant of a request ([Tenant](#tenant)); `Observer` receives every outcome ([Live-Engine](Live-Engine.md#the-observer)). `Add` refuses a duplicate name, two declarations on one method and route, the reserved names `batch` and `filters`, the library's own routes `/batch` and `/filters`, and a mutation whose `Resource` or `Effect` names a declaration that is not registered by the end of the call.

Paths use `{name}` placeholders. The name of a declaration is its socket target and the identifier of the generated clients.

## Resources

A resource is a filterable list. Its reads are the list (`GET Path`), the item (`GET Path/{id}`, with `Item`), the distinct values (`GET Path/distinct`, with `Distinct`), an item's versions (`GET Path/{id}/history`, with `Versioned` and `History`), the export (`GET Path/export`, with `Export`) and the presences on an item (`GET Path/presence`, with `Item`, when the engine serves presence). From [examples/todo](../../examples/todo/internal/controller/registry.go):

<!-- snippet: examples/todo/internal/controller/registry.go#resource -->
```go
return livewire.NewResource("todos", "/todos", &livewire.ResourceOptions[api.TodoItem]{
    Tables:       []string{"todos"},
    Read:         authorization.IsUser(),
    Fields:       c.todos.Fields(),
    DefaultSort:  []query.SortTerm{{Field: "createdAt", Descending: true}},
    Scope:        c.todos.Scope,
    MaximumLimit: 100,
    CountCap:     10000,
    Item:         &livewire.ItemKey{Parameter: "id", Field: "id"},
    Versioned:    true,
    List: func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[api.TodoItem], error) {
        page, err := c.todos.List(ctx, listQuery)
        if err != nil {
            return nil, responseError(err)
        }
        return &livewire.Page[api.TodoItem]{Items: mapItems(page.Items, toTodoItem), Total: page.Total, Next: page.Next}, nil
    },
    Get: func(ctx context.Context, listQuery *query.ListQuery) (*api.TodoItem, error) {
        todo, err := c.todos.Get(ctx, listQuery)
        if err != nil {
            return nil, responseError(err)
        }
        return new(toTodoItem(*todo)), nil
    },
    Distinct: c.todos.Distinct,
    History:  c.history,
    Export:   c.todos.Export,
})
```

- `Read` and `List` are required; `Get` comes with `Item`.
- `Scope` narrows every read and re-run before the filter and the count; nil is no scope.
- `MaximumLimit` clamps a page (default 100); `CountCap` caps the total, which is then at most `CountCap + 1` (0 counts every row); `DistinctMaximum` caps a distinct read (default 1000).
- `Cursor: true` allows keyset pages ([cursor mode](Reads-and-Filters.md#cursor-mode)); `ExportColumns` picks the export's columns (default: every field).
- A loader maps domain errors to livewire's ([Errors](#errors)); the repository behind it is on the [Postgres](Postgres.md#lists-history-distinct-and-export) page.

## Fields

`Fields` say what a client may filter, sort and search by; each names its API name (the item's JSON name), its type and the SQL that reads it over the resource's `FROM`:

<!-- snippet: examples/todo/internal/repository/todo.go#fields -->
```go
// todoSQL is what the filterable reads of todos read from (D11).
var todoSQL = &query.ResourceSQL{From: "todos", IDColumn: "todos.id"}

// TodoFields are the fields of the todos resource: the API names and the SQL that reads them.
func TodoFields() query.Fields {
    return query.Fields{
        {Name: "id", Type: query.TypeUUID, SQL: "todos.id", Filter: true, Sort: true},
        {Name: "title", Type: query.TypeText, SQL: "todos.title", Filter: true, Sort: true, Search: true},
        {Name: "priority", Type: query.TypeEnum, SQL: "todos.priority", Enum: []string{string(model.PriorityLow), string(model.PriorityNormal), string(model.PriorityHigh)}, Filter: true, Sort: true},
        {Name: "done", Type: query.TypeBoolean, SQL: "todos.done", Filter: true, Sort: true},
        {Name: "dueAt", Type: query.TypeTime, SQL: "todos.due_at", Filter: true, Sort: true, Nullable: true},
        {Name: "createdAt", Type: query.TypeTime, SQL: "todos.created_at", Filter: true, Sort: true},
        {Name: "updatedAt", Type: query.TypeTime, SQL: "todos.updated_at", Sort: true},
        {Name: "version", Type: query.TypeInteger, SQL: "todos.version"},
    }
}
```

| Type | Filter operators |
|---|---|
| `string`, `text` | `eq`, `ne`, `in`, `notIn`, `contains`, `startsWith`, `isNull`; only these two may be `Search` |
| `int`, `float`, `time` | `eq`, `ne`, `gt`, `gte`, `lt`, `lte`, `in`, `notIn`, `isNull` |
| `bool` | `eq`, `ne`, `isNull` |
| `enum` (with `Enum` values), `uuid` | `eq`, `ne`, `in`, `notIn`, `isNull` |
| `stringArray` (a `text[]`) | `has`, `hasAny`, `hasAll`, `isEmpty`; not sortable |
| `json` | none; neither filterable nor sortable |

`Nullable` must be true whenever the SQL can yield NULL, or cursor pages skip rows; a field left false is derived from the item type (a pointer field is nullable). A numeric column is declared `float` over `t.amount::float8`.

`Scope` is a `query.Predicate` that writes SQL with bound arguments; the todo app scopes by the tenant the engine resolved:

<!-- snippet: examples/todo/internal/repository/todo.go#scope -->
```go
// WorkspaceScope narrows the todos to the workspace of the request, livewire's tenant (D42),
// which the engine resolved before it calls the scope (DAT-017).
func WorkspaceScope(ctx context.Context) (query.Predicate, error) {
    workspace := livewire.TenantFrom(ctx)
    if workspace == "" {
        return nil, errNoWorkspace
    }
    return func(builder *query.Builder) {
        builder.WriteString("todos.workspace = ").Argument(workspace)
    }, nil
}
```

## Lists and objects

`NewList` answers a `ListResponse` of every item its loader returns; `NewObject` answers one item. Both take a parameters struct:

<!-- snippet: examples/todo/internal/controller/registry.go#typed-reads -->
```go
livewire.NewList("activity", "/activity", &livewire.ListOptions[api.ActivityItem, api.ActivityParameters]{
    Tables: []string{"activity"}, Read: authorization.IsUser(), Load: todos.activity,
}),
livewire.NewObject("summary", "/summary", &livewire.ObjectOptions[api.SummaryItem, livewire.NoParameters]{
    Tables: []string{"todos", "attachments"}, Read: authorization.IsUser(), Load: todos.summary,
}),
```

<!-- snippet: examples/todo/internal/api/api.go#activity-parameters -->
```go
// ActivityParameters narrow the activity to one todo.
type ActivityParameters struct {
    TodoID *uuid.UUID `query:"todoId"`
}
```

- A parameters field carries `path:"name"` (required, one per `{name}` of the path) or `query:"name"` (optional). Supported are string kinds, bool, signed integers, `uuid.UUID` and `time.Time` (RFC 3339), one pointer level, and string slices for list query parameters; `enum:"a,b"` restricts a string. Embedded structs are flattened; any other field fails `Add`. `livewire.NoParameters` is none.
- A live list's item needs a JSON field `id`: diffs key items by it.
- A typed list has no page: its `limit` is the number of items it answered (D73).
- An object's loader returns `livewire.NotFound()` when there is nothing.

## Mutations

A mutation is a write with a JSON body, on `POST`, `PUT`, `PATCH` or `DELETE`:

<!-- snippet: examples/todo/internal/controller/registry.go#mutations -->
```go
livewire.NewMutation("createTodo", http.MethodPost, "/todos", &livewire.MutationOptions[livewire.NoParameters, api.CreateTodoRequest, api.TodoItem]{
    Check: authorization.IsUser(), Status: http.StatusCreated, ClientID: livewire.UUIDv7, Effect: livewire.Upserts("todos"),
    Handle: todos.create,
}),
livewire.NewMutation("updateTodo", http.MethodPatch, "/todos/{id}", &livewire.MutationOptions[api.TodoParameters, api.UpdateTodoRequest, api.TodoItem]{
    Check: authorization.IsUser(), Resource: "todos", Effect: livewire.Upserts("todos"),
    Handle: todos.update,
}),
livewire.NewMutation("deleteTodo", http.MethodDelete, "/todos/{id}", &livewire.MutationOptions[api.TodoParameters, livewire.NoBody, livewire.NoContent]{
    Check: authorization.IsUser(), Effect: livewire.Removes("todos"),
    Handle: todos.delete,
}),
```

<!-- snippet: examples/todo/internal/api/api.go#requests -->
```go
// CreateTodoRequest creates a todo; ID is a UUID v7 the client may choose (D18), so an
// optimistic item and its retry carry the same id.
type CreateTodoRequest struct {
    ID       uuid.UUID  `json:"id,omitzero"`
    Title    string     `json:"title" validate:"required,max=200"`
    Priority Priority   `json:"priority,omitempty" validate:"omitempty,oneof=low normal high"`
    DueAt    *time.Time `json:"dueAt,omitempty"`
}

// UpdateTodoRequest changes the fields it carries; clearDueAt removes the due time.
type UpdateTodoRequest struct {
    Title      *string    `json:"title,omitempty" validate:"omitempty,min=1,max=200"`
    Priority   *Priority  `json:"priority,omitempty" validate:"omitempty,oneof=low normal high"`
    Done       *bool      `json:"done,omitempty"`
    DueAt      *time.Time `json:"dueAt,omitempty"`
    ClearDueAt bool       `json:"clearDueAt,omitempty"`
}

// TodoParameters name one todo.
type TodoParameters struct {
    ID uuid.UUID `path:"id"`
}
```

- `Check` and `Handle` are required. `Status` defaults to 200, or to 204 for `livewire.NoContent`; `livewire.NoBody` is a mutation without a body.
- The body is decoded strictly (D40): it must be an object, an unknown member at any depth and a repeated member are 400 `invalid_request` naming the member's path. Then the field checks run ([Column checks](#column-checks)), then the `validate` tags (go-playground/validator, D44), each failure named by its JSON path. A tag the validator cannot parse fails `Add`.
- `Check` runs before the body is read, on REST too (D8).
- The handler runs inside the engine's transaction, after the `Idempotency-Key` was reserved; `livewire.IdempotencyKeyFrom(ctx)` reads the key ([Postgres](Postgres.md#the-transactor)).

### Client ids

`ClientID: livewire.UUIDv7` (or `UUIDv4`) lets a `POST` create accept the item's `id` from the client, checked for its UUID version (D18); without it a create refuses an `id`. The TypeScript and Go clients fill one in when the body has none, so a retry and an optimistic item carry the same id. A duplicate id is whatever the repository answers, `livewire.Conflict()` in the todo app.

### Optimistic effects

`Effect: livewire.Upserts("todos")` lets a client that runs the mutation over the socket show the response in the live lists of `todos` at once, marked pending, until the next push reconciles it; `livewire.Removes("todos")` removes the item named by the `id` path parameter. An error takes the change back (D36, D65). `Upserts` needs a response with a JSON field `id`, `Removes` a `path:"id"` parameter.

## Uploads

An upload is a binary write, REST only: a raw body or a multipart part `file`, of a declared content type, at most `MaximumBytes`:

<!-- snippet: examples/todo/internal/controller/registry.go#upload -->
```go
livewire.NewUpload("uploadAttachment", "/todos/{id}/attachments", &livewire.UploadOptions[api.TodoParameters, api.AttachmentItem]{
    Check: authorization.IsUser(), Status: http.StatusCreated,
    ContentTypes: []string{"text/plain", "image/png", "application/pdf"}, MaximumBytes: maximumAttachmentBytes,
    Handle: todos.attach,
}),
```

The handler gets a `*livewire.File{ContentType, Size, Body, Filename}`; `ContentType` is the declared type it matched, without parameters. A raw body with a `Content-Length` streams to the handler; a multipart part or a body without a length is read into memory up to the limit.

## Column checks

A field's `Read` check strips the field from every item for an identity it refuses: on REST, the socket, the export and the history; a filter, sort, search or distinct read on it is 403 `forbidden_field` (D39). A mutation with `Resource` set applies the resource's `Write` checks (default: `Read`) to the body by JSON name; `FieldChecks` adds checks to any mutation. Typed reads name their guarded fields with `Guarded`:

<!-- snippet: example_test.go#column-checks -->
```go
staff := authorization.HasRole("staff")
employees := livewire.NewResource("employees", "/employees", &livewire.ResourceOptions[employee]{
    Tables: []string{"employees"},
    Read:   authorization.IsUser(),
    Fields: query.Fields{
        {Name: "id", Type: query.TypeUUID, SQL: "employees.id", Filter: true, Sort: true},
        {Name: "name", Type: query.TypeText, SQL: "employees.name", Filter: true, Sort: true, Search: true},
        {Name: "salary", Type: query.TypeInteger, SQL: "employees.salary", Filter: true, Sort: true, Read: staff},
    },
    DefaultSort: []query.SortTerm{{Field: "name"}},
    Cursor:      true, // keyset pages: the `after` parameter and ListResponse.next (D38)
    List:        listEmployees,
})
payroll := livewire.NewList("payroll", "/payroll", &livewire.ListOptions[employee, livewire.NoParameters]{
    Read:    authorization.IsUser(),
    Guarded: []livewire.GuardedField{{Name: "salary", Read: staff}}, // the same for a typed read
    Load:    listPayroll,
})
update := livewire.NewMutation("updateEmployee", http.MethodPatch, "/employees/{id}", &livewire.MutationOptions[employeeParameters, updateEmployee, employee]{
    Check:       authorization.IsUser(),
    Resource:    "employees", // "salary" in the body needs the field's Write check, its Read by default
    FieldChecks: map[string]authorization.Check{"note": authorization.HasRole("manager")},
    Handle:      saveEmployee,
})
```

The checks run again on every identity re-validation, so a lost role strips the field from the next push. Generated types mark guarded fields optional.

## Errors

A loader or handler returns livewire's refusals or an `*auth.Error`; anything else answers 500 with the code `internal` and none of its text:

| Constructor | Status, code |
|---|---|
| `livewire.Invalid(fields)` | 400 `invalid_request`, the fields named |
| `livewire.ForbiddenField(name)` | 403 `forbidden_field` |
| `livewire.NotFound()` | 404 `not_found` |
| `livewire.Conflict()` | 409 `conflict` |
| `livewire.InProgress()` | 409 `in_progress` |
| `*auth.Error`, `auth.ErrUnauthenticated`, `auth.ErrForbidden` | 401 `unauthenticated`, 403 `forbidden`, ... |

The filter, sort, page and cursor refusals of the `query` package answer 400 `invalid_filter`, `invalid_sort`, `invalid_page` and `invalid_cursor`. The body is always `{code, message, fields?}`; the code set is closed, so a client can switch on it. Map the domain's errors once:

<!-- snippet: examples/todo/internal/controller/registry.go#response-error -->
```go
// responseError maps the domain's errors to livewire's refusals in one place (API-002); any
// other error stays as it is, an *auth.Error answers its kind, the rest a 500 without text.
func responseError(err error) error {
    switch {
    case errors.Is(err, model.ErrNotFound):
        return livewire.NotFound()
    case errors.Is(err, model.ErrConflict):
        return livewire.Conflict()
    default:
        return err
    }
}
```

## Tenant

`RegistryOptions.Tenant` resolves the tenant once per request or subscription, after the static check, from the identity or the decoded parameters (nil for a resource read). The engine puts it into the context (`livewire.TenantFrom(ctx)`), uses it as the subscription's partition, sets `livewire.tenant` in every engine transaction, and stores filters, presences, audit rows and idempotency records under it (D42):

<!-- snippet: examples/todo/internal/actor/actor.go#tenant -->
```go
// Workspace is the workspace ctx acts in, the registry's tenant (D42).
func Workspace(ctx context.Context, _ any) (string, error) {
    member, err := Member(ctx)
    return member.Workspace, err
}
```

A declaration's `Partition` function names another partition than the tenant (D25); the triggers then carry that column ([Postgres](Postgres.md#migrations)). `livewiretest.TenantIsolation` proves that no row crosses ([Testing](Testing.md)).
