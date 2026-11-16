package aikidoadapter_test

import (
	"context"
	"fmt"

	"github.com/govengers/aikido/agent"
	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/llm/llmtest"
	"github.com/govengers/aikido/tools"

	"github.com/govengers/livewire/aikidoadapter"
	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/toolset"
)

// An aikido agent on the API's tools: the scripted model creates a note, which runs as the
// identity of the run's context.
func ExampleRegister() {
	fixture, err := toolfixture.New()
	if err != nil {
		fmt.Println(err)
		return
	}
	model := llmtest.NewStubClient(
		llmtest.TurnScript{Events: []llm.Event{{Kind: llm.EventToolCall, Tool: &llm.ToolCall{ID: "1", Name: "createNote", Arguments: `{"body":{"title":"buy milk"}}`}}, {Kind: llm.EventEnd}}},
		llmtest.TurnScript{Events: []llm.Event{{Kind: llm.EventTextDelta, Text: "Noted."}, {Kind: llm.EventEnd}}},
	)
	ctx := toolfixture.User(context.Background())
	// snippet:begin aikido
	selected, err := toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "assistant"})
	if err != nil {
		fmt.Println(err)
		return
	}
	registry := tools.NewRegistry()
	if err := aikidoadapter.Register(registry, &aikidoadapter.Options{Toolset: selected}); err != nil {
		fmt.Println(err)
		return
	}
	session, err := agent.NewLocalSession(&agent.SessionOptions{ID: "conversation-1", Client: model, Model: "openai/gpt-5.4", Tools: registry})
	if err != nil {
		fmt.Println(err)
		return
	}
	events, err := session.Run(ctx, "Remind me to buy milk.") // ctx carries the user's identity
	// snippet:end aikido
	if err != nil {
		fmt.Println(err)
		return
	}
	for event := range events {
		if event.Kind == agent.EventToolResult {
			fmt.Println(event.ToolResult.Name, event.ToolResult.OK)
		}
	}
	fmt.Println(fixture.Notes()[1].Title, fixture.Audit()[0].Identity.Real().Subject)
	// Output:
	// createNote true
	// buy milk assistant
}
