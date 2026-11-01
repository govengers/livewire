package postgresadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
)

// DBTX is the query method set of a pgx connection, pool or transaction: a superset of
// sqlc's pgx/v5 DBTX (with :copyfrom and :batch queries included), so `sqlc.New(handle)`
// takes it, and of govengers/jobs' postgresadapter.Executor. *pgxpool.Pool, *pgx.Conn and
// pgx.Tx satisfy it; the engine's query-only handle (TransactionFrom) does too and has no
// Commit or Rollback (D45). It types the handle, it does not sandbox it: a statement like
// COMMIT still reaches the server.
type DBTX interface {
	Exec(ctx context.Context, statement string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, statement string, arguments ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, statement string, arguments ...any) pgx.Row
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults
}

// TransactionFrom is the query handle of the engine transaction in ctx (D45), nil outside an
// engine unit of work, where a repository falls back to its pool:
//
//	database := postgresadapter.TransactionFrom(ctx)
//	if database == nil {
//		database = r.pool
//	}
//	row, err := sqlc.New(database).InsertPart(ctx, parameters)
func TransactionFrom(ctx context.Context) DBTX {
	handle, _ := livewire.TransactionFrom(ctx).(DBTX)
	return handle
}

// TransactorOptions configures a Transactor; Pool is required.
type TransactorOptions struct {
	Pool *pgxpool.Pool
}

// Transactor begins the engine's transactions on a pool (D31, D42, D45): a read is
// REPEATABLE READ READ ONLY, a write READ COMMITTED READ WRITE, whatever the server's or
// role's default_transaction_isolation is. Both set
// livewire.tenant (the tenant, "" with a single tenant) and a write sets livewire.actor, a
// JSON object `{"actor", "realActor", "mutation", "requestId"}` (owner keys
// "provider:subject", absent members omitted) with SET LOCAL semantics, so triggers and Row
// Level Security policies read them with current_setting('livewire.tenant', true) and both
// are gone after the transaction. It is safe for concurrent use.
type Transactor struct {
	options TransactorOptions
}

// NewTransactor validates and copies options.
func NewTransactor(options *TransactorOptions) (*Transactor, error) {
	if options == nil || options.Pool == nil {
		return nil, errors.New("postgresadapter: NewTransactor needs options with a Pool")
	}
	return &Transactor{options: *options}, nil
}

// actorSetting is the JSON of livewire.actor that HistorySQL's trigger reads.
type actorSetting struct {
	Actor     string `json:"actor,omitempty"`
	RealActor string `json:"realActor,omitempty"`
	Mutation  string `json:"mutation,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Begin begins a transaction for transactionDetails and sets its settings; it implements
// livewire.Transactor.
func (t *Transactor) Begin(ctx context.Context, transactionDetails livewire.TransactionDetails) (livewire.Transaction, error) {
	options := pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}
	actor := ""
	if transactionDetails.ReadOnly {
		options = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	} else {
		encoded, err := json.Marshal(actorSetting{
			Actor:     livewire.OwnerKey(transactionDetails.Identity),
			RealActor: livewire.OwnerKey(transactionDetails.Identity.Real()),
			Mutation:  transactionDetails.Mutation,
			RequestID: transactionDetails.RequestID,
		})
		if err != nil {
			return nil, fmt.Errorf("postgresadapter: encode livewire.actor: %w", err)
		}
		actor = string(encoded)
	}
	transaction, err := t.options.Pool.BeginTx(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: begin a transaction: %w", err)
	}
	_, err = transaction.Exec(ctx, "SELECT set_config('livewire.tenant', $1, true), set_config('livewire.actor', $2, true)",
		transactionDetails.Tenant, actor)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("postgresadapter: set the transaction's settings: %w", err), transaction.Rollback(ctx))
	}
	return &engineTransaction{transaction: transaction}, nil
}

// engineTransaction is the livewire.Transaction of a Transactor: the engine holds it, the
// unit of work's context holds its queryHandle.
type engineTransaction struct {
	transaction pgx.Tx
}

func (t *engineTransaction) Commit(ctx context.Context) error {
	if err := t.transaction.Commit(ctx); err != nil {
		return fmt.Errorf("postgresadapter: commit: %w", err)
	}
	return nil
}

func (t *engineTransaction) Rollback(ctx context.Context) error {
	if err := t.transaction.Rollback(ctx); err != nil {
		return fmt.Errorf("postgresadapter: roll back: %w", err)
	}
	return nil
}

func (t *engineTransaction) Handle() any { return queryHandle{transaction: t.transaction} }

// queryHandle is the query-only view of an engine transaction (D45).
type queryHandle struct {
	transaction pgx.Tx
}

func (h queryHandle) Exec(ctx context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	return h.transaction.Exec(ctx, statement, arguments...)
}

func (h queryHandle) Query(ctx context.Context, statement string, arguments ...any) (pgx.Rows, error) {
	return h.transaction.Query(ctx, statement, arguments...)
}

func (h queryHandle) QueryRow(ctx context.Context, statement string, arguments ...any) pgx.Row {
	return h.transaction.QueryRow(ctx, statement, arguments...)
}

func (h queryHandle) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	return h.transaction.CopyFrom(ctx, table, columns, source)
}

func (h queryHandle) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	return h.transaction.SendBatch(ctx, batch)
}
