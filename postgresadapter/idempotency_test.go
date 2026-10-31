package postgresadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
)

var _ livewire.IdempotencyStore = (*postgresadapter.IdempotencyStore)(nil)

const idempotencyKey = "0193a8f0-0000-7000-8000-00000000000a"

// inTransaction begins a write transaction and returns the context carrying its handle.
func inTransaction(t *testing.T, transactor *postgresadapter.Transactor) (context.Context, livewire.Transaction) {
	t.Helper()
	transaction, err := transactor.Begin(t.Context(), livewire.TransactionDetails{Mutation: "createPart"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = transaction.Rollback(context.Background()) }) // ended already on most paths; ErrTxClosed then
	return livewire.WithTransaction(t.Context(), transaction.Handle()), transaction
}

func newIdempotencyStore(t *testing.T, database *testDatabase) *postgresadapter.IdempotencyStore {
	t.Helper()
	store, err := postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	return store
}

func requireCode(t *testing.T, err error, code livewire.Code) {
	t.Helper()
	_, body := livewire.ResponseFor(err)
	require.Equal(t, code, body.Code, "%v", err)
}

func TestIdempotencyReservesWithoutBlockingAndReplaysAfterCommit(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newIdempotencyStore(t, database)
	transactor := newTransactor(t, database.pool)
	alice := livewire.Owner{Key: "password:alice", Tenant: "north"}

	first, firstTransaction := inTransaction(t, transactor)
	record, replay, err := store.Reserve(first, alice, idempotencyKey, "createPart")
	require.NoError(t, err)
	require.False(t, replay)
	require.Equal(t, &livewire.IdempotencyRecord{Owner: alice, Key: idempotencyKey, Mutation: "createPart"}, record)

	second, secondTransaction := inTransaction(t, transactor)
	bounded, cancel := context.WithTimeout(second, 2*time.Second)
	defer cancel()
	_, _, err = store.Reserve(bounded, alice, idempotencyKey, "createPart")
	requireCode(t, err, livewire.CodeInProgress)
	require.NoError(t, bounded.Err(), "the second reservation answered without waiting for the first")
	require.NoError(t, secondTransaction.Rollback(t.Context()))

	other, otherTransaction := inTransaction(t, transactor)
	_, replay, err = store.Reserve(other, livewire.Owner{Key: "password:alice", Tenant: "south"}, idempotencyKey, "createPart")
	require.NoError(t, err)
	require.False(t, replay, "the key of another owner is a fresh key")
	require.NoError(t, otherTransaction.Rollback(t.Context()))

	record.Status, record.Response = http.StatusCreated, json.RawMessage(`{"id":"7"}`)
	require.NoError(t, store.Record(first, record))
	require.NoError(t, firstTransaction.Commit(t.Context()))

	again, againTransaction := inTransaction(t, transactor)
	replayed, replay, err := store.Reserve(again, alice, idempotencyKey, "createPart")
	require.NoError(t, err)
	require.True(t, replay)
	require.Equal(t, http.StatusCreated, replayed.Status)
	require.JSONEq(t, `{"id":"7"}`, string(replayed.Response))
	_, _, err = store.Reserve(again, alice, idempotencyKey, "deletePart")
	requireCode(t, err, livewire.CodeConflict)
	require.NoError(t, againTransaction.Rollback(t.Context()))
}

func TestIdempotencyKeyIsFreeAfterARollback(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newIdempotencyStore(t, database)
	transactor := newTransactor(t, database.pool)
	alice := livewire.Owner{Key: "password:alice"}

	first, firstTransaction := inTransaction(t, transactor)
	_, _, err := store.Reserve(first, alice, idempotencyKey, "createPart")
	require.NoError(t, err)
	require.NoError(t, firstTransaction.Rollback(t.Context()))

	second, secondTransaction := inTransaction(t, transactor)
	_, replay, err := store.Reserve(second, alice, idempotencyKey, "createPart")
	require.NoError(t, err)
	require.False(t, replay)
	require.NoError(t, store.Record(second, &livewire.IdempotencyRecord{Owner: alice, Key: idempotencyKey, Mutation: "createPart", Status: http.StatusNoContent}))
	require.NoError(t, secondTransaction.Commit(t.Context()))

	third, _ := inTransaction(t, transactor)
	replayed, replay, err := store.Reserve(third, alice, idempotencyKey, "createPart")
	require.NoError(t, err)
	require.True(t, replay)
	require.Empty(t, replayed.Response, "a 204 has no body")
}

func TestIdempotencyStoreNeedsTheEngineTransaction(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newIdempotencyStore(t, database)
	_, _, err := store.Reserve(t.Context(), livewire.Owner{Key: "password:alice"}, idempotencyKey, "createPart")
	require.Error(t, err)
	require.Error(t, store.Record(t.Context(), &livewire.IdempotencyRecord{Key: idempotencyKey}))

	ctx, _ := inTransaction(t, newTransactor(t, database.pool))
	err = store.Record(ctx, &livewire.IdempotencyRecord{Owner: livewire.Owner{Key: "password:alice"}, Key: idempotencyKey, Status: http.StatusOK})
	require.ErrorContains(t, err, "not reserved")
}

func TestIdempotencySweepRemovesRecordsOutsideTheWindow(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newIdempotencyStore(t, database)
	_, err := database.pool.Exec(t.Context(), `INSERT INTO livewire_idempotency (owner, key, mutation, status, created_at) VALUES
		('password:alice', '0193a8f0-0000-7000-8000-000000000001', 'createPart', 201, now() - interval '2 days'),
		('password:alice', '0193a8f0-0000-7000-8000-000000000002', 'createPart', 201, now())`)
	require.NoError(t, err)
	swept, err := store.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, swept)

	_, err = postgresadapter.NewIdempotencyStore(nil)
	require.Error(t, err)
	_, err = postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: database.pool, Window: -time.Second})
	require.Error(t, err)
}
