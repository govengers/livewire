package generate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/generate/internal/fixture"
)

// compiledModule is the module path of the application the compile test writes: below
// generate/, so the fixture's internal DTO packages are importable from it, as an
// application's own DTO packages are from its client.
const compiledModule = "github.com/govengers/livewire/generate/compiled"

const compiledMain = `package main

import (
	"context"
	"os"
	"os/signal"

	"` + compiledModule + `/fixturecommand"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(fixturecommand.Run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr))
}
`

// TestGeneratedGoCompilesInAnApplicationModule writes the fixture's Go client and CLI into a
// module of their own that requires livewire through a replace directive, as an
// application's module would, with the main the CLI's package comment shows, and runs go
// build and go vet on it. The generated code must stand on its own outside this module.
func TestGeneratedGoCompilesInAnApplicationModule(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module of its own")
	}
	goBinary, err := exec.LookPath("go")
	require.NoError(t, err)
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, generate.Write(newRegistry(t), &generate.Options{
		GoClient:    generate.GoClientOptions{Directory: filepath.Join(directory, "fixtureclient"), ImportPath: compiledModule + "/fixtureclient"},
		CommandLine: generate.CommandLineOptions{Directory: filepath.Join(directory, "fixturecommand"), Name: "fixture"},
		Enums:       []generate.Enum{generate.EnumOf(fixture.KindBolt, fixture.KindNut)},
		Presence:    true,
	}))
	// A create whose JSON id lives in one of two embedded structs that both have a Go field ID:
	// the fill-in must name the full path, or the client does not build.
	require.NoError(t, generate.Write(createRegistry[fixture.CreateEmbedded](t), &generate.Options{
		GoClient: generate.GoClientOptions{Directory: filepath.Join(directory, "embeddedclient"), ImportPath: compiledModule + "/embeddedclient"},
	}))
	require.Contains(t, readGenerated(t, directory, "embeddedclient/client.go"), "if body.Primary.ID == uuid.Nil {")
	// An id promoted through an unexported embedded struct is written as body.ID, which
	// compiles from another package.
	require.NoError(t, generate.Write(createRegistry[fixture.CreateHidden](t), &generate.Options{
		GoClient: generate.GoClientOptions{Directory: filepath.Join(directory, "hiddenclient"), ImportPath: compiledModule + "/hiddenclient"},
	}))
	module := "module " + compiledModule + "\n\ngo 1.26.0\n\nrequire github.com/govengers/livewire v0.0.0\n\nreplace github.com/govengers/livewire => " + root + "\n"
	writeFile(t, filepath.Join(directory, "go.mod"), []byte(module))
	writeFile(t, filepath.Join(directory, "go.sum"), readFile(t, filepath.Join(root, "go.sum")))
	writeFile(t, filepath.Join(directory, "cmd", "fixture", "main.go"), []byte(compiledMain))
	run := func(arguments ...string) ([]byte, error) {
		command := exec.CommandContext(t.Context(), goBinary, arguments...)
		command.Dir = directory
		command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		return command.CombinedOutput()
	}
	for _, arguments := range [][]string{{"mod", "tidy"}, {"build", "./..."}, {"vet", "./..."}} {
		output, err := run(arguments...)
		require.NoError(t, err, "go %v:\n%s", arguments, output)
	}

	// The filter vocabulary is typed as filters.ts types it: a plain string is no enum value,
	// no sort term and no distinct field.
	writeFile(t, filepath.Join(directory, "misuse", "misuse.go"), []byte(compiledMisuse))
	output, err := run("build", "./misuse")
	require.Error(t, err, "%s", output)
	for _, typed := range []string{"PartsKind", "PartsSortTerm", "PartsDistinctField"} {
		require.Contains(t, string(output), "cannot use text (variable of type string) as fixtureclient."+typed+" value")
	}
	require.Equal(t, 3, strings.Count(string(output), "cannot use text"), "%s", output)
}

const compiledMisuse = `package misuse

import "` + compiledModule + `/fixtureclient"

func Misuse(text string) {
	_ = fixtureclient.PartsKindEqual(text)
	_ = fixtureclient.PartsListQuery{Sort: []fixtureclient.PartsSortTerm{text}}
	_ = fixtureclient.PartsDistinctQuery{Field: text}
}
`
