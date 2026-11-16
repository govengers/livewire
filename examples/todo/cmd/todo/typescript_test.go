package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheFrontendTypeChecks compiles web/src (the generated client and live.ts, which uses it)
// with tsc under the generator's strict settings and runs live.test.ts under bun, on the TypeScript and Vue of the generator's
// runtime harness (generate/runtime, installed from its lockfile). It runs only with
// LIVEWIRE_REQUIRE_TYPESCRIPT=1 (just test-typescript, uncached) and fails there without bun:
// go test's cache cannot see the harness bun reads outside this module.
func TestTheFrontendTypeChecks(t *testing.T) {
	t.Parallel()
	if os.Getenv("LIVEWIRE_REQUIRE_TYPESCRIPT") != "1" {
		t.Skip("the frontend's type check runs in just test-typescript (LIVEWIRE_REQUIRE_TYPESCRIPT=1), uncached")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("LIVEWIRE_REQUIRE_TYPESCRIPT=1 but bun is not installed")
	}
	harness, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "generate", "runtime"))
	require.NoError(t, err)
	runBun(t, harness, bun, "install", "--frozen-lockfile")
	web, err := filepath.Abs(filepath.Join("..", "..", "web"))
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, os.CopyFS(directory, os.DirFS(web)))
	require.NoError(t, os.Symlink(filepath.Join(harness, "node_modules"), filepath.Join(directory, "node_modules")))
	tsc := filepath.Join(harness, "node_modules", "typescript", "bin", "tsc")
	runBun(t, directory, bun, tsc, "-p", filepath.Join(directory, "tsconfig.json"))      // the browser code, without bun's globals
	runBun(t, directory, bun, tsc, "-p", filepath.Join(directory, "tsconfig.test.json")) // and the bun test
	runBun(t, directory, bun, "test")                                                    // live.test.ts: the session transitions (D66)
}

func runBun(t *testing.T, directory, bun string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), bun, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	require.NoError(t, err, "bun %v in %s:\n%s", arguments, directory, output)
}
