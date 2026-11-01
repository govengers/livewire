package postgresadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
)

// AuditStoreOptions configures an AuditStore. Pool is required; Retention (default 365
// days, not negative) is how long Maintain keeps rows at least: it drops a month's partition
// once the whole month is older than Retention, so a row stays up to a month longer.
type AuditStoreOptions struct {
	Pool      *pgxpool.Pool
	Retention time.Duration
}

const defaultAuditRetention = 365 * 24 * time.Hour

// AuditStore writes livewire_audit (AuditSQL) and implements livewire.AuditStore: the engine
// writes one row per call (D32) through EngineOptions.AuditStore. It is safe for concurrent
// use.
type AuditStore struct {
	options AuditStoreOptions
}

var _ livewire.AuditStore = (*AuditStore)(nil)

// NewAuditStore validates and copies options.
func NewAuditStore(options *AuditStoreOptions) (*AuditStore, error) {
	if options == nil || options.Pool == nil {
		return nil, errors.New("postgresadapter: NewAuditStore needs options with a Pool")
	}
	resolved := *options
	if resolved.Retention < 0 {
		return nil, errors.New("postgresadapter: AuditStoreOptions.Retention must not be negative")
	}
	if resolved.Retention == 0 {
		resolved.Retention = defaultAuditRetention
	}
	return &AuditStore{options: resolved}, nil
}

// Write inserts the row of entry in the engine's transaction in ctx, so it commits and rolls
// back with the mutation (D31); outside one, a failed call's row after the rollback, it
// writes on the pool. Parameters are stored as their JSON, the IdempotencyKey in any spelling
// uuid.Parse reads as its canonical uuid.
func (s *AuditStore) Write(ctx context.Context, entry *livewire.AuditEntry) error {
	parameters, err := json.Marshal(entry.Parameters)
	if err != nil {
		return fmt.Errorf("postgresadapter: encode the audit parameters of %s: %w", entry.Mutation, err)
	}
	var key *uuid.UUID
	if entry.IdempotencyKey != "" {
		parsed, err := uuid.Parse(entry.IdempotencyKey)
		if err != nil {
			return fmt.Errorf("postgresadapter: the audit row of %s: idempotency key %q: %w", entry.Mutation, entry.IdempotencyKey, err)
		}
		key = &parsed
	}
	var database DBTX = s.options.Pool
	if transaction := TransactionFrom(ctx); transaction != nil {
		database = transaction
	}
	_, err = database.Exec(ctx, `INSERT INTO livewire_audit (tenant, actor, real_actor, mutation, request_id, idempotency_key, parameters, status, duration_ms)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7::jsonb, $8, $9)`,
		entry.Tenant, livewire.OwnerKey(entry.Identity), livewire.OwnerKey(entry.Identity.Real()), entry.Mutation, entry.RequestID,
		key, parameters, entry.Status, entry.Duration.Milliseconds())
	if err != nil {
		return fmt.Errorf("postgresadapter: write the audit row of %s: %w", entry.Mutation, err)
	}
	return nil
}

// Maintain runs livewire_audit_maintain: it creates the partitions of this and the next UTC
// month, drops the partitions whose whole month is older than Retention and deletes the
// default partition's rows older than that, so rows go by the month, up to a month after
// their Retention. Audit writes and reads wait while it creates or drops a partition. The
// application runs it periodically, at least monthly; without it every row stays in the
// default partition.
func (s *AuditStore) Maintain(ctx context.Context) error {
	// READ COMMITTED whatever the default: the function's statements must see the rows
	// committed while it waited for its lock.
	err := pgx.BeginTxFunc(ctx, s.options.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}, func(transaction pgx.Tx) error {
		_, err := transaction.Exec(ctx, "SELECT livewire_audit_maintain(make_interval(secs => $1))", s.options.Retention.Seconds())
		return err
	})
	if err != nil {
		return fmt.Errorf("postgresadapter: maintain the audit partitions: %w", err)
	}
	return nil
}
