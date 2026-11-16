package toolset_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/query"
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

	_, err := readOnly.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x"}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeNotFound)

	_, mainReads := newToolset(t, func(options *toolset.Options) { options.ExcludeReads = []livewire.ReadKind{livewire.ReadMain} })
	require.Equal(t, []string{"createNote", "deleteNote", "attachNote"}, toolNames(mainReads.Tools()))
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	_, err = toolset.New(&toolset.Options{Engine: fixture.Engine, Catalogue: fixture.Catalogue, Agent: "a", ExcludeReads: []livewire.ReadKind{"histories"}})
	require.ErrorContains(t, err, "no read kind")
}

func TestAReadStripsTheColumnsTheIdentityMayNotRead(t *testing.T) {
	t.Parallel()
	_, tools := newToolset(t, nil)
	var page livewire.ListResponse[toolfixture.Note]
	answer, err := tools.Call(toolfixture.User(t.Context()), "getNotes", nil, toolset.CallOptions{})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(answer, &page))
	require.Equal(t, "first", page.Items[0].Title)
	require.Empty(t, page.Items[0].Secret)

	answer, err = tools.Call(toolfixture.Admin(t.Context()), "getNotes", json.RawMessage(`{"limit":5,"sort":"-title"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(answer, &page))
	require.Equal(t, "hidden", page.Items[0].Secret)
	require.Equal(t, 5, page.Limit)
}

func TestAFilterIsADocumentAndHonoursTheColumnChecks(t *testing.T) {
	t.Parallel()
	_, tools := newToolset(t, nil)
	_, err := tools.Call(toolfixture.User(t.Context()), "getNotes", json.RawMessage(`{"filter":{"where":{"field":"title","op":"eq","value":"first"}}}`), toolset.CallOptions{})
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "getNotes", json.RawMessage(`{"filter":{"where":{"field":"secret","op":"eq","value":"x"}}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeForbiddenField)
}

func TestAWriteIsAuditedAsTheAgentOnBehalfOfTheIdentity(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	answer, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"second"}}`), toolset.CallOptions{})
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

func TestAnImpersonatorStaysTheRealActorBehindTheAgent(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	impersonated := func(active bool) context.Context {
		admin := &auth.Identity{Provider: "password", Subject: "root", Active: active, Roles: []string{auth.RoleAdmin}}
		return auth.WithIdentity(t.Context(), &auth.Identity{Provider: "password", Subject: "alice", Active: true, Impersonator: admin})
	}
	ctx := impersonated(true)
	_, err := tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"second"}}`), toolset.CallOptions{})
	require.NoError(t, err)
	entry := fixture.Audit()[0]
	require.Equal(t, "password:alice", livewire.OwnerKey(entry.Identity))
	require.Equal(t, "agent:assistant@password:root", livewire.OwnerKey(entry.Identity.Real()), "the audit names the human behind the agent")
	require.Equal(t, "password:alice", livewire.OwnerKey(auth.IdentityFrom(ctx)), "the caller's identity is not changed")

	_, err = tools.Call(impersonated(false), "createNote", json.RawMessage(`{"body":{"title":"third"}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeForbidden) // a deactivated impersonator is refused through the agent as on REST
	inactive := &auth.Identity{Provider: "password", Subject: "alice", Active: false}
	_, err = tools.Call(auth.WithIdentity(t.Context(), inactive), "getNotes", nil, toolset.CallOptions{})
	refusal(t, err, livewire.CodeForbidden)
	require.Len(t, fixture.Notes(), 2)
}

func TestRefusalsAreTheAPIsAndRunNothing(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	note := fixture.Notes()[0].ID.String()
	_, err := tools.Call(toolfixture.User(t.Context()), "deleteNote", json.RawMessage(`{"id":"`+note+`"}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeForbidden)
	_, err = tools.Call(t.Context(), "getNotes", nil, toolset.CallOptions{})
	refusal(t, err, livewire.CodeUnauthenticated)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x","secret":"mine"}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeForbiddenField)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":""}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInvalidRequest)
	require.Len(t, fixture.Notes(), 1)
	require.Empty(t, fixture.Audit())

	_, err = tools.Call(toolfixture.Admin(t.Context()), "deleteNote", json.RawMessage(`{"id":"`+note+`"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.Empty(t, fixture.Notes())
}

func TestTheArgumentsAreCheckedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()
	_, tools := newToolset(t, nil)
	ctx := toolfixture.User(t.Context())
	for name, arguments := range map[string]string{
		"not an object":    `[1]`,
		"a string":         `"x"`,
		"unknown argument": `{"title":"x"}`,
		"nested list":      `{"limit":[[1]]}`,
	} {
		_, err := tools.Call(ctx, "getNotes", json.RawMessage(arguments), toolset.CallOptions{})
		refusal(t, err, livewire.CodeInvalidRequest)
		require.Error(t, err, name)
	}
	_, err := tools.Call(toolfixture.Admin(t.Context()), "deleteNote", json.RawMessage(`{"id":"not-a-uuid"}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInvalidRequest)
	_, err = tools.Call(ctx, "getNote", nil, toolset.CallOptions{})
	refusal(t, err, livewire.CodeNotFound)
	_, err = tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"x"}}`), toolset.CallOptions{IdempotencyKey: "not-a-key"})
	refusal(t, err, livewire.CodeInvalidRequest)
	require.EqualError(t, err, "invalid_request: The idempotency key is not a UUID. idempotencyKey: not a UUID")
}

func TestAWriteCarriesTheRequestIDItIsGiven(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	_, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x"}}`), toolset.CallOptions{RequestID: "run-1"})
	require.NoError(t, err)
	require.Equal(t, "run-1", fixture.Audit()[0].RequestID)
}

