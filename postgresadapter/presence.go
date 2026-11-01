package postgresadapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
)

// PresenceStoreOptions configures a PresenceStore; Pool is required.
type PresenceStoreOptions struct {
	Pool *pgxpool.Pool
}

// maximumPresences bounds one record's presence list.
const maximumPresences = 1000

// PresenceStore keeps the presences of D37 in livewire_presence (PresenceSQL), whose
// triggers carry every join, leave and state change to the engine through the Listener;
// a heartbeat changes updated_at only and notifies nobody. It implements
// livewire.PresenceStore; its calls run on the pool, outside any engine transaction (D50).
// The database clock stamps updated_at. It is safe for concurrent use.
type PresenceStore struct {
	options PresenceStoreOptions
}

// NewPresenceStore validates and copies options.
func NewPresenceStore(options *PresenceStoreOptions) (*PresenceStore, error) {
	if options == nil || options.Pool == nil {
		return nil, errors.New("postgresadapter: NewPresenceStore needs options with a Pool")
	}
	return &PresenceStore{options: *options}, nil
}

// Join inserts the socket's presence on the record of presence.Owner.Tenant, or updates its
// owner, display and state; presence.UpdatedAt is ignored.
func (s *PresenceStore) Join(ctx context.Context, presence *livewire.Presence) error {
	state := []byte(presence.State)
	if len(state) == 0 {
		state = []byte("null")
	}
	_, err := s.options.Pool.Exec(ctx, `INSERT INTO livewire_presence (tenant, target, key, socket, owner, display, state)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		ON CONFLICT (tenant, target, key, socket) DO UPDATE
		SET owner = EXCLUDED.owner, display = EXCLUDED.display, state = EXCLUDED.state, updated_at = now()`,
		presence.Owner.Tenant, presence.Target, presence.Key, presence.Socket, presence.Owner.Key, presence.Display, state)
	if err != nil {
		return fmt.Errorf("postgresadapter: join %s %q: %w", presence.Target, presence.Key, err)
	}
	return nil
}

// Leave removes the socket's presence on the tenant's record; an absent one is no error.
func (s *PresenceStore) Leave(ctx context.Context, tenant, socket, target, key string) error {
	_, err := s.options.Pool.Exec(ctx, "DELETE FROM livewire_presence WHERE tenant = $1 AND target = $2 AND key = $3 AND socket = $4",
		tenant, target, key, socket)
	if err != nil {
		return fmt.Errorf("postgresadapter: leave %s %q: %w", target, key, err)
	}
	return nil
}

// Heartbeat refreshes updated_at of every presence of the socket.
func (s *PresenceStore) Heartbeat(ctx context.Context, socket string) error {
	if _, err := s.options.Pool.Exec(ctx, "UPDATE livewire_presence SET updated_at = now() WHERE socket = $1", socket); err != nil {
		return fmt.Errorf("postgresadapter: heartbeat of socket %s: %w", socket, err)
	}
	return nil
}

// List returns at most 1000 presences on the tenant's record, by socket.
func (s *PresenceStore) List(ctx context.Context, tenant, target, key string) ([]livewire.Presence, error) {
	rows, err := s.options.Pool.Query(ctx, `SELECT tenant, target, key, socket, owner, display, state, updated_at FROM livewire_presence
		WHERE tenant = $1 AND target = $2 AND key = $3 ORDER BY socket LIMIT $4`,
		tenant, target, key, maximumPresences)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: list presences on %s %q: %w", target, key, err)
	}
	presences, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (livewire.Presence, error) {
		var presence livewire.Presence
		var state []byte
		err := row.Scan(&presence.Owner.Tenant, &presence.Target, &presence.Key, &presence.Socket, &presence.Owner.Key,
			&presence.Display, &state, &presence.UpdatedAt)
		presence.State = state
		return presence, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: list presences on %s %q: %w", target, key, err)
	}
	return presences, nil
}

// Sweep removes every presence not refreshed for maximumAge and returns how many.
func (s *PresenceStore) Sweep(ctx context.Context, maximumAge time.Duration) (int, error) {
	tag, err := s.options.Pool.Exec(ctx, "DELETE FROM livewire_presence WHERE updated_at < now() - make_interval(secs => $1)", maximumAge.Seconds())
	if err != nil {
		return 0, fmt.Errorf("postgresadapter: sweep presences: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
