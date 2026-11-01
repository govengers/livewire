package generate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/generate/internal/fixture/fixturecommand"
)

// commandRun is one run of the generated CLI.
type commandRun struct {
	code        int
	output      string
	diagnostics string
}

// runCommand runs the fixture CLI against the server with the user's key from the
// environment's place, the flags.
func runCommand(t *testing.T, served *fixtureServer, input string, arguments ...string) commandRun {
	t.Helper()
	var output, diagnostics bytes.Buffer
	all := append([]string{"fixture", "--base-url", served.baseURL, "--api-key", userKey}, arguments...)
	code := fixturecommand.Run(t.Context(), all, strings.NewReader(input), &output, &diagnostics)
	return commandRun{code: code, output: output.String(), diagnostics: diagnostics.String()}
}

func decodeOutput[Value any](t *testing.T, run commandRun) Value {
	t.Helper()
	require.Equal(t, fixturecommand.ExitSuccess, run.code, run.diagnostics)
	var decoded Value
	require.NoError(t, json.Unmarshal([]byte(run.output), &decoded), run.output)
	return decoded
}

func TestCommandLineServesEveryRouteKind(t *testing.T) {
	served := startFixtureServer(t)
	created := decodeOutput[fixture.Part](t, runCommand(t, served, "", "create-part", "--body", `{"name":"bolt","kind":"bolt"}`))
	require.Equal(t, "bolt", created.Name)
	fromInput := decodeOutput[fixture.Part](t, runCommand(t, served, `{"name":"nut","kind":"nut"}`, "create-part"))
	require.Equal(t, "nut", fromInput.Name, "the body comes from stdin without --body")
	id := created.ID.String()

	list := decodeOutput[livewire.ListResponse[fixture.Part]](t, runCommand(t, served, "", "parts", "list", "--limit", "1", "--sort=-createdAt", "--filter", `{"where":{"field":"name","op":"startsWith","value":""}}`))
	require.Len(t, list.Items, 1)
	require.Equal(t, 2, list.Total)
	require.Equal(t, "bolt", decodeOutput[fixture.Part](t, runCommand(t, served, "", "parts", "item", "--id", id)).Name)
	require.Len(t, decodeOutput[[]livewire.DistinctValue](t, runCommand(t, served, "", "parts", "distinct", "--field", "kind")), 2)
	require.Equal(t, 1, decodeOutput[livewire.ListResponse[livewire.Version[fixture.Part]]](t, runCommand(t, served, "", "parts", "history", "--id", id)).Total)
	require.Equal(t, 0, decodeOutput[livewire.ListResponse[livewire.PresenceItem]](t, runCommand(t, served, "", "parts", "presence", "--key", id)).Total)
	require.Equal(t, 1, decodeOutput[livewire.ListResponse[fixture.Note]](t, runCommand(t, served, "", "notes", "list")).Total)
	require.Equal(t, []string{"red", "green"}, decodeOutput[livewire.ListResponse[string]](t, runCommand(t, served, "", "labels", "list")).Items)
	stats := decodeOutput[livewire.ListResponse[fixture.Stat]](t, runCommand(t, served, "", "stats", "list", "--board", "b", "--state", "open", "--tag", "x,y",
		"--limit", "2", "--hidden", "--since", "2026-11-01T12:00:00Z", "--owner", id, "--kind", "bolt"))
	require.Equal(t, "b:open:x+y", stats.Items[0].Name)
	require.Equal(t, "limit 2 hidden true owner "+id+" kind bolt", stats.Items[1].Name, "every typed flag reaches the server")
	require.Equal(t, "d", decodeOutput[fixture.Dashboard](t, runCommand(t, served, "", "dashboard", "get", "--board", "d")).Title)
	require.Equal(t, 0, decodeOutput[livewire.ListResponse[livewire.PresenceItem]](t, runCommand(t, served, "", "dashboard", "presence", "--board", "d", "--key", "k")).Total)

	require.Equal(t, "screw", decodeOutput[fixture.Part](t, runCommand(t, served, "", "rename-part", "--id", id, "--body", `{"name":"screw"}`)).Name)
	charged := runCommand(t, served, "", "charge-part", "--invoice", "I-2", "--body", `{"body":{"reason":"x"}}`)
	require.JSONEq(t, `{"invoice":"I-2:x"}`, charged.output)

	file := filepath.Join(t.TempDir(), "picture.png")
	require.NoError(t, os.WriteFile(file, []byte("picture"), 0o600))
	uploaded := decodeOutput[fixture.Attachment](t, runCommand(t, served, "", "upload-attachment", "--id", id, "--file", file, "--content-type", "image/png"))
	require.Equal(t, int64(7), uploaded.Size)

	exported := runCommand(t, served, "", "parts", "export")
	require.Equal(t, fixturecommand.ExitSuccess, exported.code, exported.diagnostics)
	require.True(t, strings.HasPrefix(exported.output, "id,name\n"))
	require.Equal(t, "2 rows\n", exported.diagnostics)
	target := filepath.Join(t.TempDir(), "parts.csv")
	require.Equal(t, fixturecommand.ExitSuccess, runCommand(t, served, "", "parts", "export", "--output", target).code)
	require.Equal(t, exported.output, string(readFile(t, target)))

	removed := runCommand(t, served, "", "delete-part", "--id", fromInput.ID.String())
	require.Equal(t, commandRun{code: fixturecommand.ExitSuccess}, removed)
}

