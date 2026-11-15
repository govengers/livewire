# Agent tools

Every declared read, mutation and upload is a tool an AI agent can call, without a hand-written tool (D85 to D87). Three pieces:

1. The **catalogue** (`generate.NewCatalogue`) describes each operation as a tool: name, description, kind, whether it changes data, and JSON Schemas of its arguments, its body and its answer.
2. The **toolset** (`toolset.New`) runs the tools in process: a call is `Engine.Read`, `Engine.Call` or `Engine.Upload` under the identity in the context, so authorization, scope, column checks, audit and idempotency are exactly the API's.
3. The **adapters** hand a toolset to an agent: `aikidoadapter` registers it as govengers/aikido tools, `mcpadapter` serves it as a Model Context Protocol server. Each is a Go module of its own, so an application that uses neither never downloads aikido or the MCP SDK.

## The catalogue

<!-- snippet: generate/catalogue_example_test.go#catalogue -->
```go
catalogue, err := generate.NewCatalogue(registry, &generate.CatalogueOptions{Presence: false})
if err != nil {
    fmt.Println(err)
    return
}
for _, tool := range catalogue.Tools() {
    fmt.Println(tool.Name, tool.Kind, tool.Mutating, tool.Method, tool.Path)
}
```

- One tool per read kind of a declaration (a resource's list, item, distinct and history reads, a list's or an object's read, the presence reads when `Presence` is set), per mutation and per upload. Exports, batches and stored filters are no tools.
- The name is the OpenAPI operation id (`getTodos`, `getTodosItem`, `createTodo`), at most 64 bytes, which every model provider accepts.
- The description is the declaration's own `Description` (an option of every declaration kind, also the OpenAPI operation's description), followed by a sentence the kind writes ("Lists todos: one page of the items under an optional filter document, ...").
- `Parameters` is the schema of the arguments a model sends: the declaration's parameters as members, the reserved ones of a filterable read included (`filter` as a filter document, not as text, with one condition schema per field and operator), and a mutation's body under `body`. An upload takes `content` (text, or base64 with `contentEncoding: "base64"`) and `contentType`. `Body` is the body alone, `Result` the REST answer.
- The schemas are JSON Schema draft 2020-12, derived from the OpenAPI document's components: references point into `$defs`, `nullable` becomes `anyOf` with null, an object of declared members admits no other (a body's unknown members are refused, D40), the `x-` extensions are dropped.
- `generate.Options.CatalogueFile` writes the catalogue as JSON beside the clients, so `livewiretest.GeneratedIsCurrent` keeps it current and a diff shows what agents see change. The todo example commits [tools.json](../../examples/todo/tools.json).

## The toolset

<!-- snippet: toolset/example_test.go#toolset -->
```go
tools, err := toolset.New(&toolset.Options{
    Engine: fixture.Engine, Catalogue: fixture.Catalogue,
    Agent:   "assistant", // the audit's real actor: agent:assistant
    Exclude: []string{"deleteNote"},
    Confirm: func(context.Context, *toolset.Call) error { return nil }, // ask the user; an error declines
})
if err != nil {
    fmt.Println(err)
    return
}
answer, err := tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"buy milk"}}`), "")
```

- **Identity.** A call runs as the identity in `ctx`, the one the request's authentication put there. Every static check, scope, tenant and column check is that identity's; an agent can never do more than its user.
- **Who acted.** The toolset puts the agent behind the identity as its `Impersonator` (provider `agent`, subject `Options.Agent`), the existing real-actor mechanism (D86): the audit's `real_actor` and the history's `real_actor` read `agent:assistant`, the `actor` stays the user. An impersonation the user was under stays behind the agent. A check wrapped in `authorization.Real(...)` sees the agent, which has no roles, so real-identity-only actions (stopping an impersonation, say) are refused to agents.
- **Selection.** `Include` and `Exclude` name tools (a name that is no tool fails `New`), `ReadOnly` drops every tool that changes data. Exclude what an agent must never do: the todo example excludes `createKey`.
- **Confirmation.** `Confirm` runs before every call that changes data; an error declines it with the code `declined` and nothing runs. The hook's error text stays with the application.
- **Answer cap.** `MaximumResultBytes` (default 64 KiB): a read past it is refused with `too_large`, so the agent asks for less; a write past it committed, so it answers `{"committed": true, "answerBytes": N, ...}` instead of its body.
- **Refusals** are `*toolset.Error{Code, Message, Fields}`: livewire's closed codes with their fixed messages (`livewire.ResponseFor`, a 500 never carries its text), plus `declined` and `too_large`. Arguments are strict: an unknown member, a non-object or a nested list is `invalid_request` naming the member.
- **Idempotency.** Each call gets a fresh UUID v7 Idempotency-Key; a caller that retries one call passes its own (a UUID). A write that started runs on a context the caller's cancellation does not reach, as the REST adapter's writes do (D60); a call whose context is already cancelled starts nothing.

## govengers/aikido

`go get github.com/govengers/livewire/aikidoadapter`

<!-- snippet: aikidoadapter/example_test.go#aikido -->
```go
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
```

The run's context carries the identity: run the session on the request's context (behind the authentication), or on `auth.WithIdentity(ctx, identity)` in a background job that acts for a user. A refusal reaches the model as the tool's error, `code: message field: reason`.

## Model Context Protocol

`go get github.com/govengers/livewire/mcpadapter`

<!-- snippet: mcpadapter/example_test.go#mcp -->
```go
selected, err := toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "mcp", ReadOnly: true})
if err != nil {
    fmt.Println(err)
    return
}
handler, err := mcpadapter.New(&mcpadapter.Options{Toolset: selected, Name: "notes", Version: "1.0.0"})
if err != nil {
    fmt.Println(err)
    return
}
server := httptest.NewServer(authenticate(handler)) // the application mounts it behind its own authentication
```

- Mount the handler behind the application's authentication (API key or session); a request without an identity gets livewire's 401 body before the protocol sees it.
- The server is stateless on the Streamable HTTP transport: POST only, no `Mcp-Session-Id`, JSON answers. Each request is a session of its own, so the identity of one request never serves another, and any replica answers any request.
- A tool's annotations follow its kind: reads are read-only, a DELETE is destructive, PUT and DELETE are idempotent. An answer is one text content with the tool's JSON; a refusal sets `isError` and carries the `*toolset.Error` as JSON.
- A client retries a call safely with `_meta.idempotencyKey` (a UUID) on `tools/call`.

## In the todo example

<!-- snippet: examples/todo/cmd/todo/agents.go#agents -->
```go
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
```

`MCP_ENABLED=true` serves the tools at `POST /api/v1/mcp`; `ASSISTANT_MODEL` with `OPENROUTER_API_KEY` answers `POST /api/v1/assistant {"question": "..."}` with an aikido agent on the same tools. [agents_test.go](../../examples/todo/cmd/todo/agents_test.go) drives both on embedded PostgreSQL: an MCP client with an API key, a scripted model through aikido's `llmtest` stub, and the audit and history rows that name the agent.

## Testing

The toolset and the adapters are tested on an engine with in-memory stores (fast; the engine against Postgres is the postgresadapter suite's), the example on embedded PostgreSQL. In an application, the catalogue file and `GeneratedIsCurrent` catch a tool that changed; a test like the example's proves an agent's write lands in the audit under the agent's name.
