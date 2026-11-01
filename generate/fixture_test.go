package generate_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/generate/internal/fixture/billing"
	"github.com/govengers/livewire/query"
)

// statParameters cover every parameter type: a path parameter, an enum, a list, a pointer,
// a bool, a time, a uuid and a listed named string type.
type statParameters struct {
	Board  string       `path:"board"`
	State  string       `query:"state" enum:"open,done"`
	Tags   []string     `query:"tag"`
	Limit  *int         `query:"limit"`
	Hidden bool         `query:"hidden"`
	Since  *time.Time   `query:"since"`
	Owner  uuid.UUID    `query:"owner"`
	Kind   fixture.Kind `query:"kind"`
}

type dashboardParameters struct {
	Board string `path:"board"`
}

type partParameters struct {
	ID uuid.UUID `path:"id"`
}

type chargeParameters struct {
	Invoice string `query:"invoice"`
}

func partFields() query.Fields {
	return query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeText, SQL: "parts.name", Filter: true, Sort: true, Search: true},
		{Name: "kind", Type: query.TypeEnum, SQL: "parts.kind", Enum: []string{"bolt", "nut"}, Filter: true, Sort: true},
		{Name: "price", Type: query.TypeFloat, SQL: "parts.price", Filter: true, Sort: true},
		{Name: "secret", Type: query.TypeString, SQL: "parts.secret", Filter: true, Read: authorization.IsAdmin()},
		{Name: "createdAt", Type: query.TypeTime, SQL: "parts.created_at", Filter: true, Sort: true},
		{Name: "tags", Type: query.TypeStringArray, SQL: "parts.tags", Filter: true},
		{Name: "version", Type: query.TypeInteger, SQL: "parts.version", Filter: true, Sort: true},
		{Name: "flag", Type: query.TypeBoolean, SQL: "parts.flag", Filter: true},
		{Name: "extra", Type: query.TypeJSON, SQL: "parts.extra"},
	}
}

func parts() *livewire.Resource {
	return livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[fixture.Part]{
		Tables: []string{"parts"}, Read: authorization.IsUser(), Fields: partFields(),
		DefaultSort: []query.SortTerm{{Field: "createdAt", Descending: true}},
		Item:        &livewire.ItemKey{Parameter: "id", Field: "id"}, Versioned: true, Cursor: true,
		List: func(context.Context, *query.ListQuery) (*livewire.Page[fixture.Part], error) {
			return &livewire.Page[fixture.Part]{}, nil
		},
		Get: func(context.Context, *query.ListQuery) (*fixture.Part, error) { return &fixture.Part{}, nil },
		Distinct: func(context.Context, string, *query.ListQuery) ([]livewire.DistinctValue, error) {
			return nil, nil
		},
		History: func(context.Context, *query.ListQuery) (*livewire.Page[livewire.Version[fixture.Part]], error) {
			return &livewire.Page[livewire.Version[fixture.Part]]{}, nil
		},
		Export: func(context.Context, *query.ListQuery, query.Fields, io.Writer) (int, error) { return 0, nil },
	})
}

