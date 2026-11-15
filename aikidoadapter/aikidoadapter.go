// Package aikidoadapter registers a livewire toolset (D85, D86) as github.com/govengers/aikido
// tools: every tool the toolset selects becomes one aikido tool whose handler runs
// Toolset.Call in process under the identity in the run's context, so an aikido agent reads
// and writes through the API's own authorization, scope, column checks, audit and
// idempotency. It is a module of its own, so livewire does not require aikido.
//
// The identity travels in the context the application gives the agent run
// (auth.WithIdentity, or the request's context behind its authentication):
//
//	registry := tools.NewRegistry()
//	err := aikidoadapter.Register(registry, &aikidoadapter.Options{Toolset: toolset})
//	session, err := agent.NewSession(&agent.SessionOptions{Tools: registry, ...})
//	events, err := session.Run(request.Context(), prompt) // the caller's identity is in it
package aikidoadapter

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/tools"

	"github.com/govengers/livewire/toolset"
)

// Options configures Register; Toolset is required: the tools, their selection, the
// confirmation hook, the answer cap and the agent's name are its options.
type Options struct {
	Toolset *toolset.Toolset
}

// Register adds every tool of the toolset to registry, in the toolset's order, with the
// tool's name, description and argument schema. A handler answers the tool's JSON; a refusal
// reaches the model as the toolset's *toolset.Error, whose text is a closed code, a message
// safe to show and the offending fields. Every call gets an Idempotency-Key of its own. It
// fails on a nil registry or toolset and on a name the registry holds already (matching
// tools.ErrDuplicateTool); tools registered before that failure stay registered.
func Register(registry *tools.Registry, options *Options) error {
	if registry == nil || options == nil || options.Toolset == nil {
		return errors.New("aikidoadapter: Register needs a registry and Options with a Toolset")
	}
	toolset := options.Toolset
	for _, tool := range toolset.Tools() {
		name := tool.Name
		handler := func(ctx context.Context, arguments json.RawMessage, _ tools.Environment) (tools.Result, error) {
			answer, err := toolset.Call(ctx, name, arguments, "")
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Content: answer}, nil
		}
		if err := registry.Register(llm.ToolDefinition{Name: name, Description: tool.Description, Parameters: tool.Parameters}, handler); err != nil {
			return err
		}
	}
	return nil
}
