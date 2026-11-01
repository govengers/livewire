package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTheFrontendTypeChecks compiles web/src (the generated client and live.ts, which uses it)
// with tsc under the generator's strict settings, on the TypeScript and Vue of the generator's
// runtime harness (generate/runtime, installed from its lockfile). Without bun it skips, unless
// LIVEWIRE_REQUIRE_TYPESCRIPT=1 (set in CI, which has bun) makes it fail.
func TestTheFrontendTypeChecks(t *testing.T) {
	t.Parallel()
	bun, err := exec.LookPath("bun")
	if err != nil {
		if os.Getenv("LIVEWIRE_REQUIRE_TYPESCRIPT") == "1" {
			t.Fatal("LIVEWIRE_REQUIRE_TYPESCRIPT=1 but bun is not installed")
		}
		t.Skip("bun is not installed: the frontend's type check is skipped (LIVEWIRE_REQUIRE_TYPESCRIPT=1 requires it)")
	}
	harness, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "generate", "runtime"))
	require.NoError(t, err)
	runBun(t, harness, bun, "install", "--frozen-lockfile")
	web, err := filepath.Abs(filepath.Join("..", "..", "web"))
	require.NoError(t, err)
	directory := t.TempDir()
	require.NoError(t, os.CopyFS(directory, os.DirFS(web)))
	require.NoError(t, os.Symlink(filepath.Join(harness, "node_modules"), filepath.Join(directory, "node_modules")))
	runBun(t, directory, bun, filepath.Join(harness, "node_modules", "typescript", "bin", "tsc"), "-p", directory)
}

func runBun(t *testing.T, directory, bun string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), bun, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	require.NoError(t, err, "bun %v in %s:\n%s", arguments, directory, output)
}
