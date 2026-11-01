package livewiretest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/livewiretest"
)

// The examples run every gate against the notes application of fixture_test.go, which stands
// in for an application's own wiring. In an application each gate is a test of its own:
//
//	func TestParity(t *testing.T) {
//		application := newTestApplication(t) // main's wiring on a test database
//		livewiretest.Parity(t, &livewiretest.ParityOptions{...})
//	}

func ExampleGeneratedIsCurrent() {
	application, err := startNotes(brokenBy{})
	if err != nil {
		panic(err)
	}
	defer application.close()
	directory, err := os.MkdirTemp("", "livewiretest-example-")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	// The options the application's generator command passes, relative to the test's package.
	options := &generate.Options{TypeScriptDirectory: filepath.Join(directory, "web", "src", "api")}
	if err := generate.Write(application.Engine.Registry(), options); err != nil {
		panic(err)
	}

	fmt.Println(verdict(gateFailures(func(t testing.TB) {
		livewiretest.GeneratedIsCurrent(t, application.Engine.Registry(), options)
	})))
	// Output: passed
}

func ExampleParity() {
	application, err := startNotes(brokenBy{})
	if err != nil {
		panic(err)
	}
	defer application.close()

	fmt.Println(verdict(gateFailures(func(t testing.TB) {
		livewiretest.Parity(t, &livewiretest.ParityOptions{
			Application:  application.Application,
			Authenticate: as("alice"),
			Reads: []livewiretest.Read{
				{Target: "notes.item", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.history", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.distinct", Parameters: map[string]string{"field": "title"}},
			},
		})
	})))
	// Output: passed
}

func ExampleStale() {
	application, err := startNotes(brokenBy{})
	if err != nil {
		panic(err)
	}
	defer application.close()

	fmt.Println(verdict(gateFailures(func(t testing.TB) {
		livewiretest.Stale(t, &livewiretest.StaleOptions{
			Application:  application.Application,
			Authenticate: as("alice"),
			Reads: []livewiretest.Read{
				{Target: "notes.item", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.history", Parameters: map[string]string{"id": application.firstNote("north")}},
				{Target: "notes.distinct", Parameters: map[string]string{"field": "title"}},
			},
			Writes: []livewiretest.Write{application.addNote("north", "second"), application.addTag("north", "later")},
		})
	})))
	// Output: passed
}

func ExampleTenantIsolation() {
	application, err := startNotes(brokenBy{})
	if err != nil {
		panic(err)
	}
	defer application.close()

	fmt.Println(verdict(gateFailures(func(t testing.TB) {
		livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
			Application: application.Application,
			Tenants:     [2]livewiretest.Tenant{{Name: "north", Authenticate: as("alice")}, {Name: "south", Authenticate: as("bob")}},
		})
	})))
	// Output: passed
}
