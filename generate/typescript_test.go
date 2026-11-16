package generate_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/generate"
)

// requireTypeScript is the bun executable. The TypeScript gates run only with
// LIVEWIRE_REQUIRE_TYPESCRIPT=1 (just test-typescript, which CI runs on every push) and fail
// there without bun; otherwise they skip, since go test's cache cannot see the files bun
// reads (the runtime's sources and tests, its lockfile, node_modules) and would keep a
// result they changed (D89).
func requireTypeScript(t *testing.T) string {
	t.Helper()
	if os.Getenv("LIVEWIRE_REQUIRE_TYPESCRIPT") != "1" {
		t.Skip("the TypeScript gates run in just test-typescript (LIVEWIRE_REQUIRE_TYPESCRIPT=1), uncached")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("LIVEWIRE_REQUIRE_TYPESCRIPT=1 but bun is not installed")
	}
	return bun
}

func runBun(t *testing.T, directory, bun string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), bun, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	require.NoError(t, err, "bun %v in %s:\n%s", arguments, directory, output)
}

// installRuntime installs the harness of generate/runtime (TypeScript, Vue, the bun types)
// from its lockfile and returns its directory; the first run needs the network.
func installRuntime(t *testing.T, bun string) string {
	t.Helper()
	directory, err := filepath.Abs("runtime")
	require.NoError(t, err)
	runBun(t, directory, bun, "install", "--frozen-lockfile")
	return directory
}

// TestGeneratedTypeScriptTypeChecks compiles the fixture's generated client with a usage file
// under strict, exactOptionalPropertyTypes, noUncheckedIndexedAccess, noImplicitOverride and
// noPropertyAccessFromIndexSignature, with the edge registry and the wire-type parity file; the
// @ts-expect-error lines of the usage and the parity file must be errors.
func TestGeneratedTypeScriptTypeChecks(t *testing.T) {
	bun := requireTypeScript(t)
	runtime := installRuntime(t, bun)
	directory := t.TempDir()
	options := fixtureOptions(directory)
	options.TypeScriptDirectory = filepath.Join(directory, "src", "api")
	require.NoError(t, generate.Write(newRegistry(t), options))
	require.NoError(t, generate.Write(newEdgeRegistry(t), edgeOptions(filepath.Join(directory, "src", "edge"))))
	require.NoError(t, generate.Write(scalarRegistry(t), &generate.Options{TypeScriptDirectory: filepath.Join(directory, "src", "scalar")}))
	parity, err := generate.LibraryParityTypeScript("./api/runtime")
	require.NoError(t, err)
	writeFile(t, filepath.Join(directory, "src", "parity.ts"), []byte(parity))
	for name, target := range map[string]string{"usage.ts": "src/usage.ts", "tsconfig.json": "tsconfig.json"} {
		writeFile(t, filepath.Join(directory, target), readFile(t, filepath.Join("testdata", "typescript", name)))
	}
	require.NoError(t, os.Symlink(filepath.Join(runtime, "node_modules"), filepath.Join(directory, "node_modules")))
	runBun(t, directory, bun, filepath.Join(runtime, "node_modules", "typescript", "bin", "tsc"), "-p", directory)
}

// TestRuntimeUnitTests type-checks the runtime sources with their tests and runs the tests
// under bun (fake WebSocket and fetch).
func TestRuntimeUnitTests(t *testing.T) {
	bun := requireTypeScript(t)
	runtime := installRuntime(t, bun)
	runBun(t, runtime, bun, filepath.Join(runtime, "node_modules", "typescript", "bin", "tsc"), "-p", runtime)
	runBun(t, runtime, bun, "test")
}