func TestCommandLineExitCodes(t *testing.T) {
	served := startFixtureServer(t)
	served.backend.put(fixture.Part{ID: newPartID(), Name: "cut"})
	testCases := []struct {
		name        string
		input       string
		arguments   []string
		code        int
		diagnostics string
	}{
		{"an unknown item", "", []string{"parts", "item", "--id", newPartID().String()}, fixturecommand.ExitRefused, "404 not_found"},
		{"a forbidden mutation", "", []string{"rebuild"}, fixturecommand.ExitRefused, "403 forbidden"},
		{"a refused body", "", []string{"create-part", "--body", `{"kind":"bolt"}`}, fixturecommand.ExitRefused, "name: required"},
		{"an enum value it does not declare", "", []string{"stats", "list", "--board", "b", "--state", "later"}, fixturecommand.ExitUsage, "must be one of open, done"},
		{"no uuid", "", []string{"parts", "item", "--id", "x"}, fixturecommand.ExitUsage, "must be a uuid"},
		{"no time", "", []string{"stats", "list", "--board", "b", "--since", "yesterday"}, fixturecommand.ExitUsage, "RFC 3339"},
		{"no JSON filter", "", []string{"parts", "list", "--filter", "{"}, fixturecommand.ExitUsage, "JSON document"},
		{"a missing required flag", "", []string{"parts", "item"}, fixturecommand.ExitUsage, "id"},
		{"no body", "", []string{"create-part"}, fixturecommand.ExitUsage, "no JSON document"},
		{"an unknown command", "", []string{"parts", "frobnicate"}, fixturecommand.ExitUsage, ""},
		{"an upload without its type", "", []string{"upload-attachment", "--id", newPartID().String(), "--file", "missing.png"}, fixturecommand.ExitUsage, "--content-type is required"},
		{"an upload of a missing file", "", []string{"upload-attachment", "--id", newPartID().String(), "--file", "missing.png", "--content-type", "image/png"}, fixturecommand.ExitUsage, "--file"},
		{"no batch", "", []string{"batch", "--body", `{}`}, fixturecommand.ExitUsage, "no array of batch calls"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			run := runCommand(t, served, testCase.input, testCase.arguments...)
			require.Equal(t, testCase.code, run.code, run.diagnostics)
			require.Contains(t, run.diagnostics, testCase.diagnostics)
		})
	}

	var output, diagnostics bytes.Buffer
	require.Equal(t, fixturecommand.ExitUsage, fixturecommand.Run(t.Context(), []string{"fixture", "labels", "list"}, strings.NewReader(""), &output, &diagnostics))
	require.Contains(t, diagnostics.String(), "FIXTURE_BASE_URL")
	t.Setenv("FIXTURE_BASE_URL", "http://127.0.0.1:1/api/v1")
	diagnostics.Reset()
	require.Equal(t, fixturecommand.ExitUsage, fixturecommand.Run(t.Context(), []string{"fixture", "labels", "list"}, strings.NewReader(""), &output, &diagnostics))
	require.Contains(t, diagnostics.String(), "FIXTURE_API_KEY")
	t.Setenv("FIXTURE_API_KEY", userKey)
	require.Equal(t, fixturecommand.ExitFailure, fixturecommand.Run(t.Context(), []string{"fixture", "labels", "list"}, strings.NewReader(""), &output, &diagnostics), "an unreachable server is a failure")
	require.Equal(t, fixturecommand.ExitUsage, fixturecommand.Run(t.Context(), []string{"fixture", "--base-url", "ftp://x", "labels", "list"}, strings.NewReader(""), &output, &diagnostics))
	output.Reset()
	require.Equal(t, fixturecommand.ExitSuccess, fixturecommand.Run(t.Context(), []string{"fixture", "--help"}, strings.NewReader(""), &output, &diagnostics))
	require.Contains(t, output.String(), "subscribe")

	target := filepath.Join(t.TempDir(), "parts.csv")
	served.backend.cutExport = true
	cut := runCommand(t, served, "", "parts", "export", "--output", target)
	require.Equal(t, fixturecommand.ExitFailure, cut.code, "a cut export is a failure (D60)")
	_, err := os.Stat(target)
	require.ErrorIs(t, err, os.ErrNotExist, "a cut export leaves no file")
}

