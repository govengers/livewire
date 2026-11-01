package ginadapter_test

import (
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventgin "github.com/govengers/wide-event-logger/ginadapter"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/query"
)

// The API the routes serve: a versioned, cursor-paged resource of parts with every read and
// an export, a typed list, an object with presence, mutations and an upload.

const (
	roleReader = "reader"
	roleWriter = "writer"
	firstID    = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b41"
	secondID   = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b42"
)

type part struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Secret  string `json:"secret"`
	Version int    `json:"version"`
}

type createPart struct {
	Name string `json:"name" validate:"required"`
}

type partParameters struct {
	ID string `path:"id"`
}

type statParameters struct {
	Group string `query:"group"`
}

type stat struct {
	ID    string `json:"id"`
	Group string `json:"group"`
}

type boardParameters struct {
	Team string `path:"team"`
}

type board struct {
	Team string `json:"team"`
}

type picture struct {
	Part        string `json:"part"`
	Bytes       int    `json:"bytes"`
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
}

// api is the application behind the routes: its parts and what its handlers saw.
type api struct {
	mutex   sync.Mutex
	parts   map[string]part
	created atomic.Int64
}

func (a *api) sorted() []part {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	items := make([]part, 0, len(a.parts))
	for _, item := range a.parts {
		items = append(items, item)
	}
	slices.SortFunc(items, func(left, right part) int { return strings.Compare(left.ID, right.ID) })
	return items
}

func (a *api) put(item part) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.parts[item.ID] = item
}

// itemID is the item key a loader's query carries as its argument.
func itemID(listQuery *query.ListQuery) string {
	_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	for _, argument := range arguments {
		if id, ok := argument.(string); ok && (id == firstID || id == secondID) {
			return id
		}
	}
	return ""
}

func (a *api) registry(t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api"})
	require.NoError(t, err)
	require.NoError(t, registry.Add(a.partsResource(), a.statsList(), a.boardObject(), a.createPart(), a.removePart(), a.failPart(), a.uploadPicture()))
	return registry
}

func partFields() query.Fields {
	return query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeText, SQL: "parts.name", Filter: true, Sort: true, Search: true},
		{Name: "secret", Type: query.TypeString, SQL: "parts.secret", Read: authorization.IsAdmin()},
	}
}

func (a *api) partsResource() *livewire.Resource {
	return livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[part]{
		Tables: []string{"parts"}, Read: authorization.HasRole(roleReader), Fields: partFields(),
		Item: &livewire.ItemKey{Parameter: "id", Field: "id"}, Versioned: true, Cursor: true,
		List: func(_ context.Context, listQuery *query.ListQuery) (*livewire.Page[part], error) {
			items := a.sorted()
			if listQuery.Cursor != nil {
				index := slices.IndexFunc(items, func(item part) bool { return item.ID == listQuery.Cursor.ID })
				items = items[index+1:]
			}
			page := &livewire.Page[part]{Items: items[:min(listQuery.Limit, len(items))], Total: len(a.sorted())}
			if len(items) > listQuery.Limit {
				page.Next = listQuery.NextCursor(nil, page.Items[len(page.Items)-1].ID)
			}
			return page, nil
		},
		Get: func(_ context.Context, listQuery *query.ListQuery) (*part, error) {
			a.mutex.Lock()
			defer a.mutex.Unlock()
			if item, ok := a.parts[itemID(listQuery)]; ok {
				return &item, nil
			}
			return nil, livewire.NotFound()
		},
		Distinct: func(context.Context, string, *query.ListQuery) ([]livewire.DistinctValue, error) {
			return []livewire.DistinctValue{{Value: "bolt", Count: 1}}, nil
		},
		History: func(_ context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[part]], error) {
			_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts_history", IDColumn: "version"})
			a.mutex.Lock()
			defer a.mutex.Unlock()
			item := a.parts[arguments[0].(string)]
			versions := []livewire.Version[part]{{ID: 1, Operation: livewire.ChangeInsert, ValidFrom: time.Unix(1, 0).UTC(), Data: &item}}
			return &livewire.Page[livewire.Version[part]]{Items: versions, Total: 1}, nil
		},
		Export: a.exportParts,
	})
}

// exportParts writes the visible columns of every part; a filter on the name "fail" breaks
// after the first row.
func (a *api) exportParts(_ context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error) {
	output := csv.NewWriter(writer)
	header := make([]string, len(columns))
	for index, column := range columns {
		header[index] = column.Name
	}
	if err := output.Write(header); err != nil {
		return 0, err
	}
	_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	failing := slices.Contains(arguments, any("fail"))
	rows := 0
	for _, item := range a.sorted() {
		values := map[string]string{"id": item.ID, "name": item.Name, "secret": item.Secret}
		record := make([]string, len(columns))
		for index, column := range columns {
			record[index] = values[column.Name]
		}
		if err := output.Write(record); err != nil {
			return rows, err
		}
		rows++
		output.Flush()
		if failing {
			return rows, errDatabaseGone
		}
	}
	output.Flush()
	return rows, output.Error()
}

func (a *api) statsList() *livewire.List {
	return livewire.NewList("stats", "/stats", &livewire.ListOptions[stat, statParameters]{
		Read: authorization.HasRole(roleReader),
		Load: func(_ context.Context, parameters *statParameters) ([]stat, error) {
			return []stat{{ID: "s1", Group: parameters.Group}}, nil
		},
	})
}

