# Live engine

`livewire.NewEngine` serves the socket and runs the read and write pipelines both transports share. `Registry`, `IdentitySource` and `Transactor` are required, and `AllowedOrigins` unless `SameHostOnly`; the `ChangeFeed` and each store are optional, their feature off without them. From [examples/todo](../../examples/todo/cmd/todo/application.go):

<!-- snippet: examples/todo/cmd/todo/application.go#engine -->
```go
return livewire.NewEngine(&livewire.EngineOptions{
    Registry: registry, ChangeFeed: livewireStores.listener, IdentitySource: identities, Transactor: livewireStores.transactor,
    FilterStore: livewireStores.filters, IdempotencyStore: livewireStores.idempotency, AuditStore: livewireStores.audit,
    PresenceStore: livewireStores.presences, AllowedOrigins: options.AllowedOrigins, SameHostOnly: len(options.AllowedOrigins) == 0,
})
```

`Handler()` is the socket endpoint; mount it behind the same authentication as the REST routes (`ginadapter.SocketHandler` on gin). `Run(ctx)` consumes the change feed and drives the ticks until ctx ends; `Close(ctx)` ends every socket with 1001, refuses new ones with 503 and waits for them.

## The socket

One socket carries every read, subscription and write of a client. A request frame is `{id, op, target, params, body, idempotencyKey}`; the response `{id, ok, status, payload | error, subscription, filterId}` answers it by id, with the status the same request gets on REST and, on success, the REST body as its payload, byte for byte.

