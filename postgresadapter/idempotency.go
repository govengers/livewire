package postgresadapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
)

// The SQLSTATEs this package maps.
const (
	uniqueViolation           = "23505"
	invalidTextRepresentation = "22P02"
)

// errNoTransaction is the failure of a store call that must run in the engine's transaction
// but finds none in its context.
var errNoTransaction = errors.New("postgresadapter: no engine transaction in the context (postgresadapter.TransactionFrom)")

// IdempotencyStoreOptions configures an IdempotencyStore. Pool is required and serves Sweep
// only; Window (default 24 h, not negative) is how long Sweep keeps a record.
type IdempotencyStoreOptions struct {
	Pool   *pgxpool.Pool
	Window time.Duration
}

const defaultIdempotencyWindow = 24 * time.Hour

// IdempotencyStore keeps the records of D16 in livewire_idempotency (IdempotencySQL) and
// implements livewire.IdempotencyStore. Reserve and Record run in the engine's transaction
// (TransactionFrom), so a rolled-back call frees its key. It is safe for concurrent use.
type IdempotencyStore struct {
	options IdempotencyStoreOptions
}

// NewIdempotencyStore validates and copies options.
func NewIdempotencyStore(options *IdempotencyStoreOptions) (*IdempotencyStore, error) {
	if options == nil || options.Pool == nil {
		return nil, errors.New("postgresadapter: NewIdempotencyStore needs options with a Pool")
	}
	resolved := *options
	if resolved.Window < 0 {
		return nil, errors.New("postgresadapter: IdempotencyStoreOptions.Window must not be negative")
	}
	if resolved.Window == 0 {
		resolved.Window = defaultIdempotencyWindow
	}
	return &IdempotencyStore{options: resolved}, nil
}

// Reserve claims key for owner and mutation without blocking: it takes
// pg_try_advisory_xact_lock over a hash of owner and key, so a key another transaction holds
// is livewire.InProgress at once; a committed record is replayed (livewire.Conflict when it
// was recorded for another mutation); a fresh key is inserted as in flight. The lock and the
// reservation end with the transaction. Key is a uuid in any spelling uuid.Parse reads; the
// record carries its canonical form, so every spelling of one key is the same key.
func (s *IdempotencyStore) Reserve(ctx context.Context, owner livewire.Owner, key, mutation string) (*livewire.IdempotencyRecord, bool, error) {
	transaction := TransactionFrom(ctx)
	if transaction == nil {
		return nil, false, errNoTransaction
	}
	parsed, err := uuid.Parse(key)
	if err != nil {
		return nil, false, livewire.Invalid(map[string]string{"idempotencyKey": "must be a uuid"})
	}
	key = parsed.String()
	var locked bool
	err = transaction.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended(json_build_array($1::text, $2::text, $3::text)::text, 0))",
		owner.Key, owner.Tenant, key).Scan(&locked)
	if err != nil {
		return nil, false, fmt.Errorf("postgresadapter: lock idempotency key %s: %w", key, err)
	}
	if !locked {
		return nil, false, livewire.InProgress()
	}
	record := &livewire.IdempotencyRecord{Owner: owner, Key: key}
	var response []byte
	err = transaction.QueryRow(ctx, "SELECT mutation, status, response FROM livewire_idempotency WHERE owner = $1 AND tenant = $2 AND key = $3",
		owner.Key, owner.Tenant, key).Scan(&record.Mutation, &record.Status, &response)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return s.insertReservation(ctx, transaction, owner, key, mutation)
	case err != nil:
		return nil, false, fmt.Errorf("postgresadapter: read idempotency key %s: %w", key, err)
	case record.Mutation != mutation:
		return nil, false, livewire.Conflict()
	case record.Status == 0:
		return nil, false, livewire.InProgress() // committed without an answer: the engine never does that
	}
	record.Response = response
	return record, true, nil
}

// Record stores the status and response of a key Reserve claimed in the same transaction;
// record.Key is the key in any spelling Reserve takes.
func (s *IdempotencyStore) Record(ctx context.Context, record *livewire.IdempotencyRecord) error {
	transaction := TransactionFrom(ctx)
	if transaction == nil {
		return errNoTransaction
	}
	key, err := uuid.Parse(record.Key)
	if err != nil {
		return fmt.Errorf("postgresadapter: record idempotency key %q: %w", record.Key, err)
	}
	var response []byte
	if len(record.Response) > 0 {
		response = record.Response
	}
	tag, err := transaction.Exec(ctx, "UPDATE livewire_idempotency SET status = $4, response = $5::jsonb WHERE owner = $1 AND tenant = $2 AND key = $3",
		record.Owner.Key, record.Owner.Tenant, key, record.Status, response)
	if err != nil {
		return fmt.Errorf("postgresadapter: record idempotency key %s: %w", record.Key, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("postgresadapter: record idempotency key %s: it was not reserved in this transaction", record.Key)
	}
	return nil
}

// Sweep removes the records older than Window and returns how many; the application runs
// it periodically.
func (s *IdempotencyStore) Sweep(ctx context.Context) (int, error) {
	tag, err := s.options.Pool.Exec(ctx, "DELETE FROM livewire_idempotency WHERE created_at < now() - make_interval(secs => $1)",
		s.options.Window.Seconds())
	if err != nil {
		return 0, fmt.Errorf("postgresadapter: sweep idempotency records: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *IdempotencyStore) insertReservation(ctx context.Context, transaction DBTX, owner livewire.Owner, key, mutation string) (*livewire.IdempotencyRecord, bool, error) {
	_, err := transaction.Exec(ctx, "INSERT INTO livewire_idempotency (owner, tenant, key, mutation) VALUES ($1, $2, $3, $4)",
		owner.Key, owner.Tenant, key, mutation)
	if err != nil {
		return nil, false, fmt.Errorf("postgresadapter: reserve idempotency key %s: %w", key, err)
	}
	return &livewire.IdempotencyRecord{Owner: owner, Key: key, Mutation: mutation}, false, nil
}
