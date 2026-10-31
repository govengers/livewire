package postgresadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/govengers/auth"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
)

// AuditEntry is one mutation call as the audit of D32 records it: the tenant, the effective
// identity (its Real() is the real actor), the mutation, its decoded parameters, the
// request id, the Idempotency-Key ("" for none), the answered status and how long the call
// took.
type AuditEntry struct {
	Tenant         string
	Identity       *auth.Identity
	Mutation       string
	Parameters     any
	RequestID      string
	IdempotencyKey string
	Status         int
	Duration       time.Duration
}

// AuditStoreOptions configures an AuditStore. Pool is required; Retention (default 365
// days, not negative) is how long Maintain keeps rows.
type AuditStoreOptions struct {
	Pool      *pgxpool.Pool
	Retention time.Duration
}

const defaultAuditRetention = 365 * 24 * time.Hour

// AuditStore writes livewire_audit (AuditSQL). It is safe for concurrent use.
type AuditStore struct {
	options AuditStoreOptions
}

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
// back with the mutation (D31); outside one, a failed call's row for instance, it writes on
// the pool. Parameters are stored as their JSON.
func (s *AuditStore) Write(ctx context.Context, entry *AuditEntry) error {
	parameters, err := json.Marshal(entry.Parameters)
	if err != nil {
		return fmt.Errorf("postgresadapter: encode the audit parameters of %s: %w", entry.Mutation, err)
	}
	var database DBTX = s.options.Pool
	if transaction := TransactionFrom(ctx); transaction != nil {
		database = transaction
	}
	_, err = database.Exec(ctx, `INSERT INTO livewire_audit (tenant, actor, real_actor, mutation, request_id, idempotency_key, parameters, status, duration_ms)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, NULLIF($6, '')::uuid, $7::jsonb, $8, $9)`,
		entry.Tenant, livewire.OwnerKey(entry.Identity), livewire.OwnerKey(entry.Identity.Real()), entry.Mutation, entry.RequestID,
		entry.IdempotencyKey, parameters, entry.Status, entry.Duration.Milliseconds())
	if err != nil {
		return fmt.Errorf("postgresadapter: write the audit row of %s: %w", entry.Mutation, err)
	}
	return nil
}

// Maintain runs livewire_audit_maintain: it creates the partitions of this and the next
// month and removes the rows older than Retention. The application runs it periodically, at
// least monthly; without it every row stays in the default partition.
func (s *AuditStore) Maintain(ctx context.Context) error {
	if _, err := s.options.Pool.Exec(ctx, "SELECT livewire_audit_maintain(make_interval(secs => $1))", s.options.Retention.Seconds()); err != nil {
		return fmt.Errorf("postgresadapter: maintain the audit partitions: %w", err)
	}
	return nil
}
