package fixturecommand

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file is no generated output: it tests finishOutput, the part of the generated CLI that
// no real file can drive, since a regular file's Close does not fail on demand.

type closer struct{ err error }

func (c closer) Close() error { return c.err }

func TestAnOutputThatDoesNotCloseFailsTheExport(t *testing.T) {
	for name, testCase := range map[string]struct {
		closeErr, exportErr error
		failed              bool
	}{
		"written":            {},
		"a failing close":    {closeErr: errors.New("disk full"), failed: true},
		"a failed export":    {exportErr: errors.New("cut off"), failed: true},
		"both, export first": {closeErr: errors.New("disk full"), exportErr: errors.New("cut off"), failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "parts.csv")
			require.NoError(t, os.WriteFile(path, []byte("id\n"), 0o600))
			err := finishOutput(closer{err: testCase.closeErr}, path, testCase.exportErr)
			if !testCase.failed {
				require.NoError(t, err)
				require.FileExists(t, path)
				return
			}
			require.Equal(t, ExitFailure, exitCode(err), "%v", err)
			require.NoFileExists(t, path, "a file that did not finish is removed")
			if testCase.exportErr != nil {
				require.ErrorIs(t, err, testCase.exportErr, "the export's own failure is the one reported")
			}
		})
	}
}