func TestOneIdempotencyKeyIsOneWrite(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	key := uuid.Must(uuid.NewV7()).String()
	_, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"once"}}`), toolset.CallOptions{IdempotencyKey: key})
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"once"}}`), toolset.CallOptions{IdempotencyKey: key})
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
	_, err := tools.Call(toolfixture.User(t.Context()), "getNotes", nil, toolset.CallOptions{})
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x"}}`), toolset.CallOptions{})
	refusal(t, err, toolset.CodeDeclined)
	require.NotContains(t, err.Error(), "the user said no")
	require.Equal(t, []string{`createNote {"body":{"title":"x"}}`}, asked)
	require.Len(t, fixture.Notes(), 1)
}

func TestTheAnswerCapRefusesReadsAndKeepsWritesCommitted(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, func(options *toolset.Options) { options.MaximumResultBytes = 20 })
	_, err := tools.Call(toolfixture.User(t.Context()), "getNotes", nil, toolset.CallOptions{})
	refusal(t, err, toolset.CodeTooLarge)
	answer, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"a long title past the cap"}}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.Contains(t, string(answer), `"committed":true`)
	require.Len(t, fixture.Notes(), 2)

	_, uncapped := newToolset(t, func(options *toolset.Options) { options.MaximumResultBytes = -1 })
	_, err = uncapped.Call(toolfixture.User(t.Context()), "getNotes", nil, toolset.CallOptions{})
	require.NoError(t, err)
}

func TestAnUploadTakesTextOrBase64(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	note := fixture.Notes()[0].ID.String()
	ctx := toolfixture.User(t.Context())
	answer, err := tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"text/plain","content":"hello"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.JSONEq(t, `{"size":5,"content":"hello"}`, string(answer))
	encoded := base64.StdEncoding.EncodeToString([]byte("bytes"))
	answer, err = tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"text/plain","content":"`+encoded+`","contentEncoding":"base64"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.JSONEq(t, `{"size":5,"content":"bytes"}`, string(answer))

	png := base64.StdEncoding.EncodeToString([]byte("PNG"))
	answer, err = tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"image/png","content":"`+png+`","contentEncoding":"base64"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.JSONEq(t, `{"size":3,"content":"PNG"}`, string(answer))
	_, err = tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"image/png","content":"`+png+`"}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInvalidRequest)
	require.ErrorContains(t, err, "contentEncoding: required", "base64 text is never stored as a binary file's bytes")
	answer, err = tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"TEXT/Plain; charset=utf-8","content":"hi"}`), toolset.CallOptions{})
	require.NoError(t, err, "a media type is case-insensitive, as the engine reads it")
	require.JSONEq(t, `{"size":2,"content":"hi"}`, string(answer))
	_, err = tools.Call(ctx, "attachNote", json.RawMessage(`{"id":"`+note+`","content":"hi"}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInvalidRequest)
	require.ErrorContains(t, err, "file: the content type is not accepted", "a missing type is refused as the type, not as the encoding")

	for _, arguments := range []string{
		`{"id":"` + note + `","contentType":"text/plain"}`,
		`{"id":"` + note + `","contentType":"text/plain","content":"%%","contentEncoding":"base64"}`,
		`{"id":"` + note + `","contentType":"text/plain","content":"x","contentEncoding":"hex"}`,
		`{"id":"` + note + `","contentType":"image/gif","content":"x","contentEncoding":"text"}`,
		`{"id":"` + note + `","contentType":"text/plain","content":7}`,
	} {
		_, err := tools.Call(ctx, "attachNote", json.RawMessage(arguments), toolset.CallOptions{})
		refusal(t, err, livewire.CodeInvalidRequest)
	}
}

