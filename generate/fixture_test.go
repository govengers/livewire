package generate_test

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// errExportCut is the failure of an export the backend breaks off after its first row.
var errExportCut = errors.New("the database went away")

// fixtureBackend is the application behind the fixture registry: the parts in memory and a
// hook that reports every write to the change feed. Its zero value serves nothing, which is
// all the generator's own tests need.
type fixtureBackend struct {
	mutex     sync.Mutex
	parts     map[uuid.UUID]fixture.Part
	cutExport bool
	changed   func(table string)
}

func newFixtureBackend() *fixtureBackend {
	return &fixtureBackend{parts: map[uuid.UUID]fixture.Part{}}
}

func (b *fixtureBackend) put(part fixture.Part) {
	b.mutex.Lock()
	b.parts[part.ID] = part
	changed := b.changed
	b.mutex.Unlock()
	if changed != nil {
		changed("parts")
	}
}

func (b *fixtureBackend) sorted() []fixture.Part {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	items := make([]fixture.Part, 0, len(b.parts))
	for _, item := range b.parts {
		items = append(items, item)
	}
	slices.SortFunc(items, func(left, right fixture.Part) int { return strings.Compare(left.ID.String(), right.ID.String()) })
	return items
}

// keyOf is the part a loader's query names by its item key, found among the query's arguments.
func (b *fixtureBackend) keyOf(listQuery *query.ListQuery, table string) (fixture.Part, bool) {
	_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: table, IDColumn: table + ".id"})
	b.mutex.Lock()
	defer b.mutex.Unlock()
	for _, argument := range arguments {
		if id, err := uuid.Parse(fmt.Sprint(argument)); err == nil {
			if part, ok := b.parts[id]; ok {
				return part, true
			}
		}
	}
	return fixture.Part{}, false
}

func (b *fixtureBackend) listParts(_ context.Context, listQuery *query.ListQuery) (*livewire.Page[fixture.Part], error) {
	items := b.sorted()
	if listQuery.Cursor != nil {
		position := slices.IndexFunc(items, func(item fixture.Part) bool { return item.ID.String() == listQuery.Cursor.ID })
		items = items[position+1:]
	}
	page := &livewire.Page[fixture.Part]{Items: items[:min(listQuery.Limit, len(items))], Total: len(b.sorted())}
	if len(items) > listQuery.Limit {
		last := page.Items[len(page.Items)-1]
		page.Next = listQuery.NextCursor(orderValues(listQuery, last), last.ID.String())
	}
	return page, nil
}

// orderValues are a part's values of the query's order terms, as the page query selects them.
func orderValues(listQuery *query.ListQuery, part fixture.Part) []any {
	values := make([]any, 0, len(listQuery.Order))
	for _, term := range listQuery.Order {
		values = append(values, map[string]any{
			"id": part.ID.String(), "name": part.Name, "kind": string(part.Kind), "createdAt": part.CreatedAt, "version": int64(part.Version),
		}[term.Field])
	}
	return values
}

func (b *fixtureBackend) getPart(_ context.Context, listQuery *query.ListQuery) (*fixture.Part, error) {
	if part, ok := b.keyOf(listQuery, "parts"); ok {
		return &part, nil
	}
	return nil, livewire.NotFound()
}

func (b *fixtureBackend) distinctParts(context.Context, string, *query.ListQuery) ([]livewire.DistinctValue, error) {
	counts := map[fixture.Kind]int{}
	for _, part := range b.sorted() {
		counts[part.Kind]++
	}
	values := []livewire.DistinctValue{}
	for _, kind := range []fixture.Kind{fixture.KindBolt, fixture.KindNut} {
		if counts[kind] > 0 {
			values = append(values, livewire.DistinctValue{Value: string(kind), Count: counts[kind]})
		}
	}
	return values, nil
}

func (b *fixtureBackend) partHistory(_ context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[fixture.Part]], error) {
	part, ok := b.keyOf(listQuery, "parts_history")
	if !ok {
		return &livewire.Page[livewire.Version[fixture.Part]]{}, nil
	}
	versions := []livewire.Version[fixture.Part]{{ID: 1, Operation: livewire.ChangeInsert, ValidFrom: time.Unix(1, 0).UTC(), Data: &part}}
	return &livewire.Page[livewire.Version[fixture.Part]]{Items: versions, Total: 1}, nil
}

