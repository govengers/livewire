package mcpadapter_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/mcpadapter"
	"github.com/govengers/livewire/toolset"
)

// The API's tools as an MCP server behind the application's authentication, here a stand-in
// that makes every request alice's.
func ExampleNew() {
	fixture, err := toolfixture.New()
	if err != nil {
		fmt.Println(err)
		return
	}
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			next.ServeHTTP(writer, request.WithContext(toolfixture.User(request.Context())))
		})
	}
	// snippet:begin mcp
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
	// snippet:end mcp
	defer server.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "example", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = session.Close() }()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, tool := range listed.Tools {
		fmt.Println(tool.Name, tool.Annotations.ReadOnlyHint)
	}
	// Output: getNotes true
}