func TestAnInternalErrorKeepsItsText(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	note := fixture.Notes()[0].ID.String()
	_, err := tools.Call(toolfixture.User(t.Context()), "attachNote", json.RawMessage(`{"id":"`+note+`","contentType":"text/plain","content":"`+toolfixture.FailingContent+`"}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInternal)
	require.NotContains(t, err.Error(), "hunter2")
}

func TestACancelledCallStartsNothing(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, nil)
	ctx, cancel := context.WithCancel(toolfixture.User(t.Context()))
	cancel()
	_, err := tools.Call(ctx, "createNote", json.RawMessage(`{"body":{"title":"late"}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInternal)
	require.Len(t, fixture.Notes(), 1)
}

func TestAPanicIsAnsweredAsInternalAndReported(t *testing.T) {
	t.Parallel()
	fixture, tools := newToolset(t, func(options *toolset.Options) {
		options.Confirm = func(context.Context, *toolset.Call) error { panic("secret internal state: hunter2") }
	})
	_, err := tools.Call(toolfixture.User(t.Context()), "createNote", json.RawMessage(`{"body":{"title":"x"}}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInternal)
	require.NotContains(t, err.Error(), "hunter2")
	outcomes := fixture.Outcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, livewire.OutcomeCall, outcomes[0].Kind)
	require.ErrorContains(t, outcomes[0].Err, "hunter2", "the application's Observer sees what the model does not")
	require.Len(t, fixture.Notes(), 1)
}

// thing is an object whose parameters carry the names a mutation's or an upload's own
// members have, which a read may use.
type thingParameters struct {
	Body    string `path:"body"`
	Content string `query:"content"`
}

func TestAReadPassesEveryArgumentAsAParameter(t *testing.T) {
	t.Parallel()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{})
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewObject("thing", "/things/{body}", &livewire.ObjectOptions[thingParameters, thingParameters]{
		Read: authorization.IsUser(),
		Load: func(_ context.Context, parameters *thingParameters) (*thingParameters, error) { return parameters, nil },
	})))
	engine, err := toolfixture.NewEngine(registry)
	require.NoError(t, err)
	catalogue, err := generate.NewCatalogue(registry, nil)
	require.NoError(t, err)
	tools, err := toolset.New(&toolset.Options{Engine: engine, Catalogue: catalogue, Agent: "assistant"})
	require.NoError(t, err)
	answer, err := tools.Call(toolfixture.User(t.Context()), "getThing", json.RawMessage(`{"body":"abc","content":"def"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.JSONEq(t, `{"Body":"abc","Content":"def"}`, string(answer))
}

type taggedParameters struct {
	Tags []string `query:"tags"`
}

func TestAListValueHoldsNoComma(t *testing.T) {
	t.Parallel()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{})
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewObject("tagged", "/tagged", &livewire.ObjectOptions[taggedParameters, taggedParameters]{
		Read: authorization.IsUser(),
		Load: func(_ context.Context, parameters *taggedParameters) (*taggedParameters, error) {
			return parameters, nil
		},
	})))
	engine, err := toolfixture.NewEngine(registry)
	require.NoError(t, err)
	catalogue, err := generate.NewCatalogue(registry, nil)
	require.NoError(t, err)
	tools, err := toolset.New(&toolset.Options{Engine: engine, Catalogue: catalogue, Agent: "assistant"})
	require.NoError(t, err)
	ctx := toolfixture.User(t.Context())
	answer, err := tools.Call(ctx, "getTagged", json.RawMessage(`{"tags":["a","b"]}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.JSONEq(t, `{"Tags":["a","b"]}`, string(answer))
	_, err = tools.Call(ctx, "getTagged", json.RawMessage(`{"tags":["a,b","c"]}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInvalidRequest)
	require.ErrorContains(t, err, "tags: a list value holds no comma")
}

// search is an object and a mutation whose own parameter is named filterId, as the reserved
// parameter of a filterable read is; the catalogue keeps it (D85 drops the reserved one only).
type searchParameters struct {
	FilterID string `path:"filterId"`
}

type renameParameters struct {
	FilterID string `query:"filterId"`
}

func TestADeclaredFilterIDIsAnArgument(t *testing.T) {
	t.Parallel()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{})
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		livewire.NewObject("search", "/saved-searches/{filterId}", &livewire.ObjectOptions[searchParameters, searchParameters]{
			Read: authorization.IsUser(),
			Load: func(_ context.Context, parameters *searchParameters) (*searchParameters, error) {
				return parameters, nil
			},
		}),
		livewire.NewMutation("renameSearch", http.MethodPost, "/saved-searches", &livewire.MutationOptions[renameParameters, livewire.NoBody, renameParameters]{
			Check: authorization.IsUser(),
			Handle: func(_ context.Context, parameters *renameParameters, _ *livewire.NoBody) (*renameParameters, error) {
				return parameters, nil
			},
		}),
	))
	catalogue, err := generate.NewCatalogue(registry, nil)
	require.NoError(t, err)
	for _, name := range []string{"getSearch", "renameSearch"} {
		tool, ok := catalogue.Lookup(name)
		require.True(t, ok)
		require.Contains(t, string(tool.Parameters), `"filterId"`, name)
	}
	engine, err := toolfixture.NewEngine(registry)
	require.NoError(t, err)
	tools, err := toolset.New(&toolset.Options{Engine: engine, Catalogue: catalogue, Agent: "assistant"})
	require.NoError(t, err)
	answer, err := tools.Call(toolfixture.User(t.Context()), "getSearch", json.RawMessage(`{"filterId":"abc"}`), toolset.CallOptions{})
	require.NoError(t, err)
	require.JSONEq(t, `{"FilterID":"abc"}`, string(answer))
}

