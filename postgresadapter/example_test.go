package postgresadapter_test

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

// The adapters an engine takes, built from one pool and the database URL (DES-001).
func Example() {
	ctx := context.Background()
	databaseURL := "postgres://app@localhost/app"
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return
	}
	defer pool.Close()
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: databaseURL})
	if err != nil {
		return
	}
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
	if err != nil {
		return
	}
	filters, err := postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: pool})
	if err != nil {
		return
	}
	idempotency, err := postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: pool})
	if err != nil {
		return
	}
	presence, err := postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: pool})
	if err != nil {
		return
	}
	_ = livewire.EngineOptions{
		ChangeFeed: listener, Transactor: transactor, FilterStore: filters, IdempotencyStore: idempotency, PresenceStore: presence,
	}
}

// A resource's List loader: the page on the engine's transaction (ListPage prefers it over
// the pool), hydrated by id; a cursor-mode resource encodes page.Next.
func ExampleListPage() {
	type Part struct {
		ID   uuid.UUID
		Name string
	}
	partSQL := &query.ResourceSQL{From: "parts", IDColumn: "parts.id"}
	hydrate := func(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]Part, error) {
		parts := map[uuid.UUID]Part{}
		result, err := database.Query(ctx, "SELECT id, name FROM parts WHERE id = ANY($1)", postgresadapter.PageIDs(rows))
		if err != nil {
			return nil, err
		}
		defer result.Close()
		for result.Next() {
			var part Part
			if err := result.Scan(&part.ID, &part.Name); err != nil {
				return nil, err
			}
			parts[part.ID] = part
		}
		return parts, result.Err()
	}
	var pool *pgxpool.Pool // the repository's pool
	listParts := func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[Part], error) {
		return postgresadapter.ListPage(ctx, pool, partSQL, listQuery, hydrate) // ResourceOptions.List
	}
	_ = listParts
}
