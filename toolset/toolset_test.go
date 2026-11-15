package toolset_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/toolset"
)

func newToolset(t *testing.T, change func(*toolset.Options)) (*toolfixture.Fixture, *toolset.Toolset) {
	t.Helper()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	options := &toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "assistant"}
	if change != nil {
		change(options)
	}
	tools, err := toolset.New(options)
	require.NoError(t, err)
	return fixture, tools
}

// refusal asserts err is a toolset refusal with code.
func refusal(t *testing.T, err error, code livewire.Code) {
	t.Helper()
	var refused *toolset.Error
	require.ErrorAs(t, err, &refused)
	require.Equal(t, code, refused.Code, refused.Error())
}

func toolNames(tools []generate.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestNewValidatesItsOptions(t *testing.T) {
	t.Parallel()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	other, err := toolfixture.New()
	require.NoError(t, err)
	for name, options := range map[string]*toolset.Options{
		"nil":              nil,
		"no engine":        {Catalogue: fixture.Catalogue, Agent: "a"},
		"another registry": {Engine: fixture.Engine, Catalogue: other.Catalogue, Agent: "a"},
		"no agent":         {Engine: fixture.Engine, Catalogue: fixture.Catalogue},
		"agent with colon": {Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "a:b"},
		"unknown include":  {Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "a", Include: []string{"getNote"}},
		"unknown exclude":  {Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "a", Exclude: []string{"drop"}},
	} {
		_, err := toolset.New(options)
		require.Error(t, err, name)
	}
}

func TestTheSelectionNarrowsTheTools(t *testing.T) {
	t.Parallel()
	_, all := newToolset(t, nil)
	require.Equal(t, []string{"getNotes", "createNote", "deleteNote", "attachNote"}, toolNames(all.Tools()))
	_, readOnly := newToolset(t, func(options *toolset.Options) { options.ReadOnly = true })
	require.Equal(t, []string{"getNotes"}, toolNames(readOnly.Tools()))
	_, narrowed := newToolset(t, func(options *toolset.Options) {
		options.Include = []string{"getNotes", "createNote", "deleteNote"}
		options.Exclude = []string{"deleteNote"}
	})
	require.Equal(t, []string{"getNotes", "createNote"}, toolNames(narrowed.Tools()))

	_, err := readOnly.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x"}}`), "")
	refusal(t, err, livewire.CodeNotFound)
}

func TestAReadStripsTheColumnsTheIdentityMayNotRead(t *testing.T) {
	t.Parallel()
	_, tools := newToolset(t, nil)
	var page livewire.ListResponse[toolfixture.Note]
	answer, err := tools.Call(toolfixture.User(t.Context()), "getNotes", nil, "")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(answer, &page))
	require.Equal(t, "first", page.Items[0].Title)
	require.Empty(t, page.Items[0].Secret)

	answer, err = tools.Call(toolfixture.Admin(t.Context()), "getNotes", json.RawMessage(`{"limit":5,"sort":"-title"}`), "")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(answer, &page))
	require.Equal(t, "hidden", page.Items[0].Secret)
	require.Equal(t, 5, page.Limit)
}

func TestAFilterIsADocumentAndHonoursTheColumnChecks(t *testing.T) {
	t.Parallel()
	_, tools := newToolset(t, nil)
	_, err := tools.Call(toolfixture.User(t.Context()), "getNotes", json.RawMessage(`{"filter":{"where":{"field":"title","op":"eq","value":"first"}}}`), "")
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "getNotes", json.RawMessage(`{"filter":{"where":{"field":"secret","op":"eq","value":"x"}}}`), "")
	refusal(t, err, livewire.CodeForbiddenField)
}

func TestAWriteIsAuditedAsTheAgentOnBehalfOfTheIdentity(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	answer, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"second"}}`), "")
	require.NoError(t, err)
	var created toolfixture.Note
	require.NoError(t, json.Unmarshal(answer, &created))
	require.Equal(t, "second", created.Title)

	entries := fixture.Audit()
	require.Len(t, entries, 1)
	require.Equal(t, "password:alice", livewire.OwnerKey(entries[0].Identity))
	require.Equal(t, "agent:assistant", livewire.OwnerKey(entries[0].Identity.Real()))
	key, err := uuid.Parse(entries[0].IdempotencyKey)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), key.Version())
}

func TestAnImpersonationStaysBehindTheAgent(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	admin := &auth.Identity{Provider: "password", Subject: "root", Active: true, Roles: []string{auth.RoleAdmin}}
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{Provider: "password", Subject: "alice", Active: true, Impersonator: admin})
	_, err := tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"second"}}`), "")
	require.NoError(t, err)
	real := fixture.Audit()[0].Identity.Real()
	require.Equal(t, "agent:assistant", livewire.OwnerKey(real))
	require.Equal(t, "password:root", livewire.OwnerKey(real.Impersonator))
	require.Equal(t, "password:alice", livewire.OwnerKey(auth.IdentityFrom(ctx)), "the caller's identity is not changed")
}