func TestCommandLineRetriesUnderItsKey(t *testing.T) {
	served := startFixtureServer(t)
	arguments := []string{"create-part", "--idempotency-key", "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b49", "--body", `{"name":"once"}`}
	first := runCommand(t, served, "", arguments...)
	again := runCommand(t, served, "", arguments...)
	require.Equal(t, first, again)
	require.Len(t, served.backend.sorted(), 1)

	created := decodeOutput[fixture.Part](t, first)
	batch := `[{"target":"renamePart","params":{"id":"` + created.ID.String() + `"},"body":{"name":"batched"}},{"target":"deletePart","params":{"id":"` + newPartID().String() + `"}}]`
	results := decodeOutput[[]livewire.BatchResult](t, runCommand(t, served, "", "batch", "--idempotency-key", newPartID().String(), "--body", batch))
	require.Len(t, results, 2)
	require.Equal(t, "batched", served.backend.sorted()[0].Name)
}

func TestCommandLineStoresFilters(t *testing.T) {
	served := startFixtureServer(t)
	stored := decodeOutput[livewire.FilterItem](t, runCommand(t, served, "", "filters", "save", "--resource", "parts", "--name", "all", "--filter", `{}`))
	listed := decodeOutput[livewire.ListResponse[livewire.FilterItem]](t, runCommand(t, served, "", "filters", "list", "--resource", "parts"))
	require.Equal(t, stored.ID, listed.Items[0].ID)
	require.Equal(t, fixturecommand.ExitSuccess, runCommand(t, served, "", "parts", "list", "--filter-id", stored.ID).code)
	require.Equal(t, commandRun{code: fixturecommand.ExitSuccess}, runCommand(t, served, "", "filters", "delete", "--id", stored.ID))
	require.Equal(t, fixturecommand.ExitUsage, runCommand(t, served, "", "filters", "save", "--resource", "nowhere", "--filter", `{}`).code)
	require.Equal(t, fixturecommand.ExitUsage, runCommand(t, served, "", "filters", "save", "--resource", "parts", "--filter", `{`).code)
}

// linesUntil is an output that cancels the run after the lines it waits for.
type linesUntil struct {
	mutex  sync.Mutex
	lines  []string
	first  chan struct{}
	wanted int
	cancel context.CancelFunc
}

func (l *linesUntil) Write(data []byte) (int, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.lines = append(l.lines, strings.TrimSuffix(string(data), "\n"))
	if len(l.lines) == 1 {
		close(l.first)
	}
	if len(l.lines) == l.wanted {
		l.cancel()
	}
	return len(data), nil
}

func TestCommandLineSubscribes(t *testing.T) {
	served := startFixtureServer(t)
	served.backend.put(fixture.Part{ID: newPartID(), Name: "first"})
	ctx, cancel := context.WithCancel(t.Context())
	output := &linesUntil{first: make(chan struct{}), wanted: 2, cancel: cancel}
	done := make(chan int, 1)
	var diagnostics bytes.Buffer
	go func() {
		done <- fixturecommand.Run(ctx, []string{"fixture", "--base-url", served.baseURL, "--api-key", userKey, "subscribe", "parts", "list", "--limit", "10"}, strings.NewReader(""), output, &diagnostics)
	}()
	select {
	case <-output.first:
	case code := <-done:
		t.Fatalf("the subscription ended with %d: %s", code, diagnostics.String())
	}
	served.backend.put(fixture.Part{ID: newPartID(), Name: "second"})
	require.Equal(t, fixturecommand.ExitSuccess, <-done, "an interrupted subscription ends cleanly")
	var snapshot, diff struct {
		Kind livewire.PushKind                   `json:"kind"`
		Data livewire.ListResponse[fixture.Part] `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(output.lines[0]), &snapshot))
	require.NoError(t, json.Unmarshal([]byte(output.lines[1]), &diff))
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.Equal(t, livewire.PushListChange, diff.Kind)
	require.Len(t, diff.Data.Items, 2, "each line carries the list after the push")

	served.keys.revoke(userKey)
	refused := fixturecommand.Run(t.Context(), []string{"fixture", "--base-url", served.baseURL, "--api-key", userKey, "subscribe", "labels"}, strings.NewReader(""), io.Discard, &diagnostics)
	require.Equal(t, fixturecommand.ExitUsage, refused, "labels is not live")
	gone := fixturecommand.Run(t.Context(), []string{"fixture", "--base-url", served.baseURL, "--api-key", userKey, "subscribe", "dashboard", "get", "--board", "b"}, strings.NewReader(""), io.Discard, &diagnostics)
	require.Equal(t, fixturecommand.ExitRefused, gone, "a refused handshake ends the subscription")
}
