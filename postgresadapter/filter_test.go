package postgresadapter_test

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
)

func newFilterStore(t *testing.T, database *testDatabase, options postgresadapter.FilterStoreOptions) *postgresadapter.FilterStore {
	t.Helper()
	options.Pool = database.pool
	store, err := postgresadapter.NewFilterStore(&options)
	require.NoError(t, err)
	return store
}

var _ livewire.FilterStore = (*postgresadapter.FilterStore)(nil)

var alice = livewire.Owner{Key: "password:alice", Tenant: "north"}

func unnamed(hash string) *livewire.StoredFilter {
	return &livewire.StoredFilter{Owner: alice, Resource: "parts", Document: []byte(`{"search":"` + hash + `"}`), Hash: hash}
}

func TestFilterSaveDeduplicatesConcurrentUnnamedFilters(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	ids := make([]string, 8)
	var saves sync.WaitGroup
	for index := range ids {
		saves.Go(func() {
			stored, err := store.Save(t.Context(), unnamed("hash-1"))
			if err == nil {
				ids[index] = stored.ID
			}
		})
	}
	saves.Wait()
	for _, id := range ids {
		require.NotEmpty(t, id)
		require.Equal(t, ids[0], id)
	}
	other := unnamed("hash-1")
	other.Context = "dashboard"
	stored, err := store.Save(t.Context(), other)
	require.NoError(t, err)
	require.NotEqual(t, ids[0], stored.ID, "the context is part of the dedup key")

	got, err := store.Get(t.Context(), alice, ids[0])
	require.NoError(t, err)
	require.Equal(t, alice, got.Owner)
	require.Equal(t, "parts", got.Resource)
	require.Equal(t, "hash-1", got.Hash)
	require.JSONEq(t, `{"search":"hash-1"}`, string(got.Document))
	require.Empty(t, got.Name)
}

func TestFilterSaveBoundsTheFiltersOfAnOwner(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{MaximumUnnamedPerOwner: 2, MaximumNamedPerOwner: 1})
	saved := map[string]string{}
	for _, hash := range []string{"a", "b"} {
		stored, err := store.Save(t.Context(), unnamed(hash))
		require.NoError(t, err)
		saved[hash] = stored.ID
	}
	_, err := database.pool.Exec(t.Context(), "UPDATE livewire_filters SET last_used_at = now() - interval '1 day' WHERE document_hash = 'a'")
	require.NoError(t, err)
	again, err := store.Save(t.Context(), unnamed("b"))
	require.NoError(t, err)
	require.Equal(t, saved["b"], again.ID, "an existing filter is found at the maximum")
	_, err = store.Save(t.Context(), unnamed("c"))
	require.NoError(t, err, "a new unnamed filter at the maximum is never refused (D54)")
	_, err = store.Get(t.Context(), alice, saved["a"])
	requireCode(t, err, livewire.CodeNotFound) // the least recently used one made room
	_, err = store.Get(t.Context(), alice, saved["b"])
	require.NoError(t, err)
	bob := unnamed("d")
	bob.Owner = livewire.Owner{Key: "password:bob", Tenant: "north"}
	_, err = store.Save(t.Context(), bob)
	require.NoError(t, err)
	var kept int
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT count(*) FROM livewire_filters WHERE owner = $1 AND name IS NULL", alice.Key).Scan(&kept))
	require.Equal(t, 2, kept, "the maximum is per owner")

	named := unnamed("a")
	named.Name = "Mine"
	_, err = store.Save(t.Context(), named)
	require.NoError(t, err)
	named.Name = "Another"
	_, err = store.Save(t.Context(), named)
	requireCode(t, err, livewire.CodeInvalidRequest)
}