// newRegistry declares every kind: a live resource with every read and an export, a resource
// that is not live, typed lists and an object, mutations with and without body, content,
// parameters and effects, a mutation over DTOs whose names collide, and an upload.
func newRegistry(t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		parts(),
		livewire.NewResource("notes", "/notes", &livewire.ResourceOptions[fixture.Note]{
			Read: authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeInteger, SQL: "notes.id", Filter: true, Sort: true},
				{Name: "text", Type: query.TypeString, SQL: "notes.text"},
			},
			List: func(context.Context, *query.ListQuery) (*livewire.Page[fixture.Note], error) {
				return &livewire.Page[fixture.Note]{}, nil
			},
		}),
		livewire.NewList("stats", "/boards/{board}/stats", &livewire.ListOptions[fixture.Stat, statParameters]{
			Tables: []string{"stats"}, Read: authorization.IsUser(),
			Guarded: []livewire.GuardedField{{Name: "hits", Read: authorization.IsAdmin()}},
			Load:    func(context.Context, *statParameters) ([]fixture.Stat, error) { return nil, nil },
		}),
		livewire.NewList("labels", "/labels", &livewire.ListOptions[string, livewire.NoParameters]{
			Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]string, error) { return nil, nil },
		}),
		livewire.NewObject("dashboard", "/boards/{board}/dashboard", &livewire.ObjectOptions[fixture.Dashboard, dashboardParameters]{
			Tables: []string{"boards"}, Read: authorization.IsUser(),
			Load: func(context.Context, *dashboardParameters) (*fixture.Dashboard, error) {
				return &fixture.Dashboard{}, nil
			},
		}),
		livewire.NewMutation("createPart", http.MethodPost, "/parts", &livewire.MutationOptions[livewire.NoParameters, fixture.CreatePart, fixture.Part]{
			Check: authorization.IsUser(), Status: http.StatusCreated, ClientID: livewire.UUIDv7, Effect: livewire.Upserts("parts"),
			Handle: func(context.Context, *livewire.NoParameters, *fixture.CreatePart) (*fixture.Part, error) {
				return &fixture.Part{}, nil
			},
		}),
		livewire.NewMutation("renamePart", http.MethodPatch, "/parts/{id}", &livewire.MutationOptions[partParameters, fixture.RenamePart, fixture.Part]{
			Check: authorization.IsUser(), Resource: "parts", Effect: livewire.Upserts("parts"),
			Handle: func(context.Context, *partParameters, *fixture.RenamePart) (*fixture.Part, error) {
				return &fixture.Part{}, nil
			},
		}),
		livewire.NewMutation("deletePart", http.MethodDelete, "/parts/{id}", &livewire.MutationOptions[partParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Effect: livewire.Removes("parts"),
			Handle: func(context.Context, *partParameters, *livewire.NoBody) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		}),
		livewire.NewMutation("chargePart", http.MethodPost, "/billing/charges", &livewire.MutationOptions[chargeParameters, billing.Record, billing.Part]{
			Check: authorization.IsUser(),
			Handle: func(context.Context, *chargeParameters, *billing.Record) (*billing.Part, error) {
				return &billing.Part{}, nil
			},
		}),
		livewire.NewMutation("rebuild", http.MethodPost, "/rebuild", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, fixture.Page[fixture.Node]]{
			Check: authorization.IsAdmin(),
			Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*fixture.Page[fixture.Node], error) {
				return &fixture.Page[fixture.Node]{}, nil
			},
		}),
		livewire.NewUpload("uploadAttachment", "/parts/{id}/attachments", &livewire.UploadOptions[partParameters, fixture.Attachment]{
			Check: authorization.IsUser(), Status: http.StatusCreated, ContentTypes: []string{"image/png", "image/jpeg"}, MaximumBytes: 1 << 20,
			Handle: func(context.Context, *partParameters, *livewire.File) (*fixture.Attachment, error) {
				return &fixture.Attachment{}, nil
			},
		}),
	))
	return registry
}

func fixtureOptions(directory string) *generate.Options {
	return &generate.Options{
		TypeScriptDirectory: directory + "/typescript",
		OpenAPIFile:         directory + "/openapi.yaml",
		Enums:               []generate.Enum{generate.EnumOf(fixture.KindBolt, fixture.KindNut)},
		Presence:            true,
		OpenAPI: generate.OpenAPIOptions{
			Title: "Fixture", Version: "1.0.0", Description: "Every declaration kind.",
			Servers: []generate.Server{{URL: "https://fixture.example.com", Description: "production"}},
			SecuritySchemes: []generate.SecurityScheme{
				{Name: "session", Type: "apiKey", In: "cookie", ParameterName: "session"},
				{Name: "apiKey", Type: "http", Scheme: "bearer", BearerFormat: "API key"},
			},
		},
	}
}
