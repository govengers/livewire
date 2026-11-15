package aikidoadapter_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/govengers/aikido/agent"
	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/llm/llmtest"
	"github.com/govengers/aikido/tools"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/aikidoadapter"
	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/toolset"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func newRegistry(t *testing.T, readOnly bool) (*toolfixture.Fixture, *tools.Registry) {
	t.Helper()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	selected, err := toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "assistant", ReadOnly: readOnly})
	require.NoError(t, err)
	registry := tools.NewRegistry()
	require.NoError(t, aikidoadapter.Register(registry, &aikidoadapter.Options{Toolset: selected}))
	return fixture, registry
}

// run plays one agent run whose first reply calls the tools and whose second ends it, as the
// identity in ctx, and answers the tool results by call id.
func run(ctx context.Context, t *testing.T, registry *tools.Registry, calls ...llm.ToolCall) map[string]agent.ToolResult {
	t.Helper()
	first := llmtest.TurnScript{}
	for position := range calls {
		first.Events = append(first.Events, llm.Event{Kind: llm.EventToolCall, Tool: &calls[position]})
	}
	first.Events = append(first.Events, llm.Event{Kind: llm.EventEnd, FinishReason: "tool_calls"})
	client := llmtest.NewStubClient(first, llmtest.TurnScript{Events: []llm.Event{{Kind: llm.EventTextDelta, Text: "done"}, {Kind: llm.EventEnd}}})
	session, err := agent.NewLocalSession(&agent.SessionOptions{ID: "conversation", Client: client, Model: "stub", Tools: registry})
	require.NoError(t, err)
	events, err := session.Run(ctx, "work on the notes")
	require.NoError(t, err)
	results := map[string]agent.ToolResult{}
	for event := range events {
		switch event.Kind {
		case agent.EventToolResult:
			results[event.ToolResult.CallID] = *event.ToolResult
		case agent.EventError:
			require.NoError(t, event.Err)
		default:
		}
	}
	require.Len(t, client.Requests(), 2)
	return results
}

func TestAnAgentReadsAndWritesAsTheIdentityThroughTheAPI(t *testing.T) {
	t.Parallel()
	fixture, registry := newRegistry(t, false)
	note := fixture.Notes()[0].ID.String()
	results := run(toolfixture.User(t.Context()), t, registry,
		llm.ToolCall{ID: "list", Name: "getNotes", Arguments: `{"limit":10}`},
		llm.ToolCall{ID: "create", Name: "createNote", Arguments: `{"body":{"title":"from the agent"}}`},
		llm.ToolCall{ID: "secret", Name: "createNote", Arguments: `{"body":{"title":"x","secret":"mine"}}`},
		llm.ToolCall{ID: "delete", Name: "deleteNote", Arguments: `{"id":"` + note + `"}`},
		llm.ToolCall{ID: "missing", Name: "createNote", Arguments: `{}`},
	)

	require.True(t, results["list"].OK, results["list"].Error)
	var page livewire.ListResponse[toolfixture.Note]
	encoded, err := json.Marshal(results["list"].Content)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &page))
	require.Equal(t, "first", page.Items[0].Title)
	require.NotContains(t, results["list"].ModelContent, "hidden", "the guarded column is stripped for the user")

	require.True(t, results["create"].OK, results["create"].Error)
	require.Contains(t, results["create"].ModelContent, "from the agent")
	require.False(t, results["secret"].OK)
	require.Contains(t, results["secret"].Error, "forbidden_field")
	require.False(t, results["delete"].OK)
	require.Contains(t, results["delete"].Error, "forbidden: ")
	require.False(t, results["missing"].OK)
	require.Contains(t, results["missing"].Error, "required argument", "aikido checks the schema's required members first")

	entries := fixture.Audit()
	require.Len(t, entries, 1)
	require.Equal(t, "password:alice", livewire.OwnerKey(entries[0].Identity))
	require.Equal(t, "agent:assistant", livewire.OwnerKey(entries[0].Identity.Real()))
	require.Len(t, fixture.Notes(), 2)
}

func TestAReadOnlyAgentGetsNoWrite(t *testing.T) {
	t.Parallel()
	_, registry := newRegistry(t, true)
	var names []string
	for _, definition := range registry.Definitions() {
		names = append(names, definition.Name)
	}
	require.Equal(t, []string{"getNotes"}, names)
	results := run(toolfixture.Admin(t.Context()), t, registry, llm.ToolCall{ID: "create", Name: "createNote", Arguments: `{"body":{"title":"x"}}`})
	require.False(t, results["create"].OK)
}

func TestRegisterRefusesAMissingToolsetAndADuplicate(t *testing.T) {
	t.Parallel()
	require.Error(t, aikidoadapter.Register(nil, nil))
	require.Error(t, aikidoadapter.Register(tools.NewRegistry(), &aikidoadapter.Options{}))
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	selected, err := toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "assistant"})
	require.NoError(t, err)
	registry := tools.NewRegistry()
	require.NoError(t, aikidoadapter.Register(registry, &aikidoadapter.Options{Toolset: selected}))
	require.ErrorIs(t, aikidoadapter.Register(registry, &aikidoadapter.Options{Toolset: selected}), tools.ErrDuplicateTool)
}