func TestRefusalsAreTheAPIsAndRunNothing(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	note := fixture.Notes()[0].ID.String()
	_, err := tools.Call(toolfixture.User(t.Context()), "deleteNote", json.RawMessage(`{"id":"`+note+`"}`), "")
	refusal(t, err, livewire.CodeForbidden)
	_, err = tools.Call(t.Context(), "getNotes", nil, "")
	refusal(t, err, livewire.CodeUnauthenticated)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x","secret":"mine"}}`), "")
	refusal(t, err, livewire.CodeForbiddenField)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":""}}`), "")
	refusal(t, err, livewire.CodeInvalidRequest)
	require.Len(t, fixture.Notes(), 1)
	require.Empty(t, fixture.Audit())

	_, err = tools.Call(toolfixture.Admin(t.Context()), "deleteNote", json.RawMessage(`{"id":"`+note+`"}`), "")
	require.NoError(t, err)
	require.Empty(t, fixture.Notes())
}

func TestTheArgumentsAreCheckedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()
	_, tools := newToolset(t, nil)
	ctx := toolfixture.User(t.Context())
	for name, arguments := range map[string]string{
		"not an object":    `[1]`,
		"null":             `null`,
		"unknown argument": `{"title":"x"}`,
		"nested list":      `{"limit":[[1]]}`,
	} {
		_, err := tools.Call(ctx, "getNotes", json.RawMessage(arguments), "")
		refusal(t, err, livewire.CodeInvalidRequest)
		require.Error(t, err, name)
	}
	_, err := tools.Call(toolfixture.Admin(t.Context()), "deleteNote", json.RawMessage(`{"id":"not-a-uuid"}`), "")
	refusal(t, err, livewire.CodeInvalidRequest)
	_, err = tools.Call(ctx, "getNote", nil, "")
	refusal(t, err, livewire.CodeNotFound)
	_, err = tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"x"}}`), "not-a-key")
	refusal(t, err, livewire.CodeInvalidRequest)
	require.EqualError(t, err, "invalid_request: The idempotency key is not a UUID. idempotencyKey: not a UUID")
}

func TestOneIdempotencyKeyIsOneWrite(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	key := uuid.Must(uuid.NewV7()).String()
	_, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"once"}}`), key)
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"once"}}`), key)
	refusal(t, err, livewire.CodeInProgress) // the fixture's store answers a known key so
	require.Len(t, fixture.Notes(), 2)
}

func TestTheConfirmationGuardsWritesOnly(t *testing.T) {
	t.Parallel()
	var asked []string
	fixture, tools := newToolset(t, func(options *toolset.Options) {
		options.Confirm = func(_ context.Context, call *toolset.Call) error {
			asked = append(asked, call.Tool.Name+" "+string(call.Arguments))
			return errors.New("the user said no, and this text stays with the application")
		}
	})
	_, err := tools.Call(toolfixture.User(t.Context()), "getNotes", nil, "")
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x"}}`), "")
	refusal(t, err, toolset.CodeDeclined)
	require.NotContains(t, err.Error(), "the user said no")
	require.Equal(t, []string{`createNote {"body":{"title":"x"}}`}, asked)
	require.Len(t, fixture.Notes(), 1)
}

func TestTheAnswerCapRefusesReadsAndKeepsWritesCommitted(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, func(options *toolset.Options) { options.MaximumResultBytes = 20 })
	_, err := tools.Call(toolfixture.User(t.Context()), "getNotes", nil, "")
	refusal(t, err, toolset.CodeTooLarge)
	answer, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"a long title past the cap"}}`), "")
	require.NoError(t, err)
	require.Contains(t, string(answer), `"committed":true`)
	require.Len(t, fixture.Notes(), 2)

	_, uncapped := newToolset(t, func(options *toolset.Options) { options.MaximumResultBytes = -1 })
	_, err = uncapped.Call(toolfixture.User(t.Context()), "getNotes", nil, "")
	require.NoError(t, err)
}

func TestAnUploadTakesTextOrBase64(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	note := fixture.Notes()[0].ID.String()
	ctx := toolfixture.User(t.Context())
	answer, err := tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"text/plain","content":"hello"}`), "")
	require.NoError(t, err)
	require.JSONEq(t, `{"size":5,"content":"hello"}`, string(answer))
	encoded := base64.StdEncoding.EncodeToString([]byte("bytes"))
	answer, err = tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"text/plain","content":"`+encoded+`","contentEncoding":"base64"}`), "")
	require.NoError(t, err)
	require.JSONEq(t, `{"size":5,"content":"bytes"}`, string(answer))

	for _, arguments := range []string{
		`{"id":"` + note + `","contentType":"text/plain"}`,
		`{"id":"` + note + `","contentType":"text/plain","content":"%%","contentEncoding":"base64"}`,
		`{"id":"` + note + `","contentType":"text/plain","content":"x","contentEncoding":"hex"}`,
		`{"id":"` + note + `","contentType":"image/png","content":"x"}`,
		`{"id":"` + note + `","contentType":"text/plain","content":7}`,
	} {
		_, err := tools.Call(ctx, "attachNote", json.RawMessage(arguments), "")
		refusal(t, err, livewire.CodeInvalidRequest)
	}
}

func TestAnInternalErrorKeepsItsText(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	note := fixture.Notes()[0].ID.String()
	_, err := tools.Call(toolfixture.User(t.Context()), "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"text/plain","content":"`+toolfixture.FailingContent+`"}`), "")
	refusal(t, err, livewire.CodeInternal)
	require.NotContains(t, err.Error(), "hunter2")
}

func TestACancelledCallStartsNothing(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	ctx, cancel := context.WithCancel(toolfixture.User(t.Context()))
	cancel()
	_, err := tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"late"}}`), "")
	refusal(t, err, livewire.CodeInternal)
	require.Len(t, fixture.Notes(), 1)
}
