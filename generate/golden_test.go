package generate_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/generate"
)

var update = flag.Bool("update", false, "rewrite the golden files of the generator")

// goldenFiles are the generated files the golden directory pins; runtime.ts and vue.ts are
// the sources under runtime/ with the header (TestRuntimeIsCopiedWithTheHeader).
var goldenFiles = []string{"typescript/types.ts", "typescript/filters.ts", "typescript/api.ts", "openapi.yaml"}

// goldenGoFiles are the generated Go files; their golden copies are the fixture's committed
// packages below internal/fixture, which the module compiles and the integration tests run.
var goldenGoFiles = []string{"fixtureclient/client.go", "fixtureclient/filters.go", "fixturecommand/command.go"}

// generateFixture writes the fixture registry's artifacts into a new directory.
func generateFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	require.NoError(t, generate.Write(newRegistry(t), fixtureOptions(directory)))
	return directory
}

// TestGoldenFiles compares the generated files with testdata/golden; go test -run
// TestGoldenFiles -update rewrites them.
func TestGoldenFiles(t *testing.T) {
	directory := generateFixture(t)
	for _, name := range goldenFiles {
		generated := readFile(t, filepath.Join(directory, name))
		golden := filepath.Join("testdata", "golden", filepath.Base(name))
		if *update {
			writeFile(t, golden, generated)
			continue
		}
		expected := readFile(t, golden)
		require.Equal(t, string(expected), string(generated), "%s differs from its golden file; run go test ./generate -run TestGoldenFiles -update and review the diff", name)
	}
	for _, name := range goldenGoFiles {
		generated := readFile(t, filepath.Join(directory, name))
		golden := filepath.Join("internal", "fixture", name)
		if *update {
			writeFile(t, golden, generated)
			continue
		}
		require.Equal(t, string(readFile(t, golden)), string(generated), "%s differs from its golden file; run go test ./generate -run TestGoldenFiles -update and review the diff", name)
	}
}

// TestRenderAnswersWhatWriteWrites renders the fixture in memory: every file Write writes, at
// its path, byte for byte, and nothing on disk.
func TestRenderAnswersWhatWriteWrites(t *testing.T) {
	directory := generateFixture(t)
	rendered := t.TempDir()
	files, err := generate.Render(newRegistry(t), fixtureOptions(rendered))
	require.NoError(t, err)
	entries, err := os.ReadDir(rendered)
	require.NoError(t, err)
	require.Empty(t, entries, "Render writes nothing")
	written := 0
	require.NoError(t, filepath.WalkDir(directory, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			written++
		}
		return err
	}))
	require.Len(t, files, written)
	for _, file := range files {
		relative, err := filepath.Rel(rendered, file.Path)
		require.NoError(t, err)
		require.Equal(t, string(readFile(t, filepath.Join(directory, relative))), string(file.Content), relative)
	}
	_, err = generate.Render(nil, fixtureOptions(rendered))
	require.Error(t, err)
	_, err = generate.Render(newRegistry(t), &generate.Options{})
	require.Error(t, err)
}
