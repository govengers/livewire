// Package mcpadapter serves a livewire toolset (D85, D86) as a Model Context Protocol server
// on the Streamable HTTP transport of github.com/modelcontextprotocol/go-sdk: every tool the
// toolset selects is an MCP tool, and a tools/call runs Toolset.Call in process under the
// identity of that very HTTP request. It is a module of its own, so livewire does not
// require the SDK.
//
// The handler sits behind the application's own authentication (an API key or a session),
// which puts the identity into the request's context (auth.WithIdentity); a request without
// one is refused with 401 before the protocol sees it:
//
//	handler, err := mcpadapter.New(&mcpadapter.Options{Toolset: toolset, Name: "todo", Version: version})
//	api.POST("/api/v1/mcp", gin.WrapH(handler)) // the group runs the authentication middleware
//
// The server is stateless (no Mcp-Session-Id, POST only, JSON answers): each request is a
// session of its own, so an identity never outlives its request and any replica can answer.
// A client may set the call's Idempotency-Key (a UUID) as `_meta.idempotencyKey` of
// tools/call to retry it safely (a key that is present and no UUID, "" included, is refused);
// without one every call is its own write. A write's request
// id in the audit and the history is the X-Request-ID the application's middleware answers
// with (govengers/wide-event-logger's does), as on REST, so it joins the request's log line.
//
// The handler needs no DNS rebinding protection of its own, since every request carries an
// identity the application authenticated, so it is off and a same-host reverse proxy may keep
// the Host header. A request body is at most Options.MaximumRequestBytes; an upload's content
// travels in base64 in it, a third larger than the file.
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/govengers/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/toolset"
)

// MetaIdempotencyKey is the member of a tools/call's _meta that carries the call's
// Idempotency-Key.
const MetaIdempotencyKey = "idempotencyKey"

// RequestIDHeader is the response header whose value, set by the application's middleware
// before the handler runs, is the request id of the calls of that request.
const RequestIDHeader = "X-Request-ID"

// DefaultMaximumRequestBytes is the request body cap when Options.MaximumRequestBytes is zero.
const DefaultMaximumRequestBytes = mcp.DefaultMaxRequestBodyBytes

// Options configures New. Toolset is required: the tools, their selection, the
// confirmation hook, the answer cap and the agent's name are its options. Name (required) and
// Version describe the server to clients in its initialize answer; Instructions, when set,
// tell a client's model how to use the tools. MaximumRequestBytes caps a request's body (0:
// DefaultMaximumRequestBytes, 4 MiB); a server with an upload larger than about three
// quarters of it raises it, since the content arrives in base64.
type Options struct {
	Toolset             *toolset.Toolset
	Name                string
	Version             string
	Instructions        string
	MaximumRequestBytes int64
}

// New validates options and builds the MCP handler. A tool's annotations follow its kind: a
// read is read-only, a PUT or DELETE idempotent, and every write keeps the protocol's
// default, destructive, since a POST or a PATCH may overwrite as much as a DELETE removes. A
// tool call's
// answer is the tool's JSON as one text content; a refusal is a result with isError set
// whose text is the *toolset.Error as JSON ({code, message, fields}), a closed code and a
// message safe to show.
func New(options *Options) (http.Handler, error) {
	if options == nil || options.Toolset == nil || options.Name == "" || options.MaximumRequestBytes < 0 {
		return nil, errors.New("mcpadapter: Options needs a Toolset, a Name and a MaximumRequestBytes of 0 or more")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: options.Name, Version: options.Version}, &mcp.ServerOptions{Instructions: options.Instructions})
	tools := options.Toolset
	for _, tool := range tools.Tools() {
		name := tool.Name
		server.AddTool(&mcp.Tool{
			Name: name, Description: tool.Description, InputSchema: tool.Parameters,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:   !tool.Mutating,
				IdempotentHint: !tool.Mutating || tool.Method == http.MethodPut || tool.Method == http.MethodDelete,
			},
		}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// a retry the client believes safe must not write twice: a key that is present is a
			// UUID or refused, "" included (an unset variable on the client's side)
			key, isText := request.Params.Meta[MetaIdempotencyKey].(string)
			if member := request.Params.Meta[MetaIdempotencyKey]; member != nil && (!isText || key == "") {
				return refusal(&toolset.Error{Code: livewire.CodeInvalidRequest, Message: "The idempotency key is not a UUID.", Fields: map[string]string{"idempotencyKey": "not a UUID"}}), nil
			}
			requestID, _ := ctx.Value(requestIDKey{}).(string)
			answer, err := tools.Call(ctx, name, request.Params.Arguments, toolset.CallOptions{IdempotencyKey: key, RequestID: requestID})
			if err != nil {
				return refusal(err), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(answer)}}}, nil
		})
	}
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, DisableLocalhostProtection: true, MaxRequestBodyBytes: options.MaximumRequestBytes,
	})
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if auth.IdentityFrom(request.Context()) == nil {
			writeUnauthenticated(writer)
			return
		}
		if requestID := writer.Header().Get(RequestIDHeader); requestID != "" {
			request = request.WithContext(context.WithValue(request.Context(), requestIDKey{}, requestID))
		}
		streamable.ServeHTTP(writer, request)
	}), nil
}

// requestIDKey carries a request's id from the handler to its tool calls.
type requestIDKey struct{}

func refusal(err error) *mcp.CallToolResult {
	refused, ok := errors.AsType[*toolset.Error](err)
	if !ok { // Toolset.Call fails with an *Error only; anything else is answered without its text
		refused = &toolset.Error{Code: livewire.CodeInternal, Message: "internal error"}
	}
	encoded, _ := json.Marshal(refused) //nolint:errchkjson // strings and a map of strings always encode
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}
}

// writeUnauthenticated answers livewire's 401 error body.
func writeUnauthenticated(writer http.ResponseWriter) {
	status, body := livewire.ResponseFor(auth.ErrUnauthenticated)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body) //nolint:errchkjson // the client went away; nothing is left to tell it
}
