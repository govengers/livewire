package generate_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/generate/internal/fixture"
)

// goOptions writes only the Go outputs of a registry whose types live in the fixture.
func goOptions(directory string) *generate.Options {
	return &generate.Options{
		GoClient:    generate.GoClientOptions{Directory: filepath.Join(directory, "client"), ImportPath: "github.com/govengers/livewire/generate/internal/application/client"},
		CommandLine: generate.CommandLineOptions{Directory: filepath.Join(directory, "command"), Name: "app"},
	}
}

func TestGoOptionsAreValidated(t *testing.T) {
	registry := newRegistry(t)
	testCases := []struct {
		name    string
		change  func(options *generate.Options)
		message string
	}{
		{"no import path", func(options *generate.Options) { options.GoClient.ImportPath = "" }, "GoClient.ImportPath"},
		{"a malformed import path", func(options *generate.Options) { options.GoClient.ImportPath = "example.com//client" }, "no import path"},
		{"an upper-case package", func(options *generate.Options) { options.GoClient.Package = "Client" }, "GoClient.Package"},
		{"package main", func(options *generate.Options) { options.GoClient.Package = "main" }, "other than main"},
		{"no command name", func(options *generate.Options) { options.CommandLine.Name = "" }, "CommandLine.Name"},
		{"a command name with spaces", func(options *generate.Options) { options.CommandLine.Name = "my app" }, "CommandLine.Name"},
		{"a lower-case prefix", func(options *generate.Options) { options.CommandLine.EnvironmentPrefix = "app" }, "EnvironmentPrefix"},
		{"a directory that is no package name", func(options *generate.Options) {
			options.CommandLine.Directory = filepath.Join(t.TempDir(), "app-command")
		}, "CommandLine.Package"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := goOptions(t.TempDir())
			testCase.change(options)
			require.ErrorContains(t, generate.Write(registry, options), testCase.message)
		})
	}
	directory := t.TempDir()
	options := goOptions(directory)
	options.CommandLine.EnvironmentPrefix = "MY_APP"
	options.GoClient.Package = "appclient"
	require.NoError(t, generate.Write(registry, options))
	require.Contains(t, readGenerated(t, directory, "command/command.go"), `cli.EnvVars("MY_APP_API_KEY")`)
	require.Contains(t, readGenerated(t, directory, "client/client.go"), "package appclient\n")
}

// unexportedItem is a DTO the Go client cannot name.
type unexportedItem struct {
	ID string `json:"id"`
}

// TestItem is a DTO of this test package, which no other package can import.
type TestItem struct {
	ID string `json:"id"`
}

func objectRegistry[Item any](t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewObject("thing", "/thing", &livewire.ObjectOptions[Item, livewire.NoParameters]{
		Read: authorization.IsUser(), Load: func(context.Context, *livewire.NoParameters) (*Item, error) { return new(Item), nil },
	})))
	return registry
}

func createRegistry[Body any](t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewMutation("create", http.MethodPost, "/things", &livewire.MutationOptions[livewire.NoParameters, Body, livewire.NoContent]{
		Check: authorization.IsUser(), ClientID: livewire.UUIDv4,
		Handle: func(context.Context, *livewire.NoParameters, *Body) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})))
	return registry
}

type bodyParameters struct {
	Body string `query:"body"`
}

type keyParameters struct {
	APIKey string `query:"apiKey"`
}

