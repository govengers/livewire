package livewiretest

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
)

// GeneratedIsCurrent renders every artifact of the registry in memory, as generate.Write
// would write it with options (TypeScript, OpenAPI, the Go client and the CLI, whichever the
// options name), and fails the test for every committed file that is missing or differs,
// with the lines that differ (D23). Pass the options the application's generator command
// passes, with paths relative to the test's working directory, the package directory.
func GeneratedIsCurrent(tb testing.TB, registry *livewire.Registry, options *generate.Options) {
	tb.Helper()
	files, err := generate.Render(registry, options)
	if err != nil {
		tb.Fatalf("livewiretest: GeneratedIsCurrent: %v", err)
		return
	}
	for _, file := range files {
		committed, err := os.ReadFile(file.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			tb.Errorf("livewiretest: GeneratedIsCurrent: %s is missing: run the generator and commit its output", file.Path)
		case err != nil:
			tb.Errorf("livewiretest: GeneratedIsCurrent: %v", err)
		case !bytes.Equal(committed, file.Content):
			tb.Errorf("livewiretest: GeneratedIsCurrent: %s is not what the registry generates now: run the generator and commit its output\n%s",
				file.Path, lineDifference(committed, file.Content))
		default:
		}
	}
}

// GeneratedIsCurrentStrict is GeneratedIsCurrent for an application whose output directories
// belong to the generator: it also fails on every file below the TypeScript, Go client and CLI
// directories that the registry no longer generates, such as the client of a removed
// declaration's leftover file. An application that keeps its own files there uses
// GeneratedIsCurrent.
func GeneratedIsCurrentStrict(tb testing.TB, registry *livewire.Registry, options *generate.Options) {
	tb.Helper()
	GeneratedIsCurrent(tb, registry, options)
	files, err := generate.Render(registry, options)
	if err != nil {
		return // GeneratedIsCurrent failed the test with it
	}
	rendered := map[string]bool{}
	for _, file := range files {
		rendered[filepath.Clean(file.Path)] = true
	}
	for _, directory := range []string{options.TypeScriptDirectory, options.GoClient.Directory, options.CommandLine.Directory} {
		if directory == "" {
			continue
		}
		err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case !entry.IsDir() && !rendered[filepath.Clean(path)]:
				tb.Errorf("livewiretest: GeneratedIsCurrentStrict: %s is not generated from the registry: delete it", path)
			default:
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			tb.Errorf("livewiretest: GeneratedIsCurrentStrict: %v", err)
		}
	}
}

// differenceLines bounds the lines of each side a difference shows.
const differenceLines = 12

// lineDifference shows where two texts differ: the first differing line's number, then the
// committed lines (-) and the generated ones (+) between the common start and the common end,
// at most differenceLines of each.
func lineDifference(committed, generated []byte) string {
	left := strings.Split(string(committed), "\n")
	right := strings.Split(string(generated), "\n")
	start := 0
	for start < len(left) && start < len(right) && left[start] == right[start] {
		start++
	}
	leftEnd, rightEnd := len(left), len(right)
	for leftEnd > start && rightEnd > start && left[leftEnd-1] == right[rightEnd-1] {
		leftEnd--
		rightEnd--
	}
	var difference strings.Builder
	fmt.Fprintf(&difference, "@@ line %d: %d committed lines, %d generated lines @@\n", start+1, leftEnd-start, rightEnd-start)
	writeLines(&difference, "-", left[start:leftEnd])
	writeLines(&difference, "+", right[start:rightEnd])
	return difference.String()
}

func writeLines(output *strings.Builder, prefix string, lines []string) {
	for index, line := range lines {
		if index == differenceLines {
			fmt.Fprintf(output, "%s ... %d more\n", prefix, len(lines)-differenceLines)
			return
		}
		fmt.Fprintf(output, "%s %s\n", prefix, line)
	}
}