func (a *api) boardObject() *livewire.Object {
	return livewire.NewObject("board", "/teams/{team}/board", &livewire.ObjectOptions[board, boardParameters]{
		Read: authorization.HasRole(roleReader),
		Load: func(_ context.Context, parameters *boardParameters) (*board, error) {
			return &board{Team: parameters.Team}, nil
		},
	})
}

func (a *api) createPart() *livewire.Mutation {
	return livewire.NewMutation("createPart", http.MethodPost, "/parts", &livewire.MutationOptions[livewire.NoParameters, createPart, part]{
		Check: authorization.HasRole(roleWriter), Status: http.StatusCreated,
		Handle: func(ctx context.Context, _ *livewire.NoParameters, body *createPart) (*part, error) {
			a.created.Add(1)
			item := part{ID: secondID, Name: body.Name}
			transactionOf(ctx).hooks(func() { a.put(item) }, func() {})
			return &item, nil
		},
	})
}

func (a *api) removePart() *livewire.Mutation {
	return livewire.NewMutation("removePart", http.MethodDelete, "/parts/{id}", &livewire.MutationOptions[partParameters, livewire.NoBody, livewire.NoContent]{
		Check: authorization.HasRole(roleWriter),
		Handle: func(ctx context.Context, parameters *partParameters, _ *livewire.NoBody) (*livewire.NoContent, error) {
			transactionOf(ctx).hooks(func() {
				a.mutex.Lock()
				delete(a.parts, parameters.ID)
				a.mutex.Unlock()
			}, func() {})
			return &livewire.NoContent{}, nil
		},
	})
}

func (a *api) failPart() *livewire.Mutation {
	return livewire.NewMutation("failPart", http.MethodPost, "/parts/fail", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
		Check: authorization.HasRole(roleWriter),
		Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
			return nil, livewire.Conflict()
		},
	})
}

func (a *api) uploadPicture() *livewire.Upload {
	return livewire.NewUpload("uploadPicture", "/parts/{id}/pictures", &livewire.UploadOptions[partParameters, picture]{
		Check: authorization.HasRole(roleWriter), ContentTypes: []string{"image/png"}, MaximumBytes: 16,
		Handle: func(_ context.Context, parameters *partParameters, file *livewire.File) (*picture, error) {
			content, err := io.ReadAll(file.Body)
			if err != nil {
				return nil, err
			}
			return &picture{Part: parameters.ID, Bytes: len(content), ContentType: file.ContentType, Filename: file.Filename}, nil
		},
	})
}

// testServer is the API on gin behind the wide-event middleware and an authentication that
// takes the identity of the X-Subject header.
type testServer struct {
	t      *testing.T
	api    *api
	people *identities
	engine *livewire.Engine
	router *gin.Engine
	mutex  sync.Mutex
	events []map[string]any
}

func newServer(t *testing.T) *testServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := &testServer{t: t, api: &api{parts: map[string]part{firstID: {ID: firstID, Name: "bolt", Secret: "s1", Version: 1}}}}
	people := &identities{people: map[string]*auth.Identity{
		"ada":  {Provider: "test", Subject: "ada", Active: true, Roles: []string{roleReader, roleWriter}},
		"rita": {Provider: "test", Subject: "rita", Active: true, Roles: []string{roleReader}},
		"root": {Provider: "test", Subject: "root", Active: true, Roles: []string{roleReader, roleWriter, auth.RoleAdmin}},
	}}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: server.api.registry(t), IdentitySource: people, Transactor: &transactor{},
		FilterStore: &filters{stored: map[string]livewire.StoredFilter{}}, IdempotencyStore: &idempotency{records: map[string]*livewire.IdempotencyRecord{}},
		PresenceStore: &presences{}, SameHostOnly: true,
	})
	require.NoError(t, err)
	server.engine, server.people = engine, people
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, engine.Close(ctx))
	})
	wideEvents, err := wideeventgin.NewMiddleware(&wideeventgin.Options{Emitter: wideevent.EmitterFunc(func(we *wideevent.Event) {
		fields := map[string]any{}
		for _, field := range we.Fields() {
			fields[field.Key] = field.Value
		}
		server.mutex.Lock()
		server.events = append(server.events, fields)
		server.mutex.Unlock()
	})})
	require.NoError(t, err)
	server.router = gin.New()
	server.router.Use(wideEvents, func(ginContext *gin.Context) {
		if identity := people.get(ginContext.GetHeader(subjectHeader)); identity != nil {
			ginContext.Request = ginContext.Request.WithContext(auth.WithIdentity(ginContext.Request.Context(), identity))
		}
	})
	require.NoError(t, ginadapter.RegisterRoutes(server.router, &ginadapter.Options{Engine: engine, MaximumBodyBytes: 1024}))
	server.router.GET("/socket", ginadapter.SocketHandler(engine))
	return server
}

// do serves one request as subject and returns the recorder.
func (s *testServer) do(method, path, subject string, body io.Reader, header http.Header) *httptest.ResponseRecorder {
	s.t.Helper()
	request := httptest.NewRequestWithContext(s.t.Context(), method, path, body)
	for name, values := range header {
		request.Header[name] = values
	}
	if subject != "" {
		request.Header.Set(subjectHeader, subject)
	}
	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, request)
	return recorder
}

func (s *testServer) lastEvent() map[string]any {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.events[len(s.events)-1]
}
