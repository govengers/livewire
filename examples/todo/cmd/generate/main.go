// Command generate writes the clients of the registry (D19): the TypeScript client, the
// OpenAPI document, the Go client and the CLI. Run it from the module's directory after a
// change to the API, and commit what it writes; the tests fail while a committed file is
// not current (livewiretest.GeneratedIsCurrent):
//
//	go run ./cmd/generate
package main

import (
	"fmt"
	"os"

	"github.com/govengers/livewire/generate"

	"github.com/govengers/livewire/examples/todo/internal/controller"
	"github.com/govengers/livewire/examples/todo/internal/generated"
	"github.com/govengers/livewire/examples/todo/internal/service"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// The generator reads the declarations only, so the services stay unwired: no handler runs.
	registry, err := controller.NewRegistry(&controller.RegistryOptions{TodoService: &service.TodoService{}, AccountService: &service.AccountService{}})
	if err != nil {
		return err
	}
	return generate.Write(registry, generated.Options("."))
}