// exportParts writes the id and the name of every part; with cutExport it breaks off after
// the first row.
func (b *fixtureBackend) exportParts(_ context.Context, _ *query.ListQuery, _ query.Fields, writer io.Writer) (int, error) {
	output := csv.NewWriter(writer)
	if err := output.Write([]string{"id", "name"}); err != nil {
		return 0, err
	}
	rows := 0
	for _, part := range b.sorted() {
		if err := output.Write([]string{part.ID.String(), part.Name}); err != nil {
			return rows, err
		}
		rows++
		output.Flush()
		if b.cutExport {
			return rows, errExportCut
		}
	}
	output.Flush()
	return rows, output.Error()
}

func (b *fixtureBackend) partsResource() *livewire.Resource {
	return livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[fixture.Part]{
		Tables: []string{"parts"}, Read: authorization.IsUser(), Fields: partFields(),
		DefaultSort: []query.SortTerm{{Field: "createdAt", Descending: true}},
		Item:        &livewire.ItemKey{Parameter: "id", Field: "id"}, Versioned: true, Cursor: true,
		List: b.listParts, Get: b.getPart, Distinct: b.distinctParts, History: b.partHistory, Export: b.exportParts,
	})
}

func parts() *livewire.Resource {
	return newFixtureBackend().partsResource()
}

func (b *fixtureBackend) createPart(_ context.Context, _ *livewire.NoParameters, body *fixture.CreatePart) (*fixture.Part, error) {
	id := body.ID
	if id == uuid.Nil {
		id = uuid.Must(uuid.NewV7())
	}
	part := fixture.Part{ID: id, Name: body.Name, Kind: body.Kind}
	part.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	b.put(part)
	return &part, nil
}

func (b *fixtureBackend) renamePart(_ context.Context, parameters *partParameters, body *fixture.RenamePart) (*fixture.Part, error) {
	b.mutex.Lock()
	part, ok := b.parts[parameters.ID]
	b.mutex.Unlock()
	if !ok {
		return nil, livewire.NotFound()
	}
	part.Name = body.Name
	b.put(part)
	return &part, nil
}

func (b *fixtureBackend) deletePart(_ context.Context, parameters *partParameters, _ *livewire.NoBody) (*livewire.NoContent, error) {
	b.mutex.Lock()
	delete(b.parts, parameters.ID)
	changed := b.changed
	b.mutex.Unlock()
	if changed != nil {
		changed("parts")
	}
	return &livewire.NoContent{}, nil
}

func (b *fixtureBackend) uploadAttachment(_ context.Context, parameters *partParameters, file *livewire.File) (*fixture.Attachment, error) {
	read, err := io.ReadAll(file.Body)
	if err != nil {
		return nil, err
	}
	return &fixture.Attachment{ID: parameters.ID, Size: int64(len(read)), Type: file.ContentType}, nil
}

func loadStats(_ context.Context, parameters *statParameters) ([]fixture.Stat, error) {
	stats := []fixture.Stat{{ID: "1", Name: parameters.Board + ":" + parameters.State + ":" + strings.Join(parameters.Tags, "+"), Hits: 3}}
	if parameters.Limit != nil {
		stats = append(stats, fixture.Stat{ID: "2", Name: fmt.Sprintf("limit %d hidden %t owner %s kind %s", *parameters.Limit, parameters.Hidden, parameters.Owner, parameters.Kind)})
	}
	return stats, nil
}

// newRegistry declares every kind: a live resource with every read and an export, a resource
// that is not live, typed lists and an object, mutations with and without body, content,
// parameters and effects, a mutation over DTOs whose names collide, and an upload.
func newRegistry(t *testing.T) *livewire.Registry {
	t.Helper()
	return newFixtureBackend().registry(t)
}

