package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/llm/llmtest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/mcpadapter"

	"github.com/govengers/livewire/examples/todo/internal/server"
)

// The agents on the application as main wires them (D85, D86): an MCP client with a user's API
// key, and the in-process assistant on a scripted model, each acting for the user and audited
// under its own name.

// withKey sends the API key on every request.
type withKey struct {
	key  string
	next http.RoundTripper
}

func (k withKey) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+k.key)
	return k.next.RoundTrip(request)
}

func (a *testApplication) mcpSession(t *testing.T, username string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "todo-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   a.server.URL + server.MCPPath,
		HTTPClient: &http.Client{Transport: withKey{key: a.apiKey(t, username), next: a.server.Client().Transport}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callText(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) string {
	t.Helper()
	return resultText(t, session, &mcp.CallToolParams{Name: name, Arguments: arguments})
}

func resultText(t *testing.T, session *mcp.ClientSession, call *mcp.CallToolParams) string {
	t.Helper()
	result, err := session.CallTool(t.Context(), call)
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	text := result.Content[0].(*mcp.TextContent).Text
	require.False(t, result.IsError, text)
	return text
}

// actors are the actor and real actor of every audited call of the mutation, oldest first.
func (a *testApplication) actors(t *testing.T, mutation string) [][2]string {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), "SELECT actor, real_actor FROM livewire_audit WHERE mutation = $1 ORDER BY id", mutation)
	require.NoError(t, err)
	defer rows.Close()
	var actors [][2]string
	for rows.Next() {
		var actor, real string
		require.NoError(t, rows.Scan(&actor, &real))
		actors = append(actors, [2]string{actor, real})
	}
	require.NoError(t, rows.Err())
	return actors
}

func TestAnMCPClientWorksAsTheKeysUserAndIsAuditedAsTheAgent(t *testing.T) {
	t.Parallel()
	application := newTestApplicationWith(t, func(options *applicationOptions) { options.MCP = true })
	alice, bob := application.mcpSession(t, "alice"), application.mcpSession(t, "bob")

	listed, err := alice.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, "getTodos")
	require.Contains(t, names, "createTodo")
	require.NotContains(t, names, "createKey", "no agent mints keys")
	require.NotContains(t, names, "getTodosPresence", "no agent reads presence")

	create := &mcp.CallToolParams{
		Name: "createTodo", Arguments: map[string]any{"body": map[string]any{"title": "over mcp", "priority": "high"}},
		Meta: mcp.Meta{mcpadapter.MetaIdempotencyKey: "019a0000-0000-7000-8000-0000000000aa"},
	}
	created := resultText(t, alice, create)
	require.Equal(t, created, resultText(t, alice, create), "a retry with the same key answers the recorded response")
	var todo struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(created), &todo))
	require.Contains(t, callText(t, alice, "getTodos", map[string]any{"filter": map[string]any{"where": map[string]any{"field": "priority", "op": "eq", "value": "high"}}}), "over mcp")
	require.NotContains(t, callText(t, bob, "getTodos", nil), "over mcp", "another workspace never sees it")

	actors := application.actors(t, "createTodo")
	require.Len(t, actors, 1, "the retry wrote nothing")
	require.True(t, strings.HasPrefix(actors[0][0], "apikey:"), actors[0][0])
	require.Equal(t, "agent:mcp", actors[0][1])
	var historyActor string
	require.NoError(t, application.pool.QueryRow(t.Context(), "SELECT real_actor FROM todos_history WHERE id = $1", todo.ID).Scan(&historyActor))
	require.Equal(t, "agent:mcp", historyActor)

	refused, err := bob.CallTool(t.Context(), &mcp.CallToolParams{Name: "deleteTodo", Arguments: map[string]any{"id": todo.ID}})
	require.NoError(t, err)
	require.True(t, refused.IsError)
	require.Contains(t, refused.Content[0].(*mcp.TextContent).Text, `"code":"not_found"`)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, application.server.URL+server.MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := application.server.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, "the MCP route is behind the API's authentication")
}

func TestTheAssistantRunsTheToolsAsTheAskingUser(t *testing.T) {
	t.Parallel()
	model := llmtest.NewStubClient(
		llmtest.TurnScript{Events: []llm.Event{
			{Kind: llm.EventToolCall, Tool: &llm.ToolCall{ID: "add", Name: "createTodo", Arguments: `{"body":{"title":"buy milk"}}`}},
			{Kind: llm.EventToolCall, Tool: &llm.ToolCall{ID: "key", Name: "createKey", Arguments: `{}`}},
			{Kind: llm.EventEnd, FinishReason: "tool_calls"},
		}},
		llmtest.TurnScript{Events: []llm.Event{{Kind: llm.EventTextDelta, Text: "Added buy milk."}, {Kind: llm.EventEnd}}},
	)
	application := newTestApplicationWith(t, func(options *applicationOptions) {
		options.Assistant, options.AssistantModel = model, "stub/model"
	})
	key := application.apiKey(t, "alice")
	var requestID string
	ask := func(body string) (int, map[string]any) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, application.server.URL+server.AssistantPath, bytes.NewReader([]byte(body)))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		bearer(key)(request)
		response, err := application.server.Client().Do(request)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		requestID = response.Header.Get("X-Request-ID")
		var answer map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&answer))
		return response.StatusCode, answer
	}

	status, answer := ask(`{"question":"Put milk on the list."}`)
	require.Equal(t, http.StatusOK, status, answer)
	require.Equal(t, "Added buy milk.", answer["reply"])
	actors := application.actors(t, "createTodo")
	require.Len(t, actors, 1)
	require.Equal(t, "agent:assistant", actors[0][1])
	var audited string
	require.NoError(t, application.pool.QueryRow(t.Context(), "SELECT request_id FROM livewire_audit WHERE mutation = 'createTodo'").Scan(&audited))
	require.Equal(t, requestID, audited, "the assistant's write joins the request that asked")
	for _, actor := range application.actors(t, "createKey") {
		require.NotEqual(t, "agent:assistant", actor[1], "the excluded tool never ran")
	}

	requests := model.Requests()
	require.Len(t, requests, 2)
	var toolResults []string
	for _, message := range requests[1].Messages {
		if message.Role == llm.RoleTool {
			toolResults = append(toolResults, message.Content)
		}
	}
	require.Len(t, toolResults, 2)
	require.Contains(t, toolResults[0], "buy milk")
	require.Contains(t, toolResults[1], "unknown tool")

	status, _ = ask(`{}`)
	require.Equal(t, http.StatusBadRequest, status)
}
