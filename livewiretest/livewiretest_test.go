package livewiretest_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/livewiretest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// newApplication starts the notes application, broken as told, for one test.
func newApplication(t *testing.T, broken brokenBy) *notesApplication {
	t.Helper()
	application, err := startNotes(broken)
	require.NoError(t, err)
	t.Cleanup(application.close)
	return application
}

// requireFailure requires the gate to fail with a message that contains every part.
func requireFailure(t *testing.T, failures []string, parts ...string) {
	t.Helper()
	require.NotEmpty(t, failures, "the gate passed a broken application")
	joined := strings.Join(failures, "\n")
	for _, part := range parts {
		require.Contains(t, joined, part)
	}
}

func TestGeneratedIsCurrentFailsOnAStaleOrMissingFile(t *testing.T) {
	application := newApplication(t, brokenBy{})
	directory := t.TempDir()
	options := &generate.Options{TypeScriptDirectory: filepath.Join(directory, "api"), OpenAPIFile: filepath.Join(directory, "openapi.yaml"), OpenAPI: generate.OpenAPIOptions{Title: "Notes", Version: "1"}}
	require.NoError(t, generate.Write(application.Engine.Registry(), options))
	gate := func(t testing.TB) { livewiretest.GeneratedIsCurrent(t, application.Engine.Registry(), options) }
	require.Empty(t, gateFailures(gate))

	types := filepath.Join(directory, "api", "types.ts")
	current, err := os.ReadFile(types) //nolint:gosec // G304: a file below t.TempDir
	require.NoError(t, err)
	stale := strings.Replace(string(current), "title: string", "name: string", 1)
	require.NoError(t, os.WriteFile(types, []byte(stale), 0o600)) //nolint:gosec // G703: a file below t.TempDir
	require.NoError(t, os.Remove(filepath.Join(directory, "openapi.yaml")))
	failures := gateFailures(gate)
	require.Len(t, failures, 2)
	requireFailure(t, failures, types+" is not what the registry generates now", "- ", "name: string", "+ ", "title: string", "openapi.yaml is missing")

	refused := gateFailures(func(t testing.TB) {
		livewiretest.GeneratedIsCurrent(t, application.Engine.Registry(), &generate.Options{})
	})
	requireFailure(t, refused, "GeneratedIsCurrent")
}

func TestParityFailsWhenRESTAndSocketAnswerDifferently(t *testing.T) {
	application := newApplication(t, brokenBy{unstableNotes: true})
	failures := gateFailures(func(t testing.TB) {
		livewiretest.Parity(t, &livewiretest.ParityOptions{
			Application:  application.Application,
			Authenticate: as("alice"),
			Reads: []livewiretest.Read{
				{Target: "notes.item", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.history", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.distinct", Parameters: map[string]string{"field": "title"}},
			},
		})
	})
	requireFailure(t, failures, "Parity: notes map[]", "the REST body and the socket query's payload differ", "REST:", "socket:")
}

func TestParityNamesTheReadsItCannotRun(t *testing.T) {
	application := newApplication(t, brokenBy{})
	failures := gateFailures(func(t testing.TB) {
		livewiretest.Parity(t, &livewiretest.ParityOptions{
			Application:  application.Application,
			Authenticate: as("alice"),
			Reads: []livewiretest.Read{
				{Target: "notes.item", Parameters: map[string]string{"id": "00000000-0000-4000-8000-000000000000"}},
				{Target: "nothing"},
			},
		})
	})
	requireFailure(t, failures, "notes.distinct needs parameters", `the read "nothing" names no read target`, "notes.item", "REST answered 404")

	unauthenticated := gateFailures(func(t testing.TB) {
		livewiretest.Parity(t, &livewiretest.ParityOptions{Application: application.Application, Authenticate: as("nobody")})
	})
	requireFailure(t, unauthenticated, "the socket handshake answered 401")
	requireFailure(t, gateFailures(func(t testing.TB) { livewiretest.Parity(t, nil) }), "Parity needs options")
	requireFailure(t, gateFailures(func(t testing.TB) {
		livewiretest.Parity(t, &livewiretest.ParityOptions{Application: livewiretest.Application{Engine: application.Engine, Handler: application.Handler, SocketPath: "socket"}, Authenticate: as("alice")})
	}), "must start with a slash")
}

