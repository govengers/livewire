package generate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/internal/toolfixture"
	"github.com/govengers/livewire/query"
)

// compileSchema compiles a tool schema as JSON Schema draft 2020-12, which every one must be.
func compileSchema(t *testing.T, schema json.RawMessage) *jsonschema.Schema {
	t.Helper()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource("tool.json", document))
	compiled, err := compiler.Compile("tool.json")
	require.NoError(t, err, string(schema))
	return compiled
}

func valid(t *testing.T, schema *jsonschema.Schema, instance string) error {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(instance))
	require.NoError(t, err)
	return schema.Validate(value)
}

func TestTheCatalogueDescribesEveryOperationAsATool(t *testing.T) {
	t.Parallel()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	tools := fixture.Catalogue.Tools()
	type summary struct {
		name, kind, method, path string
		mutating                 bool
	}
	var summaries []summary
	for _, tool := range tools {
		summaries = append(summaries, summary{tool.Name, string(tool.Kind), tool.Method, tool.Path, tool.Mutating})
		for _, schema := range []json.RawMessage{tool.Parameters, tool.Body, tool.Result} {
			if schema == nil {
				continue
			}
			compileSchema(t, schema)
			for _, openAPIOnly := range []string{`"nullable"`, `"x-livewire`, `#/components/`} {
				require.NotContains(t, string(schema), openAPIOnly, tool.Name)
			}
		}
	}
	require.Equal(t, []summary{
		{"getNotes", "read", http.MethodGet, "/api/notes", false},
		{"createNote", "mutation", http.MethodPost, "/api/notes", true},
		{"deleteNote", "mutation", http.MethodDelete, "/api/notes/{id}", true},
		{"attachNote", "upload", http.MethodPost, "/api/notes/{id}/attachments", true},
	}, summaries)
	require.True(t, strings.HasPrefix(tools[0].Description, "The notes. Lists notes: one page"), tools[0].Description)
	require.Equal(t, "Changes data: DELETE /notes/{id}. Answers nothing.", tools[2].Description)
	require.JSONEq(t, `{"type":"null"}`, string(tools[2].Result))
	require.Nil(t, tools[2].Body)

	list := compileSchema(t, tools[0].Parameters)
	require.NoError(t, valid(t, list, `{}`))
	require.NoError(t, valid(t, list, `{"limit":5,"sort":"-title","filter":{"where":{"and":[{"field":"title","op":"contains","value":"a"}]},"search":"x"}}`))
	require.Error(t, valid(t, list, `{"filter":{"where":{"field":"title","op":"gt","value":1}}}`), "the conditions are typed per field")
	require.Error(t, valid(t, list, `{"page":1}`), "no member beside the declared ones")
	require.Error(t, valid(t, list, `{"filterId":"x"}`), "stored filters are no tools")

	create := compileSchema(t, tools[1].Parameters)
	require.NoError(t, valid(t, create, `{"body":{"title":"x"}}`))
	require.Error(t, valid(t, create, `{}`), "the body is required")
	require.Error(t, valid(t, create, `{"body":{"title":"x","color":"red"}}`), "the body has its members")
	require.NoError(t, valid(t, compileSchema(t, tools[1].Body), `{"title":"x"}`))

	remove := compileSchema(t, tools[2].Parameters)
	require.Error(t, valid(t, remove, `{}`), "the path parameter is required")
	attach := compileSchema(t, tools[3].Parameters)
	require.NoError(t, valid(t, attach, `{"id":"019a0000-0000-7000-8000-000000000001","content":"x","contentType":"text/plain","contentEncoding":"text"}`))
	require.Error(t, valid(t, attach, `{"id":"019a0000-0000-7000-8000-000000000001","content":"x","contentType":"image/png"}`), "a binary type needs its encoding")
	require.Error(t, valid(t, attach, `{"id":"019a0000-0000-7000-8000-000000000001","content":"x","contentType":"image/gif","contentEncoding":"base64"}`))
}

func TestTheCatalogueHandsOutCopies(t *testing.T) {
	t.Parallel()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	tool, ok := fixture.Catalogue.Lookup("getNotes")
	require.True(t, ok)
	tool.Parameters[0] = 'X'
	again, _ := fixture.Catalogue.Lookup("getNotes")
	require.Equal(t, byte('{'), again.Parameters[0])
	_, ok = fixture.Catalogue.Lookup("getNote")
	require.False(t, ok)
	require.Same(t, fixture.Engine.Registry(), fixture.Catalogue.Registry())
}

// shapes has a nullable reference, a nullable list, a member named like a keyword and a
// guarded field, the OpenAPI forms the conversion changes.
type shapes struct {
	ID       string   `json:"id"`
	Nullable *string  `json:"nullable"`
	Parent   *shapes  `json:"parent"`
	Tags     []string `json:"tags"`
	Hidden   string   `json:"hidden"`
}

func TestTheSchemasAreJSONSchemaNotOpenAPI(t *testing.T) {
	t.Parallel()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{})
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewObject("shape", "/shape", &livewire.ObjectOptions[shapes, livewire.NoParameters]{
		Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "hidden", Read: authorization.IsAdmin()}},
		Load: func(_ context.Context, _ *livewire.NoParameters) (*shapes, error) { return &shapes{}, nil },
	})))
	catalogue, err := generate.NewCatalogue(registry, &generate.CatalogueOptions{})
	require.NoError(t, err)
	tool, _ := catalogue.Lookup("getShape")
	result := compileSchema(t, tool.Result)
	require.NoError(t, valid(t, result, `{"id":"a","nullable":null,"parent":null,"tags":null}`))
	require.NoError(t, valid(t, result, `{"id":"a","nullable":"x","parent":{"id":"b","nullable":null,"parent":null,"tags":["t"]},"tags":[],"hidden":"h"}`))
	require.Error(t, valid(t, result, `{"id":"a","nullable":1,"parent":null,"tags":null}`))
	require.Error(t, valid(t, result, `{"id":"a","nullable":null,"parent":null,"tags":[1]}`))
	require.NotContains(t, string(tool.Result), "guarded")
}

