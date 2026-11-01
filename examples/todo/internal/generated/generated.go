// Package generated names the artifacts the generator writes from the registry (D19): the
// TypeScript client below web/src/api, the OpenAPI document, the Go client and the CLI. The
// generator command writes them and the tests check that the committed ones are current.
package generated

import (
	"path/filepath"

	"github.com/govengers/livewire/generate"

	"github.com/govengers/livewire/examples/todo/internal/api"
)

// modulePath is the import path of the example's module.
const modulePath = "github.com/govengers/livewire/examples/todo"

// Options are the generator's options with every path below root, the module's directory.
func Options(root string) *generate.Options {
	return &generate.Options{
		TypeScriptDirectory: filepath.Join(root, "web", "src", "api"),
		OpenAPIFile:         filepath.Join(root, "openapi.yaml"),
		OpenAPI: generate.OpenAPIOptions{
			Title:       "Todo",
			Version:     "1.0.0",
			Description: "The livewire example: todos of a workspace, live over a WebSocket.",
			SecuritySchemes: []generate.SecurityScheme{
				{Name: "session", Type: "apiKey", In: "cookie", ParameterName: "__Host-auth_session", Description: "The session cookie of a password login (auth_session over plain http in development)."},
				{Name: "apiKey", Type: "http", Scheme: "bearer", Description: "An API key from POST /api/v1/keys (D20)."},
			},
		},
		GoClient:    generate.GoClientOptions{Directory: filepath.Join(root, "client", "todoclient"), ImportPath: modulePath + "/client/todoclient"},
		CommandLine: generate.CommandLineOptions{Directory: filepath.Join(root, "client", "todocommand"), Name: "todo"},
		Enums:       []generate.Enum{generate.EnumOf(api.Priorities()...)},
		Presence:    true,
	}
}
