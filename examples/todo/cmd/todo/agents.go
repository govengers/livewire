package main

import (
	"net/http"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/mcpadapter"
	"github.com/govengers/livewire/toolset"

	"github.com/govengers/livewire/examples/todo/internal/assistant"
	"github.com/govengers/livewire/examples/todo/internal/generated"
)

// agentExcluded are the tools no agent gets: an agent that mints API keys would outlive the
// conversation that allowed it.
var agentExcluded = []string{"createKey"}

// agents are the agent entry points the options turn on, nil when off.
type agents struct {
	mcp       http.Handler
	assistant *assistant.Assistant
}

// snippet:begin agents

// newAgents builds the API's tools from the registry (D85) and, as the options ask, the MCP
// server for agents outside the process and the in-process assistant; each acts for the
// identity of its request and is audited under its own name (D86).
func newAgents(options *applicationOptions, engine *livewire.Engine) (*agents, error) {
	built := &agents{}
	if !options.MCP && options.Assistant == nil {
		return built, nil
	}
	catalogue, err := generate.NewCatalogue(engine.Registry(), generated.CatalogueOptions())
	if err != nil {
		return nil, err
	}
	if options.MCP {
		tools, err := toolset.New(&toolset.Options{Engine: engine, Catalogue: catalogue, Agent: "mcp", Exclude: agentExcluded})
		if err != nil {
			return nil, err
		}
		if built.mcp, err = mcpadapter.New(&mcpadapter.Options{Toolset: tools, Name: "todo", Version: version}); err != nil {
			return nil, err
		}
	}
	if options.Assistant != nil {
		tools, err := toolset.New(&toolset.Options{Engine: engine, Catalogue: catalogue, Agent: "assistant", Exclude: agentExcluded})
		if err != nil {
			return nil, err
		}
		if built.assistant, err = assistant.New(&assistant.Options{Toolset: tools, Client: options.Assistant, Model: options.AssistantModel}); err != nil {
			return nil, err
		}
	}
	return built, nil
}

// snippet:end agents
