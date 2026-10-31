package livewire

import "context"

type contextKey int

const (
	transactionKey contextKey = iota
	idempotencyKeyKey
)

// WithTransaction returns a copy of ctx carrying the mutation transaction; the engine calls
// it before the handler runs (D31).
func WithTransaction(ctx context.Context, transaction Transaction) context.Context {
	return context.WithValue(ctx, transactionKey, transaction)
}

// TransactionFrom returns the mutation transaction in ctx, or nil outside a mutation, so a
// repository falls back to its pool. The adapter that began it offers the typed view
// (postgresadapter hands out the pgx.Tx).
func TransactionFrom(ctx context.Context) Transaction {
	transaction, _ := ctx.Value(transactionKey).(Transaction)
	return transaction
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
