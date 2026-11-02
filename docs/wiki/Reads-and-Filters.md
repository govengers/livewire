# Reads and filters

The reads of a [resource](Declarations.md#resources) take the reserved parameters, on REST as query parameters and on the socket as `params`:

| Parameter | Meaning |
|---|---|
| `filter` | an inline filter document (JSON) |
| `filterId` | a stored filter's id; the engine reads the document from the `FilterStore` |
| `sort` | comma-separated fields, `-` for descending: `-createdAt,title`; none is the resource's `DefaultSort` |
| `limit`, `offset` | the page; an empty or larger `limit` is the resource's `MaximumLimit` |
| `after` | the `next` of the previous page, in [cursor mode](#cursor-mode) |
| `field` | the field of a [distinct](#distinct) read |

Every list answers `{limit, offset, total, items}` (D14) with the applied page; `total` counts the rows under scope and filter, at most `CountCap + 1` when the resource caps it. A cursor-mode page adds `next`.

The whole request is parsed first: an unknown field, a bad sort, page or cursor is 400 with `invalid_filter`, `invalid_sort`, `invalid_page` or `invalid_cursor`, whoever asks. Then the fields it names are checked against the [column checks](Declarations.md#column-checks): a guarded one is 403 `forbidden_field`.

## Filter documents

A document is `{"where": <node>, "search": "..."}`, both optional; the empty document matches every row. A node is a condition `{"field", "op", "value"}` or `{"and": [...]}`, `{"or": [...]}`, `{"not": <node>}`. From [query/example_test.go](../../query/example_test.go):

<!-- snippet: query/example_test.go#filter-document -->
```go
document, err := query.Parse(fields, []byte(`{
    "where": {"and": [
        {"field": "done", "op": "eq", "value": false},
        {"or": [
            {"field": "priority", "op": "in", "value": ["high", "normal"]},
            {"field": "dueAt", "op": "lt", "value": {"relative": "+2d"}}
        ]},
        {"not": {"field": "title", "op": "startsWith", "value": "draft"}}
    ]},
    "search": "milk"
}`))
```

- A condition names a field declared `Filter`, with an operator of its type ([Fields](Declarations.md#fields)) and a value of that type: a list for `in` and `notIn`, a bool for `isNull` and `isEmpty`.
- A time is an RFC 3339 string or a time relative to the database clock, `{"relative": "-7d"}` (units `s`, `m`, `h`, `d`, `w`, combinable: `-1h30m`). A live read with a relative time re-runs on the engine's safety tick (default 30 s), since its result moves without a write.
- `search` matches every visible field declared `Search`, case-insensitive, as a substring.
- `ne` and `notIn` include the rows whose value is NULL, exactly like `not` around `eq` and `in`.
- Limits (D12): depth 8, 200 nodes, 500 values in a list, strings of 1 KiB, documents of 16 KiB.

The generated clients type documents per resource: `TodosFilter` in TypeScript (`filters.ts`) and in the Go client, where `TodosPriorityEqual(...)` builds one condition and `client.And`, `Or` and `Not` combine them.

## Stored filters

A client stores a filter once and reads by its id (D13). The routes are the library's own, below the `BasePath`:

| Route | Does |
|---|---|
| `POST /filters` | stores `{resource, context?, name?, filter}` and answers 201 with the `FilterItem` |
| `GET /filters?resource=todos` | the caller's named filters, of every resource without `resource` |
| `DELETE /filters/{id}` | removes one of the caller's filters |

A filter belongs to its owner (`provider:subject`) and tenant; another owner's id is 404. An unnamed filter is deduplicated by owner, tenant, resource, `context` and the hash of its canonical form, and swept after 30 days unused. An inline `filter` on a subscribe is stored as an unnamed filter, and the response names it (`filterId`). A filter of a history read names the resource `todos.history`. The Go client stores one and reads by it:

<!-- snippet: examples/todo/cmd/todo/client_test.go#go-client-filter -->
```go
filter := todoclient.TodosFilter{Where: new(todoclient.TodosPriorityEqual(todoclient.TodosPriorityLow))}
document, err := json.Marshal(filter)
require.NoError(t, err)
stored, err := alice.SaveFilter(ctx, &livewire.FilterRequest{Resource: "todos", Name: "low", Filter: document})
require.NoError(t, err)
low, err := alice.GetTodos(ctx, todoclient.TodosListQuery{FilterID: stored.ID})
```

The `FilterStore` keeps at most 1000 unnamed filters per owner and tenant, replacing the least recently used, and 200 named ones (one more is 400). So a 404 on an unnamed `filterId` means: send the filter inline again. The TypeScript runtime and the Go client do it once by themselves, for the unnamed filters they stored or registered (D54).

## Distinct

`GET /todos/distinct?field=priority` (socket target `todos.distinct`) answers the values of one filterable scalar field under scope and filter with their counts, most frequent first, at most `DistinctMaximum` (default 1000): `[{"value": "high", "count": 3}]`. It takes `filter` and `filterId`, is live, and refuses a guarded field. The loader passes it to [postgresadapter.Distinct](Postgres.md#lists-history-distinct-and-export).

## Cursor mode

A resource with `Cursor: true` pages by keyset on request (D38): the client sends the `next` of a page as `after` and gets the page after it, with its own `next`. A page that is not full has no `next`; a full page always has one, even when no row follows, and the page after it is empty, which ends the walk. Offset paging stays the default; `after` on a resource without `Cursor` is 400. The cursor holds the sort it was taken under, so a different `sort` with it is `invalid_cursor`; `total` is still counted and capped. A field that can be NULL must be declared `Nullable`, or the walk skips rows ([Fields](Declarations.md#fields)).

`useLiveList(live.todos, { mode: 'cursor' })` tiles live pages with `loadMore()`; each page is a subscription after the `next` of the page above, which the runtime follows when the page's rows change, so no row falls between two pages (D64).

## History

A resource with `Versioned: true`, an `Item` on its uuid field `id` and a `History` loader serves `GET /todos/{id}/history` and the live target `todos.history` (D34, D56). Each item of the answer is a `Version{id, operation, validFrom, validTo, actor, realActor, mutation, requestId, data}`: `id` is the version number, `data` the item as that version left it, passing the column checks. The history table and its trigger come from `postgresadapter.HistorySQL(table)`, the loader from `postgresadapter.HistoryPage`:

<!-- snippet: examples/todo/internal/repository/todo.go#history -->
```go
// History reads one page of a todo's versions (D34, D56).
func (r *TodoRepository) History(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[model.Todo]], error) {
    page, err := postgresadapter.HistoryPage(ctx, r.pool, "todos", listQuery, decodeTodoRow)
    if err != nil {
        return nil, fmt.Errorf("todo history: %w", err)
    }
    return page, nil
}
```

- The reserved parameters filter and sort over `livewire.HistoryFields()`: `id`, `operation`, `validFrom`, `validTo`, `actor`, `realActor`, `mutation`, `requestId`. The default sort is by version; paging is by offset.
- The item read runs first under the caller, so scope and ownership decide who sees a history. A deleted item's history is 404 on this route; a live history ends with a `not_found` error push.
- A history covers the id's current life: an item created again under a deleted id starts over. A row moved to another partition keeps its history.
- The actor, the mutation and the request id are what the engine's transaction recorded; a batch records each call's own mutation (D58).

## Export

`GET /todos/export?format=csv` streams every row under scope, filter and sort as CSV (D35, D57), REST only. The columns are `ExportColumns`, or every field, without those whose `Read` check refuses the caller. The loader passes its arguments to [postgresadapter.WriteCSV](Postgres.md#lists-history-distinct-and-export):

<!-- snippet: examples/todo/internal/repository/todo.go#export -->
```go
// Export writes every todo under the query as CSV (D35).
func (r *TodoRepository) Export(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error) {
    rows, err := postgresadapter.WriteCSV(ctx, r.pool, &postgresadapter.Export{Specification: todoSQL, Query: listQuery, Columns: columns}, writer)
    if err != nil {
        return rows, fmt.Errorf("export todos: %w", err)
    }
    return rows, nil
}
```

- A text cell that a spreadsheet would run as a formula (starting with `=`, `+`, `-`, `@`, a tab or a carriage return) is written with a single quote in front, and so is one that starts with a quote. A program reading the CSV as data drops exactly one leading quote of every text cell that has one (D69).
- A complete export ends with the trailer `Livewire-Export-Rows`; an export that fails after its first byte is cut off ([REST-and-Metrics](REST-and-Metrics.md#exports)). The Go client and the CLI fail on a cut export.
- Excel is not built in v1 ([TODO.md](../../TODO.md)).