func TestGoClientRefusesWhatItCannotImport(t *testing.T) {
	testCases := []struct {
		name       string
		registry   *livewire.Registry
		importPath string
		message    string
	}{
		{"an unexported type", objectRegistry[unexportedItem](t), "", "unexported"},
		{"a type of a test package", objectRegistry[TestItem](t), "", "a main or test package"},
		{"an unnamed struct", objectRegistry[struct {
			A int `json:"a"`
		}](t), "", "give it a name"},
		{"an internal package of another tree", objectRegistry[fixture.Note](t), "example.com/application/client", "it is internal to github.com/govengers/livewire/generate"},
		{"the generated package itself", objectRegistry[fixture.Note](t), "github.com/govengers/livewire/generate/internal/fixture", "declared in the generated package"},
		{"a generic argument of a test package", objectRegistry[fixture.Page[TestItem]](t), "", "a main or test package"},
		{"a client id it cannot fill", createRegistry[fixture.Note](t), "", "fills in a client id"},
		{"a type that marshals itself as text only", objectRegistry[fixture.TextOnly](t), "", "fixture.TextOnly: it has MarshalText but no UnmarshalText"},
		{"a field that does", objectRegistry[fixture.HoldsTextOnly](t), "", "fixture.HoldsTextOnly.Inner[]: it has MarshalText but no UnmarshalText"},
		{"a type with MarshalJSON only", objectRegistry[fixture.JSONOnly](t), "", "fixture.JSONOnly: it has MarshalJSON but neither UnmarshalJSON nor UnmarshalText"},
		{"a struct with UnmarshalText only", objectRegistry[fixture.TextReader](t), "", "fixture.TextReader: encoding/json writes it as its struct"},
		{"a text map key", objectRegistry[fixture.HoldsTextKeys](t), "", "the keys of fixture.HoldsTextKeys.Counts: fixture.TextKey has MarshalText"},
		{"an interface with methods", objectRegistry[fixture.HoldsStringer](t), "", "fixture.HoldsStringer.Hook: an interface with methods"},
		{"a mutation response", responseRegistry[fixture.TextOnly](t, false), "", "(the mutation answer)"},
		{"an upload response", responseRegistry[fixture.TextOnly](t, true), "", "(the upload answer)"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := goOptions(t.TempDir())
			if testCase.importPath != "" {
				options.GoClient.ImportPath = testCase.importPath
			}
			options.CommandLine = generate.CommandLineOptions{}
			require.ErrorContains(t, generate.Write(testCase.registry, options), testCase.message)
		})
	}
}

func TestGoClientFillsEveryClientIDForm(t *testing.T) {
	for name, testCase := range map[string]struct {
		registry *livewire.Registry
		fill     string
	}{
		"a named string": {createRegistry[fixture.CreateLabel](t), "if body.ID == \"\" {\n\t\tbody.ID = fixture.Color(client.NewUUIDv4().String())\n\t}"},
		"a pointer":      {createRegistry[fixture.CreateNote](t), "if body.ID == nil {\n\t\tid := client.NewUUIDv4()\n\t\tbody.ID = &id\n\t}"},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			require.NoError(t, generate.Write(testCase.registry, goOptions(directory)))
			require.Contains(t, readGenerated(t, directory, "client/client.go"), testCase.fill)
		})
	}
}

func TestCommandLineRefusesCollidingNames(t *testing.T) {
	handle := func(context.Context, *bodyParameters, *fixture.RenamePart) (*livewire.NoContent, error) {
		return &livewire.NoContent{}, nil
	}
	keyed := func(context.Context, *keyParameters, *livewire.NoBody) (*livewire.NoContent, error) {
		return &livewire.NoContent{}, nil
	}
	nothing := func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
		return &livewire.NoContent{}, nil
	}
	testCases := []struct {
		name        string
		declaration livewire.Declaration
		message     string
	}{
		{"a parameter named body", livewire.NewMutation("rename", http.MethodPost, "/rename", &livewire.MutationOptions[bodyParameters, fixture.RenamePart, livewire.NoContent]{
			Check: authorization.IsUser(), Handle: handle,
		}), `both "body" on the command line`},
		{"a parameter that is a root flag", livewire.NewMutation("touch", http.MethodPost, "/touch", &livewire.MutationOptions[keyParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Handle: keyed,
		}), `both "api-key" on the command line`},
		{"a declaration named subscribe", livewire.NewMutation("subscribe", http.MethodPost, "/subscribe", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Handle: nothing,
		}), `both "subscribe" on the command line`},
		{"a declaration that is the Go client's type", livewire.NewMutation("client", http.MethodPost, "/client", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Handle: nothing,
		}), "both named Client in the Go client"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registry, err := livewire.NewRegistry(nil)
			require.NoError(t, err)
			require.NoError(t, registry.Add(testCase.declaration))
			require.ErrorContains(t, generate.Write(registry, goOptions(t.TempDir())), testCase.message)
		})
	}
}