func TestFilterMaximumsHoldUnderConcurrentSaves(t *testing.T) {
	t.Parallel()
	for name, parameters := range map[string]string{"server default": "", "repeatable read default": "?default_transaction_isolation=repeatable%20read"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			database := newDatabase(t)
			pool, err := pgxpool.New(t.Context(), database.dsn()+parameters)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			store, err := postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: pool, MaximumUnnamedPerOwner: 1, MaximumNamedPerOwner: 1})
			require.NoError(t, err)
			var saves sync.WaitGroup
			var mutex sync.Mutex
			deduplicated := map[string]int{}
			var failures []error
			for range 8 {
				saves.Go(func() {
					stored, err := store.Save(t.Context(), unnamed("same")) // one row, one id for all
					mutex.Lock()
					defer mutex.Unlock()
					if err != nil {
						failures = append(failures, err)
						return
					}
					deduplicated[stored.ID]++
				})
			}
			saves.Wait()
			require.Empty(t, failures, "concurrent Saves of one document all answer")
			require.Len(t, deduplicated, 1, "and answer the same filter")
			for index := range 8 {
				saves.Go(func() {
					_, _ = store.Save(t.Context(), unnamed("concurrent-"+strconv.Itoa(index))) // each replaces the one before
					named := unnamed("named-" + strconv.Itoa(index))
					named.Name = "Mine " + strconv.Itoa(index)
					_, _ = store.Save(t.Context(), named) // all but one are refused
				})
			}
			saves.Wait()
			var unnamedRows, namedRows int
			require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT count(*) FILTER (WHERE name IS NULL), count(*) FILTER (WHERE name IS NOT NULL) FROM livewire_filters").
				Scan(&unnamedRows, &namedRows))
			require.Equal(t, []int{1, 1}, []int{unnamedRows, namedRows})
		})
	}
}

func TestFilterIDsAreReadInEverySpellingOfTheUUID(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	stored, err := store.Save(t.Context(), unnamed("a"))
	require.NoError(t, err)
	for _, spelling := range []string{"{" + stored.ID + "}", strings.ToUpper(stored.ID), "urn:uuid:" + stored.ID} {
		got, err := store.Get(t.Context(), alice, spelling)
		require.NoError(t, err, spelling)
		require.Equal(t, stored.ID, got.ID)
	}
	require.NoError(t, store.Delete(t.Context(), alice, "urn:uuid:"+stored.ID))
	_, err = store.Get(t.Context(), alice, stored.ID)
	requireCode(t, err, livewire.CodeNotFound)
}

func TestFilterNamesAreUniquePerOwnerAndResource(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	named := unnamed("a")
	named.Name = "Open parts"
	first, err := store.Save(t.Context(), named)
	require.NoError(t, err)
	_, err = store.Save(t.Context(), named)
	requireCode(t, err, livewire.CodeConflict)
	named.Resource = "gadgets"
	second, err := store.Save(t.Context(), named)
	require.NoError(t, err)
	_, err = store.Save(t.Context(), unnamed("a"))
	require.NoError(t, err)

	all, err := store.List(t.Context(), alice, "")
	require.NoError(t, err)
	require.Equal(t, []string{second.ID, first.ID}, []string{all[0].ID, all[1].ID}, "named only, by resource and name")
	parts, err := store.List(t.Context(), alice, "parts")
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, "Open parts", parts[0].Name)
	foreign, err := store.List(t.Context(), livewire.Owner{Key: alice.Key}, "")
	require.NoError(t, err)
	require.Empty(t, foreign, "another tenant is another owner")
}

