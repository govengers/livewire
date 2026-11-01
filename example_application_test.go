package livewire_test

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/govengers/auth/authorization"
	authginadapter "github.com/govengers/auth/ginadapter"
	"github.com/govengers/auth/session"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

// The README's example, compiled with the module: the docs quote the regions between the
// snippet markers, and TestDocumentSnippetsMatchTheirSources fails when a quote drifts.

// todoRepository reads and writes the todos of the README's example on PostgreSQL.
type todoRepository struct {
	pool *pgxpool.Pool
}

// todoSQL is where the todos resource reads its rows from (D11).
var todoSQL = &query.ResourceSQL{From: "todos", IDColumn: "todos.id"}

// List is the resource's loader: the page query, the capped count and the hydration on one
// snapshot, the engine's read transaction (D53, D55).
func (r *todoRepository) List(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[todo], error) {
	return postgresadapter.ListPage(ctx, r.pool, todoSQL, listQuery, r.hydrate)
}

// Create runs in the mutation's transaction, which the engine owns (D31): it commits with the
// audit row and the idempotency record, or not at all.
func (r *todoRepository) Create(ctx context.Context, _ *livewire.NoParameters, body *createTodo) (*todo, error) {
	database := postgresadapter.TransactionFrom(ctx)
	if database == nil {
		database = r.pool // outside an engine unit of work
	}
	created := &todo{Title: body.Title}
	err := database.QueryRow(ctx, "INSERT INTO todos (id, title) VALUES (coalesce($1, uuidv7()), $2) RETURNING id, done, created_at",
		body.ID, body.Title).Scan(&created.ID, &created.Done, &created.CreatedAt)
	return created, err
}

func (r *todoRepository) hydrate(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]todo, error) {
	result, err := database.Query(ctx, "SELECT id, title, done, created_at FROM todos WHERE id = ANY($1)", postgresadapter.PageIDs(rows))
	if err != nil {
		return nil, err
	}
	defer result.Close()
	found, err := pgx.CollectRows(result, pgx.RowToStructByPos[todo])
	if err != nil {
		return nil, err
	}
	todos := make(map[uuid.UUID]todo, len(found))
	for _, item := range found {
		todos[item.ID] = item
	}
	return todos, nil
}

// snippet:begin declare
func declare(todos *todoRepository) (*livewire.Registry, error) {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api/v1"})
	if err != nil {
		return nil, err
	}
	return registry, registry.Add( // a declaration mistake fails here, at startup (D43)
		livewire.NewResource("todos", "/todos", &livewire.ResourceOptions[todo]{
			Tables: []string{"todos"}, // a committed write to todos re-runs every live read of it
			Read:   authorization.IsUser(),
			Fields: query.Fields{
				{Name: "title", Type: query.TypeText, SQL: "todos.title", Filter: true, Sort: true, Search: true},
				{Name: "done", Type: query.TypeBoolean, SQL: "todos.done", Filter: true, Sort: true},
				{Name: "createdAt", Type: query.TypeTime, SQL: "todos.created_at", Filter: true, Sort: true},
			},
			DefaultSort: []query.SortTerm{{Field: "createdAt", Descending: true}},
			List:        todos.List,
		}),
		livewire.NewMutation("createTodo", http.MethodPost, "/todos", &livewire.MutationOptions[livewire.NoParameters, createTodo, todo]{
			Check: authorization.IsUser(), Status: http.StatusCreated,
			ClientID: livewire.UUIDv7, Effect: livewire.Upserts("todos"), // the client shows it at once
			Handle: todos.Create,
		}),
	)
}

// snippet:end declare

// snippet:begin serve
func serve(ctx context.Context, pool *pgxpool.Pool, databaseURL string, guard *session.Guard, router gin.IRouter) error {
	registry, err := declare(&todoRepository{pool: pool})
	if err != nil {
		return err
	}
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
	if err != nil {
		return err
	}
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: databaseURL})
	if err != nil {
		return err
	}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, ChangeFeed: listener, Transactor: transactor,
		IdentitySource: guard, // re-validates the session of every socket (D6)
		AllowedOrigins: []string{"https://todo.example.com"},
	})
	if err != nil {
		return err
	}
	api := router.Group("", authginadapter.Middleware(guard.RequireAPI())) // the identity of every request
	if err := ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: engine}); err != nil {
		return err
	}
	api.GET("/api/v1/socket", ginadapter.SocketHandler(engine))
	return engine.Run(ctx) // the change feed and the ticks; on shutdown engine.Close(ctx) ends every socket
}

// snippet:end serve

// Example_application declares the README's API and renders its TypeScript client and
// OpenAPI document; serve wires it on gin and PostgreSQL.
func Example_application() {
	registry, err := declare(&todoRepository{})
	if err != nil {
		fmt.Println(err)
		return
	}
	_ = serve // main calls it with its pool, session guard and router
	// snippet:begin generate
	options := &generate.Options{
		TypeScriptDirectory: "web/src/api", // types.ts, filters.ts, api.ts, runtime.ts, vue.ts
		OpenAPIFile:         "openapi.yaml",
		OpenAPI:             generate.OpenAPIOptions{Title: "Todo", Version: "1.0.0"},
	}
	// snippet:end generate
	files, err := generate.Render(registry, options) // what generate.Write(registry, options) writes
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, file := range files {
		fmt.Println(file.Path)
	}
	// Unordered output:
	// web/src/api/types.ts
	// web/src/api/filters.ts
	// web/src/api/api.ts
	// web/src/api/runtime.ts
	// web/src/api/vue.ts
	// openapi.yaml
}