func TestStaleFailsWhenADeclarationMissesATable(t *testing.T) {
	application := newApplication(t, brokenBy{summaryTables: []string{"notes"}})
	failures := gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{
			Application:  application.Application,
			Authenticate: as("alice"),
			Reads: []livewiretest.Read{
				{Target: "notes.item", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.history", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.distinct", Parameters: map[string]string{"field": "title"}},
			},
			Writes:  []livewiretest.Write{application.addNote("north", "second"), application.addTag("north", "later")},
			Timeout: 500 * time.Millisecond,
		})
	})
	require.Len(t, failures, 1, "only the summary misses the tag")
	requireFailure(t, failures, "Stale: summary map[] did not re-run after the write to tags", `"tags":2`, `"tags":1`)
}

func TestStaleFailsOnWritesThatProveNothing(t *testing.T) {
	application := newApplication(t, brokenBy{})
	reads := []livewiretest.Read{
		{Target: "notes.item", Parameters: map[string]string{"id": application.firstNote("north")}},
		{Target: "notes.history", Parameters: map[string]string{"id": application.firstNote("north")}},
		{Target: "notes.distinct", Parameters: map[string]string{"field": "title"}},
	}
	failures := gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{
			Application: application.Application, Authenticate: as("alice"), Reads: reads,
			Writes: []livewiretest.Write{application.addNote("south", "elsewhere")},
		})
	})
	requireFailure(t, failures, "no Write for the table tags, which tags lists", "the write to notes changed no live read")

	failed := gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{
			Application: application.Application, Authenticate: as("alice"), Reads: reads,
			Writes: []livewiretest.Write{
				{Table: "notes", Commit: func(context.Context) error { return errors.New("the database is gone") }},
				application.addTag("north", "later"),
			},
		})
	})
	requireFailure(t, failed, "the write to notes: the database is gone")
	requireFailure(t, gateFailures(func(t testing.TB) { livewiretest.Stale(t, nil) }), "Stale needs options")
	requireFailure(t, gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{Application: application.Application, Authenticate: as("alice"), Reads: reads, Writes: []livewiretest.Write{{Table: "notes"}}})
	}), "the Write of notes has no Commit")
}

func TestTenantIsolationFailsWhenRowsCrossTenants(t *testing.T) {
	application := newApplication(t, brokenBy{leakingTenants: true})
	failures := gateFailures(func(t testing.TB) {
		livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
			Application: application.Application,
			Tenants:     [2]livewiretest.Tenant{{Name: "north", Authenticate: as("alice")}, {Name: "south", Authenticate: as("bob")}},
		})
	})
	southNote := application.firstNote("south")
	requireFailure(t, failures, "TenantIsolation: notes: north lists the row "+southNote+" of south", "north reads /notes/{id} of south's row "+southNote, "north's export writes the row")
}

func TestTenantIsolationNeedsRowsOfBothTenants(t *testing.T) {
	application := newApplication(t, brokenBy{})
	failures := gateFailures(func(t testing.TB) {
		livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
			Application: application.Application,
			Tenants:     [2]livewiretest.Tenant{{Name: "north", Authenticate: as("alice")}, {Name: "west", Authenticate: as("dave")}},
		})
	})
	requireFailure(t, failures, "notes: west reads no row")
	unauthenticated := gateFailures(func(t testing.TB) {
		livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
			Application: application.Application,
			Tenants:     [2]livewiretest.Tenant{{Name: "north", Authenticate: as("alice")}, {Name: "nobody", Authenticate: as("nobody")}},
		})
	})
	requireFailure(t, unauthenticated, "notes: as nobody: REST answered 401")
	requireFailure(t, gateFailures(func(t testing.TB) { livewiretest.TenantIsolation(t, nil) }), "TenantIsolation needs options")
	requireFailure(t, gateFailures(func(t testing.TB) {
		livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{Application: application.Application})
	}), "needs two tenants")
}

// isolationFailures runs TenantIsolation as north and south on an application broken as told.
func isolationFailures(t *testing.T, broken brokenBy) []string {
	t.Helper()
	application := newApplication(t, broken)
	return gateFailures(func(t testing.TB) {
		livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
			Application: application.Application,
			Tenants:     [2]livewiretest.Tenant{{Name: "north", Authenticate: as("alice")}, {Name: "south", Authenticate: as("bob")}},
		})
	})
}

