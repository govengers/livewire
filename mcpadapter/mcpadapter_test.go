package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/mcpadapter"
	"github.com/govengers/livewire/toolset"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// serve mounts the MCP handler behind a stand-in for the application's authentication and
// its wide-event middleware: the bearer key "alice" is a user, "root" an admin, "impersonated"
// alice impersonated by an inactive root, anything else no identity; every answer carries
// the request id "request-1". change, when set, changes the toolset's options.
func serve(t *testing.T, change func(*toolset.Options)) (*toolfixture.Fixture, *httptest.Server) {
	t.Helper()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	options := &toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "mcp", Exclude: []string{"attachNote"}}
	if change != nil {
		change(options)
	}
	tools, err := toolset.New(options)
	require.NoError(t, err)
	handler, err := mcpadapter.New(&mcpadapter.Options{Toolset: tools, Name: "notes", Version: "1.0.0"})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := request.Context()
		switch request.Header.Get("Authorization") {
		case "Bearer alice":
			ctx = toolfixture.User(ctx)
		case "Bearer root":
			ctx = toolfixture.Admin(ctx)
		case "Bearer impersonated":
			ctx = auth.WithIdentity(ctx, &auth.Identity{Provider: "password", Subject: "alice", Active: true, Impersonator: &auth.Identity{Provider: "password", Subject: "root", Active: false}})
		}
		writer.Header().Set(mcpadapter.RequestIDHeader, "request-1")
		handler.ServeHTTP(writer, request.WithContext(ctx))
	}))
	t.Cleanup(server.Close)
	return fixture, server
}

// bearer adds an Authorization header to every request.
type bearer struct {
	key  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+b.key)
	return b.next.RoundTrip(request)
}

