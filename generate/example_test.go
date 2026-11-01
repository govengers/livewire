package generate_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
)

type exampleNote struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ExampleWrite generates the TypeScript client and the OpenAPI document of a registry, the
// way an application's `go generate` step or a test (livewiretest.GeneratedIsCurrent) does.
func ExampleWrite() {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
	if err != nil {
		panic(err)
	}
	err = registry.Add(livewire.NewList("notes", "/notes", &livewire.ListOptions[exampleNote, livewire.NoParameters]{
		Tables: []string{"notes"}, Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]exampleNote, error) { return nil, nil },
	}))
	if err != nil {
		panic(err)
	}
	directory, err := os.MkdirTemp("", "livewire-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(directory)
	err = generate.Write(registry, &generate.Options{
		TypeScriptDirectory: filepath.Join(directory, "ui", "src", "api"),
		OpenAPIFile:         filepath.Join(directory, "openapi.yaml"),
		OpenAPI: generate.OpenAPIOptions{
			Title: "Notes", Version: "1.0.0",
			SecuritySchemes: []generate.SecurityScheme{{Name: "apiKey", Type: "http", Scheme: "bearer"}},
		},
	})
	if err != nil {
		panic(err)
	}
	written, err := filepath.Glob(filepath.Join(directory, "ui", "src", "api", "*.ts"))
	if err != nil {
		panic(err)
	}
	for _, path := range written {
		fmt.Println(filepath.Base(path))
	}
	// Output:
	// api.ts
	// filters.ts
	// runtime.ts
	// types.ts
	// vue.ts
}
