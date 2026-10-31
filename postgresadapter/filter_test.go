package postgresadapter_test

import (
	"strings"
	"sync"
	"testing"

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
	for _, hash := range []string{"a", "b"} {
		_, err := store.Save(t.Context(), unnamed(hash))
		require.NoError(t, err)
	}
	_, err := store.Save(t.Context(), unnamed("c"))
	requireCode(t, err, livewire.CodeInvalidRequest)
	_, err = store.Save(t.Context(), unnamed("a"))
	require.NoError(t, err, "an existing filter is still found at the maximum")
	bob := unnamed("c")
	bob.Owner = livewire.Owner{Key: "password:bob", Tenant: "north"}
	_, err = store.Save(t.Context(), bob)
	require.NoError(t, err, "the maximum is per owner")

	named := unnamed("a")
	named.Name = "Mine"
	_, err = store.Save(t.Context(), named)
	require.NoError(t, err)
	named.Name = "Another"
	_, err = store.Save(t.Context(), named)
	requireCode(t, err, livewire.CodeInvalidRequest)
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