// TestTenantIsolationFindsEachSelectiveLeak pins every way a loader leaks without its list
// leaking: each passed the first version of the gate.
func TestTenantIsolationFindsEachSelectiveLeak(t *testing.T) {
	t.Parallel()
	require.Empty(t, isolationFailures(t, brokenBy{notesPerTenant: 12, pageSize: 2, countCap: 3}), "a sound application passes")
	for name, testCase := range map[string]struct {
		broken brokenBy
		parts  []string
	}{
		"a distinct read":                   {brokenBy{leakingDistinct: true}, []string{"north's distinct title counts", "its own rows 0 times"}},
		"a tenant's own history":            {brokenBy{leakingHistory: true}, []string{"/notes/{id}/history of north's row", "answers the row", "of south"}},
		"rows past the capped total":        {brokenBy{notesPerTenant: 3, pageSize: 2, countCap: 1, leakingPastCap: true}, []string{"north lists the row", "of south"}},
		"the eleventh foreign item":         {brokenBy{notesPerTenant: 12, leakingItemsFrom: 10}, []string{"north reads /notes/{id} of south's row"}},
		"an export without its key":         {brokenBy{exportWithoutKey: true}, []string{`north's export writes no "id" column`}},
		"a resource whose rows have no key": {brokenBy{keylessMemos: true}, []string{"memos: as north: a listed row cannot be checked", `no usable "id"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requireFailure(t, isolationFailures(t, testCase.broken), testCase.parts...)
		})
	}
}

func TestStaleComparesAnObjectWhole(t *testing.T) {
	t.Parallel()
	application := newApplication(t, brokenBy{boardObject: true})
	failures := gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{
			Application: application.Application, Authenticate: as("alice"), Reads: notesReads(application),
			Writes:  []livewiretest.Write{application.addNote("north", "second"), application.addTag("north", "later")},
			Timeout: 500 * time.Millisecond,
		})
	})
	require.Len(t, failures, 1, "an object with items and a limit is no list: its limit counts")
	requireFailure(t, failures, "Stale: board map[] did not re-run after the write to tags", `"limit":2`)
}

func TestStaleAcceptsAWriteToAnUndeclaredTable(t *testing.T) {
	t.Parallel()
	application := newApplication(t, brokenBy{})
	require.Empty(t, gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{
			Application: application.Application, Authenticate: as("alice"), Reads: notesReads(application),
			Writes: []livewiretest.Write{
				application.addNote("north", "second"), application.addTag("north", "later"),
				{Table: "comments", Commit: func(context.Context) error { return nil }}, // no declaration reads it
			},
		})
	}))
}

func TestGeneratedIsCurrentStrictFailsOnAFileTheRegistryNoLongerGenerates(t *testing.T) {
	t.Parallel()
	application := newApplication(t, brokenBy{})
	options := &generate.Options{TypeScriptDirectory: filepath.Join(t.TempDir(), "api")}
	require.NoError(t, generate.Write(application.Engine.Registry(), options))
	strict := func(t testing.TB) { livewiretest.GeneratedIsCurrentStrict(t, application.Engine.Registry(), options) }
	require.Empty(t, gateFailures(strict))
	leftover := filepath.Join(options.TypeScriptDirectory, "removed.ts")
	require.NoError(t, os.WriteFile(leftover, []byte("// a removed declaration's client\n"), 0o600))
	requireFailure(t, gateFailures(strict), leftover+" is not generated from the registry")
	require.Empty(t, gateFailures(func(t testing.TB) {
		livewiretest.GeneratedIsCurrent(t, application.Engine.Registry(), options)
	}), "the default mode leaves other files alone")
}

// notesReads are the reads of the notes that need parameters.
func notesReads(application *notesApplication) []livewiretest.Read {
	id := map[string]string{"id": application.firstNote("north")}
	return []livewiretest.Read{
		{Target: "notes.item", Parameters: id},
		{Target: "notes.history", Parameters: id},
		{Target: "notes.distinct", Parameters: map[string]string{"field": "title"}},
	}
}
