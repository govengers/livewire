package livewire

import "context"

type contextKey int

const (
	transactionKey contextKey = iota
	idempotencyKeyKey
	tenantKey
)

// WithTransaction returns a copy of ctx carrying the query handle of the engine's transaction
// (Transaction.Handle); the engine calls it before a handler runs (D31). Only the engine
// holds the Transaction itself, so a handler cannot commit or roll back.
func WithTransaction(ctx context.Context, handle any) context.Context {
	return context.WithValue(ctx, transactionKey, handle)
}

// TransactionFrom returns the query handle of the mutation transaction in ctx, or nil outside
// a mutation, so a repository falls back to its pool. Its type is the adapter's: for
// postgresadapter a pgx.Tx, which `postgresadapter.TransactionFrom(ctx)` returns typed.
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