| Op | Target | Does |
|---|---|---|
| `query` | a read | one read, answered in the response |
| `subscribe` | a live read | the response names the subscription; pushes follow |
| `unsubscribe` | the subscription | ends it |
| `call` | a mutation | one mutation; uploads are REST only |
| `batch` | | the body is `[{target, params, body}]`, run in one transaction, all or nothing |
| `join`, `leave` | a declaration | [presence](#presence) |
| `ping` | | keeps the socket alive |

A read's target is the declaration's name with the read kind after a dot (D46): `todos`, `todos.item`, `todos.distinct`, `todos.history`, `todos.presence`. Parameters are text by name, as a REST query string carries them; a list is one comma-separated value.

A push is `{sub, seq, kind, payload}`, `seq` counting from 1 per subscription. `snapshot` is a list's full body, `diff` a list change `{upserts, removes, order, total}` (the order only when membership or order changed), `replace` an object's full body, `error` an error body that ends the subscription. Close codes: 1000 normal, 1001 the engine closes, 1009 a frame past `FrameBytes`, 1013 the client fell behind its queue, 4401 the identity is gone.

The socket admits an `Origin` in `AllowedOrigins` or the request's own host (D9); a request without `Origin` is refused when it carries cookies. That is the CSRF defence of socket writes.

## Subscriptions and re-runs

- The `Listener` reports `{table, op, id, partition}` per row change. A subscription is indexed by the tables of its declaration and its partition; a change marks the matching ones dirty, and a change without a partition (a table without `PartitionedTriggerSQL`) reaches every partition.
- A dirty subscription re-runs once; changes during a re-run collapse into one more. Its pushes leave at most `DeliveryBatchWait` (default 100 ms) after the first change, one per subscription.
- A re-run is the read again under the socket's identity: the static check, the scope, the loader, the column checks. A list then pushes a diff against what it pushed last, an object a replace; nothing when nothing changed.
- A re-run that fails with a 5xx (a panicking loader included) is retried with backoff from `RetryInterval` (default 1 s) up to 32 times it; a 4xx ends the subscription with an error push.
- Every successful connect of the feed, the first included, re-runs every subscription with a snapshot, since changes may have been missed in between.
- A filter with a relative time re-runs on the `SafetyTickInterval` (default 30 s).
- The partition of a subscription is the tenant, or what the declaration's `Partition` function names (D25).

Business reactions to a change are not subscriptions: a mutation enqueues a govengers/jobs job in its transaction ([Postgres](Postgres.md#the-transactor)), and the `OutcomeAfterCommit` outcome serves at-most-once reactions on the replica that ran it (D41).

## Identity re-validation

The engine asks the `IdentitySource` (`session.Guard.Identify` for a session, any implementation for API keys) who the socket is on every subscribe, query, call, batch, join and leave, on every re-run (cached for the `RevalidateInterval`) and on that tick, default 60 s (D6, D51):

- an error matching `auth.ErrUnauthenticated` or `auth.ErrForbidden`, or no identity, closes the socket with 4401;
- another identity re-runs every subscription under its rights; one it may not read ends with an error push `forbidden` (D7);
- any other error keeps the last identity and tries again.

`Engine.RevalidateOwner(livewire.OwnerKey(identity))` and `RevalidateAll()` re-validate at once instead of at the next tick. The todo app does it when a member moves to another workspace:

<!-- snippet: examples/todo/cmd/todo/application.go#move-member -->
```go
// moveMember puts a user into another workspace and re-validates the user's sockets at once
// (section 6, push-driven revocation): the workspace is part of the identity, so each socket
// ends the subscriptions of the old tenant now instead of at its next re-validation tick.
func (a *application) moveMember(ctx context.Context, userID uuid.UUID, workspace string) error {
    if err := a.accounts.Join(ctx, userID, workspace); err != nil {
        return err
    }
    a.revalidateUser(userID)
    return nil
}
```

<!-- snippet: examples/todo/cmd/todo/application.go#revalidate -->
```go
// revalidateUser re-validates every socket of the user, by session and by API key.
func (a *application) revalidateUser(userID uuid.UUID) {
    for _, provider := range []string{"password", "apikey"} {
        a.engine.RevalidateOwner(livewire.OwnerKey(&auth.Identity{Provider: provider, Subject: userID.String()}))
    }
}
```

## Presence

With a `PresenceStore` the engine serves presence (D37): who is on a record, with a state the client chooses (the field being edited, a cursor). `join` names a declaration (a resource with an item read, or an object), carries the record's `key` in its parameters and the state as its body; `leave` ends it. The record's own read runs first under the caller, so only who may read the record may join it or see who is there.

- The presences of a record are the live read `todos.presence` with the `key` parameter, and `GET /todos/presence?key=...` on REST: a `ListResponse` of `{id, display, state}` (`{"limit": 1, "offset": 0, "total": 1, "items": [...]}`), the display name from the identity's `Name` or `Username`.
- The engine refreshes a socket's presences every `PresenceHeartbeat` (30 s) and sweeps those older than `PresenceExpiry` (90 s), so a vanished replica's presences go too.
- Presence lives in the `livewire_presence` table, so it crosses replicas through the change feed.
- In TypeScript, `useLivePresence` joins and reads ([Generated-Clients](Generated-Clients.md#vue)).

## Limits

| Limit | Default | Over it |
|---|---|---|
| `SubscriptionsPerSocket` | 64 | 429 `invalid_request` at `subscriptions` |
| `InFlightPerSocket` | 16 | 429 at `requests` |
| `PresencesPerSocket` | 64 | 429 at `presences` |
| `FiltersPerSocket` (inline filters of live subscriptions) | 64 | 429 at `filters` |
| `QueuedBytesPerSocket` | 8 MiB | close 1013 |
| `FrameBytes` (one client frame) | 64 KiB | close 1009 |
| `WriteTimeout` | 10 s | close 1013 |
| `QueryConcurrency` (socket work only, D52) | 32 | the work waits |
| `SocketsPerOwner` | unlimited | 429 at the upgrade |

Filter documents have limits of their own ([Reads-and-Filters](Reads-and-Filters.md#filter-documents)), and so do pages (`MaximumLimit`, `CountCap`), distinct reads and uploads.

## The Observer

livewire never logs. The registry's `Observer` gets one `Outcome` per unit of work, synchronously; it must not block. An outcome carries its kind, the socket operation, the target (a declaration name with its read kind, `batch`, `filters` or `""`, so a label on it stays bounded), the subscription, the tenant, the identity, the decoded parameters, the status, the error (with an `*auth.Error`'s server-side reason), the duration, the exported rows and the wait for query slots.

| Kind | When |
|---|---|
| `request` | a REST read or stored filter route |
| `subscribe`, `rerun`, `end` | a subscription opened, re-ran, ended |
| `call`, `batch`, `after_commit` | a mutation or upload, a batch, a committed transaction |
| `export` | an export, with its rows |
| `open`, `close` | a socket upgraded, ended (the status is the close code) |
| `revalidate`, `feed`, `heartbeat` | the background work: a re-validation, a change feed failure, the presence tick |

The todo app feeds Prometheus and writes one wide event per outcome that has no request event of its own:

<!-- snippet: examples/todo/internal/server/server.go#observer -->
```go
// NewObserver is the registry's Observer (D49): every outcome feeds the metrics; an outcome
// without a request's wide event (a socket frame, a re-run, the feed, a heartbeat: the socket
// route takes the upgrade's event out of their context) is a unit of work of its own and gets
// one event. A REST outcome is on the request's event already, which the REST adapter recorded.
func NewObserver(metrics *prometheusadapter.Metrics, emitter wideevent.Emitter) livewire.Observer {
    return func(ctx context.Context, outcome livewire.Outcome) {
        metrics.Observe(ctx, outcome)
        if wideevent.FromContext(ctx) != nil {
            return
        }
        we := wideevent.Begin(emitter, "livewire."+string(outcome.Kind))
        we.String("livewire.kind", string(outcome.Kind)).
            String("livewire.operation", string(outcome.Operation)).
            String("livewire.target", outcome.Target).
            String("livewire.subscription", outcome.Subscription).
            String("livewire.tenant", outcome.Tenant).
            Int("livewire.status", outcome.Status).
            Duration("livewire.duration", outcome.Duration).
            Duration("livewire.query_wait", outcome.QueryWait)
        if outcome.Identity != nil {
            we.UserID(outcome.Identity.UserID)
        }
        recordIdentifiers(we, outcome.Parameters)
        if outcome.Err != nil {
            we.Failure(outcome.Err)
        } else {
            we.Success()
        }
        we.Emit()
    }
}
```
