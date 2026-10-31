package postgresadapter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
)

// FilterStoreOptions configures a FilterStore. Pool is required. MaximumUnnamedPerOwner
// (default 1000) and MaximumNamedPerOwner (default 200) bound the filters one owner (key and
// tenant) keeps, so neither inline subscriptions nor POST /filters grow the table without
// bound; neither may be negative.
type FilterStoreOptions struct {
	Pool                   *pgxpool.Pool
	MaximumUnnamedPerOwner int
	MaximumNamedPerOwner   int
}

const (
	defaultMaximumUnnamedPerOwner = 1000
	defaultMaximumNamedPerOwner   = 200
	// filterTouchInterval is how stale last_used_at may get before Get writes it again: at
	// most one write per filter and hour, whatever the traffic.
	filterTouchInterval = time.Hour
	// unnamedFilterExpiry is how long an unnamed filter survives without use (D13).
	unnamedFilterExpiry = 30 * 24 * time.Hour
	// maximumFilterLabelLength bounds Name and Context in characters, as FiltersSQL does.
	maximumFilterLabelLength = 200
)

// FilterStore keeps the stored filters of D13 in livewire_filters (FiltersSQL) and
// implements livewire.FilterStore. Its calls run on the pool, outside the engine's read
// transaction (D50). It is safe for concurrent use.
type FilterStore struct {
	options FilterStoreOptions
}

// NewFilterStore validates and copies options.
func NewFilterStore(options *FilterStoreOptions) (*FilterStore, error) {
	if options == nil || options.Pool == nil {
		return nil, errors.New("postgresadapter: NewFilterStore needs options with a Pool")
	}
	resolved := *options
	if resolved.MaximumUnnamedPerOwner < 0 || resolved.MaximumNamedPerOwner < 0 {
		return nil, errors.New("postgresadapter: FilterStoreOptions maximums must not be negative")
	}
	if resolved.MaximumUnnamedPerOwner == 0 {
		resolved.MaximumUnnamedPerOwner = defaultMaximumUnnamedPerOwner
	}
	if resolved.MaximumNamedPerOwner == 0 {
		resolved.MaximumNamedPerOwner = defaultMaximumNamedPerOwner
	}
	return &FilterStore{options: resolved}, nil
}

const filterColumns = "id::text, owner, tenant, resource, context, coalesce(name, ''), document, document_hash, created_at, last_used_at"

// Save stores filter and returns the stored row. An unnamed filter is deduplicated by one
// upsert on the owner, resource, context and hash, so concurrent Saves of one document get
// one row, which is marked used. A new filter beyond the owner's maximum is refused with
// livewire.Invalid at "filter"; a name the owner already gave a filter of the resource with
// livewire.Conflict; a Name or Context longer than 200 characters with livewire.Invalid.
func (s *FilterStore) Save(ctx context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	if filter.Name != "" {
		return s.saveNamed(ctx, filter)
	}
	return s.saveUnnamed(ctx, filter)
}

func validateFilter(filter *livewire.StoredFilter) error {
	switch {
	case filter == nil || filter.Owner.Key == "" || filter.Resource == "" || filter.Hash == "" || len(filter.Document) == 0:
		return errors.New("postgresadapter: a stored filter needs an owner, a resource, a document and its hash")
	case utf8.RuneCountInString(filter.Name) > maximumFilterLabelLength:
		return livewire.Invalid(map[string]string{"name": "at most " + strconv.Itoa(maximumFilterLabelLength) + " characters"})
	case utf8.RuneCountInString(filter.Context) > maximumFilterLabelLength:
		return livewire.Invalid(map[string]string{"context": "at most " + strconv.Itoa(maximumFilterLabelLength) + " characters"})
	}
	return nil
}

// ownerBelow is the condition of an insert under the owner's maximum: $1 owner, $2 tenant,
// $3 the maximum, named or not by the flag.
func ownerBelow(named bool) string {
	condition := "name IS NULL"
	if named {
		condition = "name IS NOT NULL"
	}
	return "(SELECT count(*) FROM (SELECT 1 FROM livewire_filters WHERE owner = $1 AND tenant = $2 AND " + condition + " LIMIT $3) AS kept) < $3"
}

// ponytail: the maximum is checked before the insert without a lock, so concurrent Saves of
// one owner may overshoot it by their number; an advisory lock per owner if it must be exact.
func (s *FilterStore) saveUnnamed(ctx context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	row := s.options.Pool.QueryRow(ctx, `INSERT INTO livewire_filters (owner, tenant, resource, context, document, document_hash)
		SELECT $1, $2, $4, $5, $6::jsonb, $7 WHERE `+ownerBelow(false)+`
		ON CONFLICT (owner, tenant, resource, context, document_hash) WHERE name IS NULL DO UPDATE SET last_used_at = now()
		RETURNING `+filterColumns,
		filter.Owner.Key, filter.Owner.Tenant, s.options.MaximumUnnamedPerOwner, filter.Resource, filter.Context, []byte(filter.Document), filter.Hash)
	stored, err := scanFilter(row)
	if !errors.Is(err, pgx.ErrNoRows) {
		return stored, wrapFilterError(err)
	}
	// At the maximum nothing was inserted, so nothing conflicted: an existing row of the
	// document is still the answer.
	row = s.options.Pool.QueryRow(ctx, `UPDATE livewire_filters SET last_used_at = now()
		WHERE owner = $1 AND tenant = $2 AND resource = $3 AND context = $4 AND document_hash = $5 AND name IS NULL
		RETURNING `+filterColumns,
		filter.Owner.Key, filter.Owner.Tenant, filter.Resource, filter.Context, filter.Hash)
	stored, err = scanFilter(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ownerFull(s.options.MaximumUnnamedPerOwner, "unnamed")
	}
	return stored, wrapFilterError(err)
}