func TestAPanicOfAReadIsReportedUnderTheEnginesTarget(t *testing.T) {
	t.Parallel()
	var outcomes []livewire.Outcome
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{Observer: func(_ context.Context, outcome livewire.Outcome) { outcomes = append(outcomes, outcome) }})
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewResource("things", "/things", &livewire.ResourceOptions[toolfixture.Note]{
		Read:   func(context.Context) error { panic("secret internal state: hunter2") },
		Fields: query.Fields{{Name: "id", Type: query.TypeUUID, SQL: "things.id", Filter: true, Sort: true}},
		Item:   &livewire.ItemKey{Parameter: "id", Field: "id"},
		List: func(context.Context, *query.ListQuery) (*livewire.Page[toolfixture.Note], error) {
			return &livewire.Page[toolfixture.Note]{}, nil
		},
		Get: func(context.Context, *query.ListQuery) (*toolfixture.Note, error) { return &toolfixture.Note{}, nil },
	})))
	engine, err := toolfixture.NewEngine(registry)
	require.NoError(t, err)
	catalogue, err := generate.NewCatalogue(registry, nil)
	require.NoError(t, err)
	tools, err := toolset.New(&toolset.Options{Engine: engine, Catalogue: catalogue, Agent: "assistant"})
	require.NoError(t, err)
	_, err = tools.Call(toolfixture.User(t.Context()), "getThingsItem", json.RawMessage(`{"id":"019a0000-0000-7000-8000-000000000001"}`), toolset.CallOptions{})
	refusal(t, err, livewire.CodeInternal)
	require.Len(t, outcomes, 1)
	require.Equal(t, "things.item", outcomes[0].Target, "counted under the item read, not the list")
	require.Positive(t, outcomes[0].Duration)
}
