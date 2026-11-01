//go:build e2e

package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/generate"
)

// TestTheTypeScriptRuntimeDrivesBothEngines generates the TypeScript client of the suite's
// registry and runs typescript/scenario.ts under bun against the two engines: REST on engine A,
// the socket on engine B, the change feed between them, D54's recovery against the real
// FilterStore on the REST and the subscribe path, and a re-login through the pair model (D66).
// Without bun it skips, unless LIVEWIRE_REQUIRE_TYPESCRIPT=1 (set in CI) makes it fail.
func TestTheTypeScriptRuntimeDrivesBothEngines(t *testing.T) {
	t.Parallel()
	bun, err := exec.LookPath("bun")
	if err != nil {
		if os.Getenv("LIVEWIRE_REQUIRE_TYPESCRIPT") == "1" {
			t.Fatal("LIVEWIRE_REQUIRE_TYPESCRIPT=1 but bun is not installed")
		}
		t.Skip("bun is not installed: the TypeScript runtime's end-to-end run is skipped (LIVEWIRE_REQUIRE_TYPESCRIPT=1 requires it)")
	}
	cluster := newCluster(t)
	directory := t.TempDir()
	require.NoError(t, generate.Write(cluster.a.engine.Registry(), &generate.Options{TypeScriptDirectory: filepath.Join(directory, "api"), Presence: true}))
	scenario, err := os.ReadFile(filepath.Join("typescript", "scenario.ts"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "scenario.ts"), scenario, 0o600)) //nolint:gosec // G703: a file below t.TempDir

	command := exec.CommandContext(t.Context(), bun, "run", "scenario.ts")
	command.Dir = directory
	command.Env = append(os.Environ(), "LIVEWIRE_ENGINE_A="+cluster.a.server.URL, "LIVEWIRE_ENGINE_B="+cluster.b.server.URL)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "bun run scenario.ts:\n%s", output)
	require.Contains(t, string(output), "ok")
}