func (s *FilterStore) saveNamed(ctx context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	row := s.options.Pool.QueryRow(ctx, `INSERT INTO livewire_filters (owner, tenant, resource, context, name, document, document_hash)
		SELECT $1, $2, $4, $5, $6, $7::jsonb, $8 WHERE `+ownerBelow(true)+`
		RETURNING `+filterColumns,
		filter.Owner.Key, filter.Owner.Tenant, s.options.MaximumNamedPerOwner, filter.Resource, filter.Context, filter.Name, []byte(filter.Document), filter.Hash)
	stored, err := scanFilter(row)
	var postgresError *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ownerFull(s.options.MaximumNamedPerOwner, "named")
	case errors.As(err, &postgresError) && postgresError.Code == uniqueViolation:
		return nil, livewire.Conflict()
	}
	return stored, wrapFilterError(err)
}

func wrapFilterError(err error) error {
	if err != nil {
		return fmt.Errorf("postgresadapter: save a filter: %w", err)
	}
	return nil
}

func ownerFull(maximum int, kind string) error {
	return livewire.Invalid(map[string]string{"filter": "at most " + strconv.Itoa(maximum) + " " + kind + " filters per owner"})
}

// Get returns the owner's filter and marks it used at most once per hour; another owner's
// filter, an unknown id and an id that is not a uuid are livewire.NotFound.
func (s *FilterStore) Get(ctx context.Context, owner livewire.Owner, id string) (*livewire.StoredFilter, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, livewire.NotFound()
	}
	row := s.options.Pool.QueryRow(ctx, "SELECT "+filterColumns+" FROM livewire_filters WHERE id = $1 AND owner = $2 AND tenant = $3",
		id, owner.Key, owner.Tenant)
	stored, err := scanFilter(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, livewire.NotFound()
	}
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: get filter %s: %w", id, err)
	}
	if time.Since(stored.LastUsedAt) < filterTouchInterval {
		return stored, nil
	}
	_, err = s.options.Pool.Exec(ctx, "UPDATE livewire_filters SET last_used_at = now() WHERE id = $1 AND last_used_at < now() - make_interval(secs => $2)",
		id, filterTouchInterval.Seconds())
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: mark filter %s used: %w", id, err)
	}
	return stored, nil
}

// List returns the owner's named filters of a resource, of every resource when resource is
// empty, by resource and name; unnamed filters are registrations, not the owner's list. It
// returns at most MaximumNamedPerOwner filters.
func (s *FilterStore) List(ctx context.Context, owner livewire.Owner, resource string) ([]livewire.StoredFilter, error) {
	rows, err := s.options.Pool.Query(ctx, "SELECT "+filterColumns+` FROM livewire_filters
		WHERE owner = $1 AND tenant = $2 AND ($3 = '' OR resource = $3) AND name IS NOT NULL
		ORDER BY resource, name LIMIT $4`,
		owner.Key, owner.Tenant, resource, s.options.MaximumNamedPerOwner)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: list filters: %w", err)
	}
	filters, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (livewire.StoredFilter, error) {
		stored, err := scanFilter(row)
		if err != nil {
			return livewire.StoredFilter{}, err
		}
		return *stored, nil
	})
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: list filters: %w", err)
	}
	return filters, nil
}

// Delete removes the owner's filter; a filter that is not there, or not the owner's, is no
// error.
func (s *FilterStore) Delete(ctx context.Context, owner livewire.Owner, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return nil //nolint:nilerr // an id that is not a uuid names no filter, which is no error
	}
	if _, err := s.options.Pool.Exec(ctx, "DELETE FROM livewire_filters WHERE id = $1 AND owner = $2 AND tenant = $3", id, owner.Key, owner.Tenant); err != nil {
		return fmt.Errorf("postgresadapter: delete filter %s: %w", id, err)
	}
	return nil
}

// Sweep removes the unnamed filters unused for 30 days (D13) and returns how many; the
// application runs it periodically, a job of govengers/jobs for instance.
func (s *FilterStore) Sweep(ctx context.Context) (int, error) {
	tag, err := s.options.Pool.Exec(ctx, "DELETE FROM livewire_filters WHERE name IS NULL AND last_used_at < now() - make_interval(secs => $1)",
		unnamedFilterExpiry.Seconds())
	if err != nil {
		return 0, fmt.Errorf("postgresadapter: sweep unused filters: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func scanFilter(row pgx.Row) (*livewire.StoredFilter, error) {
	var stored livewire.StoredFilter
	var document []byte
	err := row.Scan(&stored.ID, &stored.Owner.Key, &stored.Owner.Tenant, &stored.Resource, &stored.Context, &stored.Name,
		&document, &stored.Hash, &stored.CreatedAt, &stored.LastUsedAt)
	if err != nil {
		return nil, err
	}
	stored.Document = document
	return &stored, nil
}