func connect(t *testing.T, server *httptest.Server, key string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: &http.Client{Transport: bearer{key: key, next: server.Client().Transport}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func text(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, result.Content, 1)
	content, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return content.Text
}

func TestAClientListsTheSelectedToolsWithTheirSchemas(t *testing.T) {
	t.Parallel()
	_, server := serve(t, nil)
	session := connect(t, server, "alice")
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	require.Len(t, byName, 3)
	require.True(t, byName["getNotes"].Annotations.ReadOnlyHint)
	require.False(t, byName["createNote"].Annotations.ReadOnlyHint)
	for _, tool := range listed.Tools {
		require.Nil(t, tool.Annotations.DestructiveHint, "a write may overwrite: the protocol's default, destructive")
	}
	require.True(t, byName["deleteNote"].Annotations.IdempotentHint)
	require.False(t, byName["createNote"].Annotations.IdempotentHint)
	schema, err := json.Marshal(byName["createNote"].InputSchema)
	require.NoError(t, err)
	require.Contains(t, string(schema), `"required":["body"]`)
}

func TestACallRunsAsTheIdentityOfItsRequest(t *testing.T) {
	t.Parallel()
	fixture, server := serve(t, nil)
	alice, root := connect(t, server, "alice"), connect(t, server, "root")
	list := &mcp.CallToolParams{Name: "getNotes", Arguments: map[string]any{"limit": 10}}

	result, err := alice.CallTool(t.Context(), list)
	require.NoError(t, err)
	require.False(t, result.IsError, text(t, result))
	require.NotContains(t, text(t, result), "hidden", "the guarded column is stripped for the user")
	result, err = root.CallTool(t.Context(), list)
	require.NoError(t, err)
	require.Contains(t, text(t, result), "hidden")

	note := fixture.Notes()[0].ID.String()
	result, err = alice.CallTool(t.Context(), &mcp.CallToolParams{Name: "deleteNote", Arguments: map[string]any{"id": note}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	var refused toolset.Error
	require.NoError(t, json.Unmarshal([]byte(text(t, result)), &refused))
	require.Equal(t, livewire.CodeForbidden, refused.Code)

	result, err = alice.CallTool(t.Context(), &mcp.CallToolParams{Name: "createNote", Arguments: map[string]any{"body": map[string]any{"title": "x", "secret": "mine"}}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, text(t, result), `"code":"forbidden_field"`)
	require.Len(t, fixture.Notes(), 1)
}

func TestAWriteIsAuditedAsTheAgentWithTheClientsKey(t *testing.T) {
	t.Parallel()
	fixture, server := serve(t, nil)
	alice := connect(t, server, "alice")
	key := uuid.Must(uuid.NewV7()).String()
	create := &mcp.CallToolParams{
		Name: "createNote", Arguments: map[string]any{"body": map[string]any{"title": "over mcp"}},
		Meta: mcp.Meta{mcpadapter.MetaIdempotencyKey: key},
	}
	result, err := alice.CallTool(t.Context(), create)
	require.NoError(t, err)
	require.False(t, result.IsError, text(t, result))
	require.Contains(t, text(t, result), "over mcp")

	entries := fixture.Audit()
	require.Len(t, entries, 1)
	require.Equal(t, "password:alice", livewire.OwnerKey(entries[0].Identity))
	require.Equal(t, "agent:mcp", livewire.OwnerKey(entries[0].Identity.Real()))
	require.Equal(t, key, entries[0].IdempotencyKey)
	require.Equal(t, "request-1", entries[0].RequestID, "the request id the middleware answers with")

	result, err = alice.CallTool(t.Context(), create)
	require.NoError(t, err)
	require.True(t, result.IsError, "the same key is the same write")
	require.Len(t, fixture.Notes(), 2)
}

func TestARequestWithoutIdentityIsRefused(t *testing.T) {
	t.Parallel()
	_, server := serve(t, nil)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	var body livewire.ErrorBody
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	require.Equal(t, livewire.CodeUnauthenticated, body.Code)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	_, err = client.Connect(context.WithoutCancel(t.Context()), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: server.Client()}, nil)
	require.Error(t, err)
}

func TestNewRefusesIncompleteOptions(t *testing.T) {
	t.Parallel()
	_, err := mcpadapter.New(nil)
	require.Error(t, err)
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	tools, err := toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "mcp"})
	require.NoError(t, err)
	_, err = mcpadapter.New(&mcpadapter.Options{Toolset: tools})
	require.Error(t, err, "a server needs a name")
	_, err = mcpadapter.New(&mcpadapter.Options{Toolset: tools, Name: "notes", MaximumRequestBytes: -1})
	require.Error(t, err, "a server keeps a body cap")
}

func TestAKeyThatIsNoStringIsRefused(t *testing.T) {
	t.Parallel()
	fixture, server := serve(t, nil)
	result, err := connect(t, server, "alice").CallTool(t.Context(), &mcp.CallToolParams{
		Name: "createNote", Arguments: map[string]any{"body": map[string]any{"title": "x"}}, Meta: mcp.Meta{mcpadapter.MetaIdempotencyKey: 12345},
	})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, text(t, result), `"code":"invalid_request"`)
	require.Len(t, fixture.Notes(), 1, "a retry the client believes safe never writes under a fresh key")
}

func TestAPanicIsAnInternalRefusalAndTheServerLives(t *testing.T) {
	t.Parallel()
	_, server := serve(t, func(options *toolset.Options) {
		options.Confirm = func(context.Context, *toolset.Call) error { panic("secret internal state: hunter2") }
	})
	alice := connect(t, server, "alice")
	result, err := alice.CallTool(t.Context(), &mcp.CallToolParams{Name: "createNote", Arguments: map[string]any{"body": map[string]any{"title": "x"}}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, text(t, result), `"code":"internal"`)
	require.NotContains(t, text(t, result), "hunter2")
	result, err = alice.CallTool(t.Context(), &mcp.CallToolParams{Name: "getNotes"})
	require.NoError(t, err)
	require.False(t, result.IsError, "the server still answers")
}

func TestAnInactiveImpersonatorIsRefusedThroughTheAgent(t *testing.T) {
	t.Parallel()
	fixture, server := serve(t, nil)
	result, err := connect(t, server, "impersonated").CallTool(t.Context(), &mcp.CallToolParams{Name: "createNote", Arguments: map[string]any{"body": map[string]any{"title": "x"}}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, text(t, result), `"code":"forbidden"`)
	require.Len(t, fixture.Notes(), 1)
}

func TestARequestBehindASameHostProxyIsServed(t *testing.T) {
	t.Parallel()
	_, server := serve(t, nil)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"getNotes","arguments":{}}}`))
	require.NoError(t, err)
	request.Host = "app.example.com" // a reverse proxy on the same host that keeps the Host header
	request.Header.Set("Authorization", "Bearer alice")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
}
