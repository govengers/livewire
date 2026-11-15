package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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

// serve mounts the MCP handler behind a stand-in for the application's authentication: the
// bearer key "alice" is a user, "root" an admin, anything else no identity.
func serve(t *testing.T) (*toolfixture.Fixture, *httptest.Server) {
	t.Helper()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	tools, err := toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "mcp", Exclude: []string{"attachNote"}})
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
		}
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
	_, server := serve(t)
	session := connect(t, server, "alice")
	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	require.Len(t, byName, 3)
	require.True(t, byName["getNotes"].Annotations.ReadOnlyHint)
	require.True(t, *byName["deleteNote"].Annotations.DestructiveHint)
	require.False(t, byName["createNote"].Annotations.ReadOnlyHint)
	schema, err := json.Marshal(byName["createNote"].InputSchema)
	require.NoError(t, err)
	require.Contains(t, string(schema), `"required":["body"]`)
}

func TestACallRunsAsTheIdentityOfItsRequest(t *testing.T) {
	t.Parallel()
	fixture, server := serve(t)
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
	fixture, server := serve(t)
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

	result, err = alice.CallTool(t.Context(), create)
	require.NoError(t, err)
	require.True(t, result.IsError, "the same key is the same write")
	require.Len(t, fixture.Notes(), 2)
}

func TestARequestWithoutIdentityIsRefused(t *testing.T) {
	t.Parallel()
	_, server := serve(t)
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
}