// search is the item of a versioned resource with a history.
type search struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

func TestTheCatalogueRefusesWhatAModelCannotCall(t *testing.T) {
	t.Parallel()
	mutation := func(name string) livewire.Declaration {
		return livewire.NewMutation(name, http.MethodPost, "/x/{body}", &livewire.MutationOptions[struct {
			Body string `path:"body"`
		}, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Handle: func(context.Context, *struct {
				Body string `path:"body"`
			}, *livewire.NoBody,
			) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		})
	}
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{})
	require.NoError(t, err)
	require.NoError(t, registry.Add(mutation("shadowed")))
	_, err = generate.NewCatalogue(registry, nil)
	require.ErrorContains(t, err, `the parameter "body" is the tool argument`)

	long, err := livewire.NewRegistry(&livewire.RegistryOptions{})
	require.NoError(t, err)
	require.NoError(t, long.Add(livewire.NewObject(strings.Repeat("a", 62), "/a", &livewire.ObjectOptions[shapes, livewire.NoParameters]{
		Read: authorization.IsUser(), Load: func(context.Context, *livewire.NoParameters) (*shapes, error) { return &shapes{}, nil },
	})))
	_, err = generate.NewCatalogue(long, nil)
	require.ErrorContains(t, err, "longer than 64 bytes")

	_, err = generate.NewCatalogue(nil, nil)
	require.Error(t, err)
	_, err = generate.NewCatalogue(long, &generate.CatalogueOptions{Enums: []generate.Enum{{}}})
	require.Error(t, err)
}

// TestTheCatalogueRefusesAReadParameterNamedLikeAReservedOne: REST keeps a history's item key
// (path) and its reserved parameters (query) apart, a tool's arguments are one object.
func TestTheCatalogueRefusesAReadParameterNamedLikeAReservedOne(t *testing.T) {
	t.Parallel()
	for _, key := range []string{livewire.ParameterFilterID, livewire.ParameterFilter, livewire.ParameterSort, livewire.ParameterLimit, livewire.ParameterOffset} {
		registry, err := livewire.NewRegistry(&livewire.RegistryOptions{})
		require.NoError(t, err)
		require.NoError(t, registry.Add(livewire.NewResource("searches", "/searches", &livewire.ResourceOptions[search]{
			Read: authorization.IsUser(), Versioned: true,
			Fields: query.Fields{{Name: "id", Type: query.TypeUUID, SQL: "searches.id", Filter: true, Sort: true}},
			Item:   &livewire.ItemKey{Parameter: key, Field: "id"},
			List: func(context.Context, *query.ListQuery) (*livewire.Page[search], error) {
				return &livewire.Page[search]{}, nil
			},
			Get: func(context.Context, *query.ListQuery) (*search, error) { return &search{}, nil },
			History: func(context.Context, *query.ListQuery) (*livewire.Page[livewire.Version[search]], error) {
				return &livewire.Page[livewire.Version[search]]{}, nil
			},
		})), key)
		_, err = generate.NewCatalogue(registry, nil)
		require.ErrorContains(t, err, `getSearchesHistory: the parameter "`+key+`" is also a reserved parameter`, "the history's item key and its reserved parameter share one argument")
	}
}

func TestRenderWritesTheCatalogueFile(t *testing.T) {
	t.Parallel()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	files, err := generate.Render(fixture.Engine.Registry(), &generate.Options{CatalogueFile: "tools.json"})
	require.NoError(t, err)
	require.Len(t, files, 1)
	var written struct {
		Comment string          `json:"$comment"`
		Tools   []generate.Tool `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(files[0].Content, &written))
	require.Equal(t, "Code generated by livewire. DO NOT EDIT.", written.Comment)
	require.Len(t, written.Tools, len(fixture.Catalogue.Tools()))
	for position, tool := range fixture.Catalogue.Tools() {
		require.Equal(t, tool.Name, written.Tools[position].Name)
		require.JSONEq(t, string(tool.Parameters), string(written.Tools[position].Parameters))
	}
}

func TestTheOpenAPIOperationsCarryTheDeclaredDescriptions(t *testing.T) {
	t.Parallel()
	fixture, err := toolfixture.New()
	require.NoError(t, err)
	files, err := generate.Render(fixture.Engine.Registry(), &generate.Options{OpenAPIFile: "openapi.yaml", OpenAPI: generate.OpenAPIOptions{Title: "Notes", Version: "1"}})
	require.NoError(t, err)
	_, document, _ := bytes.Cut(files[0].Content, []byte("\n"))
	var parsed struct {
		Paths map[string]map[string]struct {
			Description string `json:"description"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(document, &parsed))
	require.Equal(t, "The notes.", parsed.Paths["/api/notes"]["get"].Description)
	require.Equal(t, "Creates a note.", parsed.Paths["/api/notes"]["post"].Description)
	require.Empty(t, parsed.Paths["/api/notes/{id}"]["delete"].Description)
	require.Equal(t, "At most 64 bytes (D17).", parsed.Paths["/api/notes/{id}/attachments"]["post"].Description)
}
