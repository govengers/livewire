package generate_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/generate/internal/fixture/fixturecommand"
)

// failingOutput refuses every write, as a closed pipe does.
type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestASubscriptionEndsWithItsCommand runs subscribe on a context nobody cancels into an
// output that fails: the command returns, and its subscription, socket and delivery end with
// it.
func TestASubscriptionEndsWithItsCommand(t *testing.T) {
	served := startFixtureServer(t)
	ignore := goleak.IgnoreCurrent()
	var diagnostics bytes.Buffer
	code := fixturecommand.Run(context.Background(), []string{"fixture", "--base-url", served.baseURL, "--api-key", userKey, "subscribe", "dashboard", "get", "--board", "b"},
		strings.NewReader(""), failingOutput{}, &diagnostics)
	require.Equal(t, fixturecommand.ExitFailure, code, diagnostics.String())
	require.NoError(t, goleak.Find(ignore), "the subscription outlived its command")
}

func TestASubscriptionReportsEveryFailedConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	var output, diagnostics bytes.Buffer
	code := fixturecommand.Run(ctx, []string{"fixture", "--base-url", "http://127.0.0.1:1/api/v1", "--api-key", userKey, "--socket-url", "ws://127.0.0.1:1/socket", "subscribe", "dashboard", "get", "--board", "b"},
		strings.NewReader(""), &output, &diagnostics)
	require.Equal(t, fixturecommand.ExitSuccess, code, "an interrupted subscription ends cleanly")
	require.Empty(t, output.String())
	require.Contains(t, diagnostics.String(), "fixture: livewire: connecting to ws://127.0.0.1:1/socket: ")
	require.Contains(t, diagnostics.String(), "; reconnecting\n")
}

func TestCommandLineIdempotencyKeys(t *testing.T) {
	served := startFixtureServer(t)
	run := runCommand(t, served, "", "create-part", "--idempotency-key", "retry-1", "--body", `{"name":"x"}`)
	require.Equal(t, fixturecommand.ExitUsage, run.code, "a key that is no UUID is the command line's mistake")
	require.Contains(t, run.diagnostics, "--idempotency-key must be a UUID")

	var output, diagnostics bytes.Buffer
	code := fixturecommand.Run(t.Context(), []string{"fixture", "--base-url", "http://127.0.0.1:1/api/v1", "--api-key", userKey, "create-part", "--body", `{"name":"x"}`},
		strings.NewReader(""), &output, &diagnostics)
	require.Equal(t, fixturecommand.ExitFailure, code)
	shown := strings.SplitN(diagnostics.String(), "repeat it with --idempotency-key ", 2)
	require.Len(t, shown, 2, "a failed call shows the key it went out under: %s", diagnostics.String())
	key := strings.Fields(shown[1])[0]

	repeated := runCommand(t, served, "", "create-part", "--idempotency-key", key, "--body", `{"name":"x"}`)
	again := runCommand(t, served, "", "create-part", "--idempotency-key", key, "--body", `{"name":"x"}`)
	require.Equal(t, repeated, again, "the shown key repeats the call safely")
	require.NotContains(t, repeated.diagnostics, "repeat it with", "a key the command line named is not shown again")
	require.Len(t, served.backend.sorted(), 1)
}

// TestUsageErrorsGoToStderr keeps stdout for JSON (D68): a command-line mistake writes its
// diagnostic and a hint on stderr and nothing on stdout; explicit --help is stdout's.
func TestUsageErrorsGoToStderr(t *testing.T) {
	for _, arguments := range [][]string{
		{"stats", "list", "--board", ""},
		{"stats", "list", "--nope"},
		{"stats", "list"},
		{"filters", "delete", "--id", ""},
		{"create-part", "--idempotency-key", "x", "--body", "{}"},
	} {
		var output, diagnostics bytes.Buffer
		code := fixturecommand.Run(t.Context(), append([]string{"fixture", "--base-url", "http://127.0.0.1:1/api/v1", "--api-key", userKey}, arguments...), strings.NewReader(""), &output, &diagnostics)
		require.Equal(t, fixturecommand.ExitUsage, code, "%v: %s", arguments, diagnostics.String())
		require.Empty(t, output.String(), "%v writes no usage text on stdout", arguments)
		require.Contains(t, diagnostics.String(), "fixture: see --help\n", arguments)
	}
	var output, diagnostics bytes.Buffer
	require.Equal(t, fixturecommand.ExitSuccess, fixturecommand.Run(t.Context(), []string{"fixture", "stats", "list", "--help"}, strings.NewReader(""), &output, &diagnostics))
	require.Contains(t, output.String(), "--board")
	require.Empty(t, diagnostics.String())
}

func TestAnEmptyRequiredParameterIsAUsageError(t *testing.T) {
	served := startFixtureServer(t)
	served.backend.put(fixture.Part{ID: newPartID(), Name: "x"})
	for _, arguments := range [][]string{
		{"stats", "list", "--board", ""},
		{"dashboard", "presence", "--board", "b", "--key", ""},
	} {
		run := runCommand(t, served, "", arguments...)
		require.Equal(t, fixturecommand.ExitUsage, run.code, "%v: %s", arguments, run.diagnostics)
		require.Contains(t, run.diagnostics, "must not be empty")
	}
}
