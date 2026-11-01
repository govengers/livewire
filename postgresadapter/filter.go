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
// bound (D54); neither may be negative.
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

// Save stores filter and returns the stored row; the Saves of one owner run one after the
// other, so the maximums hold exactly. An unnamed filter is deduplicated on the owner,
// resource, context and hash, and marked used; a new one beyond MaximumUnnamedPerOwner
// replaces the owner's least recently used unnamed filter, because unnamed filters are
// registrations a client can send again (D54). A named filter beyond MaximumNamedPerOwner is
// refused with livewire.Invalid at "filter", a name the owner already gave a filter of the
// resource with livewire.Conflict, a Name or Context longer than 200 characters with
// livewire.Invalid.
func (s *FilterStore) Save(ctx context.Context, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	if err := validateFilter(filter); err != nil {
		return nil, err
	}
	save := s.saveUnnamed
	if filter.Name != "" {
		save = s.saveNamed
	}
	var stored *livewire.StoredFilter
	err := pgx.BeginFunc(ctx, s.options.Pool, func(transaction pgx.Tx) error {
		_, err := transaction.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(json_build_array('livewire_filters', $1::text, $2::text)::text, 0))",
			filter.Owner.Key, filter.Owner.Tenant)
		if err != nil {
			return err
		}
		stored, err = save(ctx, transaction, filter)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: save a filter: %w", err)
	}
	return stored, nil
}

// Get returns the owner's filter and marks it used at most once per hour; another owner's
// filter, an unknown id and an id that is not a uuid are livewire.NotFound.
func (s *FilterStore) Get(ctx context.Context, owner livewire.Owner, id string) (*livewire.StoredFilter, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, livewire.NotFound()
	}
	row := s.options.Pool.QueryRow(ctx, "SELECT "+filterColumns+" FROM livewire_filters WHERE id = $1 AND owner = $2 AND tenant = $3",
		parsed, owner.Key, owner.Tenant)
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
		parsed, filterTouchInterval.Seconds())
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
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil //nolint:nilerr // an id that is not a uuid names no filter, which is no error
	}
	if _, err := s.options.Pool.Exec(ctx, "DELETE FROM livewire_filters WHERE id = $1 AND owner = $2 AND tenant = $3", parsed, owner.Key, owner.Tenant); err != nil {
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

// saveUnnamed marks the owner's filter of the document used or inserts it, making room
// first; it runs under the owner's lock.
func (s *FilterStore) saveUnnamed(ctx context.Context, transaction pgx.Tx, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	stored, err := scanFilter(transaction.QueryRow(ctx, `UPDATE livewire_filters SET last_used_at = now()
		WHERE owner = $1 AND tenant = $2 AND resource = $3 AND context = $4 AND document_hash = $5 AND name IS NULL
		RETURNING `+filterColumns,
		filter.Owner.Key, filter.Owner.Tenant, filter.Resource, filter.Context, filter.Hash))
	if !errors.Is(err, pgx.ErrNoRows) {
		return stored, err
	}
	_, err = transaction.Exec(ctx, `DELETE FROM livewire_filters WHERE id IN (
		SELECT id FROM livewire_filters WHERE owner = $1 AND tenant = $2 AND name IS NULL ORDER BY last_used_at DESC, id DESC OFFSET $3)`,
		filter.Owner.Key, filter.Owner.Tenant, s.options.MaximumUnnamedPerOwner-1)
	if err != nil {
		return nil, err
	}
	return scanFilter(transaction.QueryRow(ctx, `INSERT INTO livewire_filters (owner, tenant, resource, context, document, document_hash)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6) RETURNING `+filterColumns,
		filter.Owner.Key, filter.Owner.Tenant, filter.Resource, filter.Context, []byte(filter.Document), filter.Hash))
}

// saveNamed inserts a named filter below the owner's maximum; it runs under the owner's lock.
func (s *FilterStore) saveNamed(ctx context.Context, transaction pgx.Tx, filter *livewire.StoredFilter) (*livewire.StoredFilter, error) {
	var kept int
	err := transaction.QueryRow(ctx, "SELECT count(*) FROM livewire_filters WHERE owner = $1 AND tenant = $2 AND name IS NOT NULL",
		filter.Owner.Key, filter.Owner.Tenant).Scan(&kept)
	if err != nil {
		return nil, err
	}
	if kept >= s.options.MaximumNamedPerOwner {
		return nil, livewire.Invalid(map[string]string{"filter": "at most " + strconv.Itoa(s.options.MaximumNamedPerOwner) + " named filters per owner"})
	}
	stored, err := scanFilter(transaction.QueryRow(ctx, `INSERT INTO livewire_filters (owner, tenant, resource, context, name, document, document_hash)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7) RETURNING `+filterColumns,
		filter.Owner.Key, filter.Owner.Tenant, filter.Resource, filter.Context, filter.Name, []byte(filter.Document), filter.Hash))
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == uniqueViolation {
		return nil, livewire.Conflict()
	}
	return stored, err
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
