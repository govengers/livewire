// Package assistant is the example's in-process agent (D85, D86): one govengers/aikido run per
// question, whose tools are the API's own, generated from the livewire registry and run under
// the identity of the request that asked, so the assistant can do exactly what its user can
// and the audit names it as acting for the user.
package assistant

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/govengers/aikido/agent"
	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/tools"

	"github.com/govengers/livewire/aikidoadapter"
	"github.com/govengers/livewire/toolset"
)

// maximumTurns bounds the model calls of one question.
const maximumTurns = 8

const systemPrompt = "You manage the todos of the user's workspace through the tools. " +
	"Read before you change, change only what the user asked for, and answer in one or two sentences."

// Options configures the assistant; every field is required.
type Options struct {
	Toolset *toolset.Toolset
	Client  llm.Client
	Model   string
}

// Assistant answers questions about the workspace's todos with the API's tools.
type Assistant struct {
	options Options
	tools   *tools.Registry
}

// New registers the toolset's tools for the assistant's runs.
func New(options *Options) (*Assistant, error) {
	if options == nil || options.Toolset == nil || options.Client == nil || options.Model == "" {
		return nil, errors.New("assistant: Options needs a Toolset, a Client and a Model")
	}
	registry := tools.NewRegistry()
	if err := aikidoadapter.Register(registry, &aikidoadapter.Options{Toolset: options.Toolset}); err != nil {
		return nil, err
	}
	return &Assistant{options: *options, tools: registry}, nil
}

// Ask runs one conversation of the question as the identity in ctx and answers the reply.
func (a *Assistant) Ask(ctx context.Context, question string) (string, error) {
	session, err := agent.NewLocalSession(&agent.SessionOptions{
		ID: uuid.NewString(), Client: a.options.Client, Model: a.options.Model, Tools: a.tools,
		SystemPrompt: systemPrompt, MaximumTurns: maximumTurns,
	})
	if err != nil {
		return "", err
	}
	events, err := session.Run(ctx, question)
	if err != nil {
		return "", err
	}
	var reply strings.Builder
	var failure error
	for event := range events {
		switch event.Kind {
		case agent.EventText:
			reply.WriteString(event.Text)
		case agent.EventError:
			failure = event.Err
		default:
		}
	}
	return strings.TrimSpace(reply.String()), failure
}