func TestFilterGetIsOwnerOnlyAndMarksUseAtMostHourly(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	stored, err := store.Save(t.Context(), unnamed("a"))
	require.NoError(t, err)

	for name, lookup := range map[string]struct {
		owner livewire.Owner
		id    string
	}{
		"another owner":  {livewire.Owner{Key: "password:bob", Tenant: "north"}, stored.ID},
		"another tenant": {livewire.Owner{Key: alice.Key, Tenant: "south"}, stored.ID},
		"unknown id":     {alice, "0193a8f0-0000-7000-8000-000000000001"},
		"not a uuid":     {alice, "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get(t.Context(), lookup.owner, lookup.id)
			requireCode(t, err, livewire.CodeNotFound)
		})
	}

	_, err = database.pool.Exec(t.Context(), "UPDATE livewire_filters SET last_used_at = now() - interval '2 hours'")
	require.NoError(t, err)
	_, err = store.Get(t.Context(), alice, stored.ID)
	require.NoError(t, err)
	var fresh bool
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT now() - last_used_at < interval '1 minute' FROM livewire_filters").Scan(&fresh))
	require.True(t, fresh, "a stale filter is marked used")

	_, err = database.pool.Exec(t.Context(), "UPDATE livewire_filters SET last_used_at = now() - interval '10 minutes'")
	require.NoError(t, err)
	_, err = store.Get(t.Context(), alice, stored.ID)
	require.NoError(t, err)
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT now() - last_used_at < interval '1 minute' FROM livewire_filters").Scan(&fresh))
	require.False(t, fresh, "a filter used within the hour is not written again")
}

func TestFilterDeleteAndSweep(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	kept, err := store.Save(t.Context(), unnamed("a"))
	require.NoError(t, err)
	require.NoError(t, store.Delete(t.Context(), livewire.Owner{Key: "password:bob", Tenant: "north"}, kept.ID))
	require.NoError(t, store.Delete(t.Context(), alice, "nope"))
	_, err = store.Get(t.Context(), alice, kept.ID)
	require.NoError(t, err, "another owner's delete removed nothing")
	require.NoError(t, store.Delete(t.Context(), alice, kept.ID))
	_, err = store.Get(t.Context(), alice, kept.ID)
	requireCode(t, err, livewire.CodeNotFound)

	stale, err := store.Save(t.Context(), unnamed("b"))
	require.NoError(t, err)
	named := unnamed("c")
	named.Name = "Old but named"
	_, err = store.Save(t.Context(), named)
	require.NoError(t, err)
	_, err = store.Save(t.Context(), unnamed("d"))
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "UPDATE livewire_filters SET last_used_at = now() - interval '31 days' WHERE id = $1 OR name IS NOT NULL", stale.ID)
	require.NoError(t, err)
	swept, err := store.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, swept)
}

func TestFilterSaveRefusesIncompleteOrOversizedFilters(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	_, err := store.Save(t.Context(), nil)
	require.Error(t, err)
	_, err = store.Save(t.Context(), &livewire.StoredFilter{Owner: alice, Resource: "parts", Hash: "a"})
	require.Error(t, err, "no document")
	long := unnamed("a")
	long.Name = strings.Repeat("ä", 201)
	_, err = store.Save(t.Context(), long)
	requireCode(t, err, livewire.CodeInvalidRequest)
	long = unnamed("a")
	long.Context = strings.Repeat("x", 201)
	_, err = store.Save(t.Context(), long)
	requireCode(t, err, livewire.CodeInvalidRequest)
	broken := unnamed("a")
	broken.Document = []byte("{")
	_, err = store.Save(t.Context(), broken)
	require.Error(t, err, "the database refuses a document that is no JSON")

	_, err = postgresadapter.NewFilterStore(nil)
	require.Error(t, err)
	_, err = postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: database.pool, MaximumNamedPerOwner: -1})
	require.Error(t, err)
}

func TestFilterNameTakenIsAConflictAtTheMaximumToo(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	store := newFilterStore(t, database, postgresadapter.FilterStoreOptions{MaximumNamedPerOwner: 1})
	named := unnamed("a")
	named.Name = "Mine"
	_, err := store.Save(t.Context(), named)
	require.NoError(t, err)
	_, err = store.Save(t.Context(), named)
	requireCode(t, err, livewire.CodeConflict)
	named.Name = "Another"
	_, err = store.Save(t.Context(), named)
	requireCode(t, err, livewire.CodeInvalidRequest)
}