func (b *fixtureBackend) registry(t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		b.partsResource(),
		livewire.NewResource("notes", "/notes", &livewire.ResourceOptions[fixture.Note]{
			Read: authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeInteger, SQL: "notes.id", Filter: true, Sort: true},
				{Name: "text", Type: query.TypeString, SQL: "notes.text"},
			},
			List: func(context.Context, *query.ListQuery) (*livewire.Page[fixture.Note], error) {
				return &livewire.Page[fixture.Note]{Items: []fixture.Note{{ID: 1, Text: "note"}}, Total: 1}, nil
			},
		}),
		livewire.NewList("stats", "/boards/{board}/stats", &livewire.ListOptions[fixture.Stat, statParameters]{
			Tables: []string{"stats"}, Read: authorization.IsUser(),
			Guarded: []livewire.GuardedField{{Name: "hits", Read: authorization.IsAdmin()}},
			Load:    loadStats,
		}),
		livewire.NewList("labels", "/labels", &livewire.ListOptions[string, livewire.NoParameters]{
			Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]string, error) { return []string{"red", "green"}, nil },
		}),
		livewire.NewObject("dashboard", "/boards/{board}/dashboard", &livewire.ObjectOptions[fixture.Dashboard, dashboardParameters]{
			Tables: []string{"boards"}, Read: authorization.IsUser(),
			Load: func(_ context.Context, parameters *dashboardParameters) (*fixture.Dashboard, error) {
				return &fixture.Dashboard{Title: parameters.Board}, nil
			},
		}),
		livewire.NewMutation("createPart", http.MethodPost, "/parts", &livewire.MutationOptions[livewire.NoParameters, fixture.CreatePart, fixture.Part]{
			Check: authorization.IsUser(), Status: http.StatusCreated, ClientID: livewire.UUIDv7, Effect: livewire.Upserts("parts"),
			Handle: b.createPart,
		}),
		livewire.NewMutation("renamePart", http.MethodPatch, "/parts/{id}", &livewire.MutationOptions[partParameters, fixture.RenamePart, fixture.Part]{
			Check: authorization.IsUser(), Resource: "parts", Effect: livewire.Upserts("parts"),
			Handle: b.renamePart,
		}),
		livewire.NewMutation("deletePart", http.MethodDelete, "/parts/{id}", &livewire.MutationOptions[partParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Effect: livewire.Removes("parts"),
			Handle: b.deletePart,
		}),
		livewire.NewMutation("chargePart", http.MethodPost, "/billing/charges", &livewire.MutationOptions[chargeParameters, billing.Record, billing.Part]{
			Check: authorization.IsUser(),
			Handle: func(_ context.Context, parameters *chargeParameters, body *billing.Record) (*billing.Part, error) {
				return &billing.Part{Invoice: parameters.Invoice + ":" + body.Body.Reason}, nil
			},
		}),
		livewire.NewMutation("rebuild", http.MethodPost, "/rebuild", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, fixture.Page[fixture.Node]]{
			Check: authorization.IsAdmin(),
			Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*fixture.Page[fixture.Node], error) {
				return &fixture.Page[fixture.Node]{Count: 1}, nil
			},
		}),
		livewire.NewUpload("uploadAttachment", "/parts/{id}/attachments", &livewire.UploadOptions[partParameters, fixture.Attachment]{
			Check: authorization.IsUser(), Status: http.StatusCreated, ContentTypes: []string{"image/png", "image/jpeg"}, MaximumBytes: 1 << 20,
			Handle: b.uploadAttachment,
		}),
	))
	return registry
}

// fixtureClientImportPath is the import path of the fixture's committed Go client, whose
// internal DTO packages it may import.
const fixtureClientImportPath = "github.com/govengers/livewire/generate/internal/fixture/fixtureclient"

func fixtureOptions(directory string) *generate.Options {
	return &generate.Options{
		TypeScriptDirectory: directory + "/typescript",
		OpenAPIFile:         directory + "/openapi.yaml",
		GoClient:            generate.GoClientOptions{Directory: directory + "/fixtureclient", ImportPath: fixtureClientImportPath},
		CommandLine:         generate.CommandLineOptions{Directory: directory + "/fixturecommand", Name: "fixture"},
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
