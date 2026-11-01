package livewire

import (
	"context"
	"sync/atomic"
	"time"
)

type contextKey int

const (
	transactionKey contextKey = iota
	idempotencyKeyKey
	tenantKey
	queryWaitKey
)

// queryWait sums the time one unit of work waited for query slots (D52).
type queryWait struct {
	nanoseconds atomic.Int64
}

// withQueryWait returns a copy of ctx that sums the slot waits of the unit of work it starts.
func withQueryWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, queryWaitKey, &queryWait{})
}

// addQueryWait adds a slot wait to the sum of ctx, if it has one.
func addQueryWait(ctx context.Context, wait time.Duration) {
	if sum, ok := ctx.Value(queryWaitKey).(*queryWait); ok {
		sum.nanoseconds.Add(int64(wait))
	}
}

// queryWaitOf is the slot wait summed in ctx, zero without a sum.
func queryWaitOf(ctx context.Context) time.Duration {
	sum, ok := ctx.Value(queryWaitKey).(*queryWait)
	if !ok {
		return 0
	}
	return time.Duration(sum.nanoseconds.Load())
}

// WithTransaction returns a copy of ctx carrying the query-only handle of the engine's
// transaction (Transaction.Handle); the engine calls it for every unit of work, reads
// included (D45), before a loader or handler runs. Only the engine holds the Transaction
// itself, so nothing in the context can commit or roll back.
func WithTransaction(ctx context.Context, handle any) context.Context {
	return context.WithValue(ctx, transactionKey, handle)
}

// TransactionFrom returns the query-only handle of the engine transaction in ctx, or nil
// outside an engine unit of work, so a repository falls back to its pool. Its type is the
// adapter's: postgresadapter.TransactionFrom(ctx) types it as sqlc's DBTX (D45).
func TransactionFrom(ctx context.Context) any {
	return ctx.Value(transactionKey)
}

// WithIdempotencyKey returns a copy of ctx carrying the Idempotency-Key of the call.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKeyKey, key)
}

// IdempotencyKeyFrom returns the Idempotency-Key of the mutation in ctx, or "" outside one.
func IdempotencyKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyKey).(string)
	return key
}

// WithTenant returns a copy of ctx carrying the tenant the registry's Tenant function
// resolved (D42); the engine calls it once per request or subscription after the static
// check.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// TenantFrom returns the tenant of the request in ctx, "" in a single-tenant registry or
// outside a request.
func TenantFrom(ctx context.Context) string {
	tenant, _ := ctx.Value(tenantKey).(string)
	return tenant
}
