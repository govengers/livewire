package livewire_test

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

type todo struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"createdAt"`
}

type createTodo struct {
	ID    *uuid.UUID `json:"id,omitempty"` // client-generated (ClientID: UUIDv7)
	Title string     `json:"title"`
}

func ExampleRegistry_Add() {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
	if err != nil {
		fmt.Println(err)
		return
	}
	err = registry.Add(
		livewire.NewResource("todos", "/todos", &livewire.ResourceOptions[todo]{
			Tables: []string{"todos"},
			Read:   authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeUUID, SQL: "todos.id", Filter: true, Sort: true},
				{Name: "title", Type: query.TypeText, SQL: "todos.title", Filter: true, Sort: true, Search: true},
				{Name: "done", Type: query.TypeBoolean, SQL: "todos.done", Filter: true, Sort: true},
				{Name: "createdAt", Type: query.TypeTime, SQL: "todos.created_at", Filter: true, Sort: true},
			},
			DefaultSort: []query.SortTerm{{Field: "createdAt", Descending: true}},
			Item:        &livewire.ItemKey{Parameter: "id", Field: "id"},
			List: func(context.Context, *query.ListQuery) (*livewire.Page[todo], error) {
				return &livewire.Page[todo]{}, nil // the repository runs postgresadapter.ListPage
			},
			Get: func(context.Context, *query.ListQuery) (*todo, error) {
				return nil, livewire.NotFound()
			},
		}),
		livewire.NewMutation("createTodo", http.MethodPost, "/todos", &livewire.MutationOptions[livewire.NoParameters, createTodo, todo]{
			Check:    authorization.HasPermission("todos", auth.AccessWrite),
			Status:   http.StatusCreated,
			ClientID: livewire.UUIDv7,
			Resource: "todos",
			Effect:   livewire.Upserts("todos"),
			Handle: func(_ context.Context, _ *livewire.NoParameters, body *createTodo) (*todo, error) {
				return &todo{ID: uuid.New(), Title: body.Title}, nil
			},
		}),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, declaration := range registry.Declarations() {
		fmt.Println(declaration.Kind(), declaration.Name(), declaration.Method(), registry.BasePath()+declaration.Path())
	}
	todos, _ := registry.Lookup("todos")
	fmt.Println(todos.(*livewire.Resource).ItemPath())
	// Output:
	// resource todos GET /api/v1/todos
	// mutation createTodo POST /api/v1/todos
	// /todos/{id}
}

// ExampleNewEngine wires the engine in main: the registry, the IdentitySource that
// re-validates every socket (session.Guard), the Transactor and the change feed of
// postgresadapter, the browser origins that may open a socket. Handler serves the socket
// behind the same authentication as the REST routes; Run consumes the change feed until the
// context ends; Close ends every socket with 1001.
func ExampleNewEngine() {
	var (
		ctx            context.Context
		registry       *livewire.Registry      // built with NewRegistry and Add
		identitySource livewire.IdentitySource // session.Guard
		transactor     livewire.Transactor     // postgresadapter
		changeFeed     livewire.ChangeFeed     // postgresadapter's LISTEN feed
		authenticate   func(http.Handler) http.Handler
	)
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry:       registry,
		ChangeFeed:     changeFeed,
		IdentitySource: identitySource,
		Transactor:     transactor,
		AllowedOrigins: []string{"https://app.example"},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	http.Handle("/api/v1/live", authenticate(engine.Handler()))
	go func() { _ = engine.Run(ctx) }()
	defer func() { _ = engine.Close(ctx) }()
}
