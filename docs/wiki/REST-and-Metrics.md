# REST and metrics

`ginadapter` serves the engine on gin; `prometheusadapter` turns its outcomes into metrics. Neither logs.

## Routes

`ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine, MaximumBodyBytes})` mounts every route of the engine's registry below its `BasePath`; `ginadapter.SocketHandler(engine)` is the socket. Both go behind the application's authentication, which puts the `auth.Identity` into the request context. From [examples/todo](../../examples/todo/internal/server/server.go):

<!-- snippet: examples/todo/internal/server/server.go#routes -->
```go
api := router.Group("", authenticated)
if err := ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: engine}); err != nil {
    return nil, err
}
socket := ginadapter.SocketHandler(engine)
api.GET(SocketPath, func(ginContext *gin.Context) {
    // The socket's units of work are not the upgrade request: they get events of their own
    // (NewObserver), so nothing writes to the upgrade's event after it was emitted (LOG-008).
    ginContext.Request = ginContext.Request.WithContext(wideevent.NewContext(ginContext.Request.Context(), nil))
    socket(ginContext)
})
```

| Declaration | Routes |
|---|---|
| resource | `GET Path`, `GET Path/{id}` (with `Item`), `GET Path/distinct`, `GET Path/{id}/history`, `GET Path/export`, `GET Path/presence` (with presence) |
| list | `GET Path` |
| object | `GET Path`, `GET Path/presence` (with presence) |
| mutation | its method and `Path` |
| upload | `POST Path` |
| the library's | `POST /batch`, `POST /filters`, `GET /filters`, `DELETE /filters/{id}` |

- Every read is the `Engine.Read` the socket's `query` runs, so the REST body is the socket payload byte for byte; every refusal is `livewire.ResponseFor`'s status and `{code, message, fields}` body.
- A mutation's and an upload's `Check` runs before the body is read (D8): a refused request reads no body.
- The `Idempotency-Key` header (a UUID) is the call's key (D16); the `X-Request-ID` the wide-event middleware answers with is its request id, which the history and the audit record.
- Mutations, batches and uploads run on a context the client's disconnect does not cancel, so a write that started commits and is audited as what it did.
- `MaximumBodyBytes` (default 1 MiB) bounds the JSON body of a mutation, a batch and a stored filter; a larger one is 400 `invalid_request` at `body`.
- Presence routes are mounted only when the engine serves presence (`Engine.ServesPresence`).
- On REST, `distinct`, `export` and `presence` beside the item route win: an item whose key is one of these words is readable on the socket only. A uuid key never is.
- A route gin refuses (one that conflicts with a route on the router already) is returned as the error; register on a router or group of the API's own.

### Batches

`POST /batch` takes `[{target, params, body}]` and runs every call in one transaction, all or nothing, under one `Idempotency-Key` (D33, D47). It answers `[{status, payload}]` in order. A refusal names the failing call: `calls.<index>` carries its code and its fields are prefixed with `calls.<index>.`. Uploads cannot be batched.

### Uploads

An upload's body is the raw file with its `Content-Type`, or a `multipart/form-data` body whose part `file` is the file. The content type must be one the upload declares, the size at most its `MaximumBytes`. A raw body with a `Content-Length` streams to the handler; a multipart part or a body without a length is read into memory up to the limit (D60).

### Exports

An export streams; its status and headers leave with the first rows. An export that fails after its first byte aborts the response, so the client's read fails instead of ending cleanly (D60): on HTTP/1.1 the connection is closed under it, on HTTP/2 the stream is reset. Behind gin's writer or any wrapper with `Unwrap`, gin.Recovery included, that reaches the connection. A recovery of the application's own must let `http.ErrAbortHandler` through:

<!-- snippet: examples/todo/internal/server/server.go#recovery -->
```go
// recovery answers a panic with the 500 error body and prints nothing, so no header, cookie or
// API key reaches a log (LOG-009); the wide-event middleware inside it recorded the panic. A
// handler that aborts with http.ErrAbortHandler (an export cut after its first byte, D60)
// panics on, so net/http drops the connection as the handler meant.
func recovery() gin.HandlerFunc {
    return func(ginContext *gin.Context) {
        defer func() {
            recovered := recover()
            if recovered == nil {
                return
            }
            if err, isError := recovered.(error); isError && errors.Is(err, http.ErrAbortHandler) {
                panic(recovered)
            }
            if !ginContext.Writer.Written() {
                writeError(ginContext, fmt.Errorf("panic: %v", recovered))
            }
            ginContext.Abort()
        }()
        ginContext.Next()
    }
}
```

Behind a writer wrapper without `Unwrap` (a gzip middleware embedding `gin.ResponseWriter`) the abort reaches no connection. The trailer `Livewire-Export-Rows` (`ginadapter.ExportRowsTrailer`) is the signal there, and an extra one everywhere else: it is declared with the headers and set, to the rows written, only when the export finished. The Go client and the CLI fail an export without it.

## Wide events

With the wide-event-logger's gin middleware in front, every REST outcome is recorded on the request's event: `livewire.target`, `livewire.kind`, `livewire.read` for a read kind, `livewire.status`, `livewire.code` on a refusal, `livewire.rows` for an export, and the event's success or failure. The socket's units of work are not the upgrade request: the application's [Observer](Live-Engine.md#the-observer) emits an event per socket outcome, and the socket route takes the upgrade's event out of the context so nothing writes to it after it was emitted (the routes snippet above does).

## Prometheus

`prometheusadapter.New(&prometheusadapter.Options{Registerer, Namespace})` registers the collectors on the application's own registry, never the default one; `Metrics.Observe` is a `livewire.Observer`, passed as `RegistryOptions.Observer` or called from the application's own:

<!-- snippet: examples/todo/cmd/todo/application.go#metrics -->
```go
metrics, err := prometheusadapter.New(&prometheusadapter.Options{Registerer: options.Metrics, Namespace: "todo"})
if err != nil {
    return nil, err
}
```

| Metric (`<namespace>_livewire_...`) | Labels |
|---|---|
| `units_total`, `unit_duration_seconds` | `kind`, `operation`, `target`, `status_class` (the duration without `status_class`) |
| `reruns_total`, `rerun_duration_seconds` | `target`, `status_class` (the duration `target` only) |
| `sockets`, `subscriptions` | gauges |
| `socket_closes_total` | `code`: 1000, 1001, 1005, 1006, 1009, 1011, 1013, 4401, `other` |
| `queue_drops_total` | sockets closed 1013 |
| `query_wait_seconds` | `operation`: the socket op, `rerun`, `revalidate` |
| `background_failures_total` | `kind`: `feed`, `heartbeat`, `revalidate` |

Every label value is from a closed set: an outcome's target is a registered name, `batch`, `filters` or `""`, and an unknown socket operation is `other` (D59). A registration that fails (a collector registered twice) is returned, and the collectors registered before it are unregistered again.
