// Package repository persists the example's data (DAT-001): sqlc queries for rows and
// livewire's postgresadapter for the filterable reads, the history and the export. Every
// method runs on the engine's transaction when ctx carries one (D45, D53) and on the pool
// otherwise, so a mutation's writes, its job and its audit row commit together.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/govengers/jobs"
	jobspostgres "github.com/govengers/jobs/postgresadapter"
	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"

	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/repository/internal/sqlc"
)

// todoSQL is what the filterable reads of todos read from (D11).
var todoSQL = &query.ResourceSQL{From: "todos", IDColumn: "todos.id"}

// TodoFields are the fields of the todos resource: the API names and the SQL that reads them.
func TodoFields() query.Fields {
	return query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "todos.id", Filter: true, Sort: true},
		{Name: "title", Type: query.TypeText, SQL: "todos.title", Filter: true, Sort: true, Search: true},
		{Name: "priority", Type: query.TypeEnum, SQL: "todos.priority", Enum: []string{string(model.PriorityLow), string(model.PriorityNormal), string(model.PriorityHigh)}, Filter: true, Sort: true},
		{Name: "done", Type: query.TypeBoolean, SQL: "todos.done", Filter: true, Sort: true},
		{Name: "dueAt", Type: query.TypeTime, SQL: "todos.due_at", Filter: true, Sort: true, Nullable: true},
		{Name: "createdAt", Type: query.TypeTime, SQL: "todos.created_at", Filter: true, Sort: true},
		{Name: "updatedAt", Type: query.TypeTime, SQL: "todos.updated_at", Sort: true},
		{Name: "version", Type: query.TypeInteger, SQL: "todos.version"},
	}
}

// WorkspaceScope narrows the todos to the workspace of the request, livewire's tenant (D42),
// which the engine resolved before it calls the scope (DAT-017).
func WorkspaceScope(ctx context.Context) (query.Predicate, error) {
	workspace := livewire.TenantFrom(ctx)
	if workspace == "" {
		return nil, errNoWorkspace
	}
	return func(builder *query.Builder) {
		builder.WriteString("todos.workspace = ").Argument(workspace)
	}, nil
}

// errNoWorkspace is a read outside a workspace, which the registry's tenant never lets through.
var errNoWorkspace = errors.New("repository: a todo read without a workspace")

// TodoRepository stores todos, their attachments and the workspace activity.
type TodoRepository struct {
	pool     *pgxpool.Pool
	jobStore *jobspostgres.Store
}

// TodoRepositoryOptions are the dependencies of a TodoRepository.
type TodoRepositoryOptions struct {
	Pool     *pgxpool.Pool       // required
	JobStore *jobspostgres.Store // required; enqueues on the engine's transaction (D41)
}

// NewTodoRepository fails without the pool or the job store.
func NewTodoRepository(options *TodoRepositoryOptions) (*TodoRepository, error) {
	if options == nil || options.Pool == nil || options.JobStore == nil {
		return nil, errors.New("repository: TodoRepositoryOptions.Pool and JobStore are required")
	}
	return &TodoRepository{pool: options.Pool, jobStore: options.JobStore}, nil
}

// database is the engine's transaction in ctx, or the pool outside an engine unit of work.
func (r *TodoRepository) database(ctx context.Context) postgresadapter.DBTX {
	if transaction := postgresadapter.TransactionFrom(ctx); transaction != nil {
		return transaction
	}
	return r.pool
}

// List reads one page of todos under the query's scope, filter, sort and page.
func (r *TodoRepository) List(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[model.Todo], error) {
	page, err := postgresadapter.ListPage(ctx, r.pool, todoSQL, listQuery, hydrateTodos)
	if err != nil {
		return nil, fmt.Errorf("list todos: %w", err)
	}
	return page, nil
}

// Get reads the one todo the query names, or model.ErrNotFound.
func (r *TodoRepository) Get(ctx context.Context, listQuery *query.ListQuery) (*model.Todo, error) {
	page, err := r.List(ctx, listQuery)
	if err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, model.ErrNotFound
	}
	return &page.Items[0], nil
}

// Distinct counts the values of one field under the query.
func (r *TodoRepository) Distinct(ctx context.Context, field string, listQuery *query.ListQuery) ([]livewire.DistinctValue, error) {
	declared, found := TodoFields().Lookup(field)
	if !found {
		return nil, fmt.Errorf("distinct todos: the field %q is not declared", field)
	}
	values, err := postgresadapter.Distinct(ctx, r.pool, todoSQL, listQuery, declared)
	if err != nil {
		return nil, fmt.Errorf("distinct todos: %w", err)
	}
	return values, nil
}

// History reads one page of a todo's versions (D34, D56).
func (r *TodoRepository) History(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[model.Todo]], error) {
	page, err := postgresadapter.HistoryPage(ctx, r.pool, "todos", listQuery, decodeTodoRow)
	if err != nil {
		return nil, fmt.Errorf("todo history: %w", err)
	}
	return page, nil
}

// Export writes every todo under the query as CSV (D35).
func (r *TodoRepository) Export(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error) {
	rows, err := postgresadapter.WriteCSV(ctx, r.pool, &postgresadapter.Export{Specification: todoSQL, Query: listQuery, Columns: columns}, writer)
	if err != nil {
		return rows, fmt.Errorf("export todos: %w", err)
	}
	return rows, nil
}

// Create inserts a todo of the member's workspace.
func (r *TodoRepository) Create(ctx context.Context, member model.Member, todo model.NewTodo) (*model.Todo, error) {
	row, err := sqlc.New(r.database(ctx)).InsertTodo(ctx, sqlc.InsertTodoParams{
		ID: todo.ID, Workspace: member.Workspace, Title: todo.Title, Priority: string(todo.Priority), DueAt: todo.DueAt, CreatedBy: member.UserID,
	})
	if err != nil {
		return nil, fmt.Errorf("create todo %s: %w", todo.ID, mapError(err))
	}
	return toTodo(row), nil
}

// Lock reads a todo of the workspace and locks its row until the transaction ends (DAT-008).
func (r *TodoRepository) Lock(ctx context.Context, workspace string, id uuid.UUID) (*model.Todo, error) {
	row, err := sqlc.New(r.database(ctx)).LockTodo(ctx, sqlc.LockTodoParams{ID: id, Workspace: workspace})
	if err != nil {
		return nil, fmt.Errorf("lock todo %s: %w", id, mapError(err))
	}
	return toTodo(row), nil
}

// Save writes a locked todo's fields and counts its version up.
func (r *TodoRepository) Save(ctx context.Context, workspace string, todo model.Todo) (*model.Todo, error) {
	row, err := sqlc.New(r.database(ctx)).UpdateTodo(ctx, sqlc.UpdateTodoParams{
		ID: todo.ID, Workspace: workspace, Title: todo.Title, Priority: string(todo.Priority), Done: todo.Done, DueAt: todo.DueAt,
	})
	if err != nil {
		return nil, fmt.Errorf("save todo %s: %w", todo.ID, mapError(err))
	}
	return toTodo(row), nil
}

// Delete removes a todo of the workspace, or answers model.ErrNotFound.
func (r *TodoRepository) Delete(ctx context.Context, workspace string, id uuid.UUID) error {
	deleted, err := sqlc.New(r.database(ctx)).DeleteTodo(ctx, sqlc.DeleteTodoParams{ID: id, Workspace: workspace})
	switch {
	case err != nil:
		return fmt.Errorf("delete todo %s: %w", id, err)
	case deleted == 0:
		return model.ErrNotFound
	default:
		return nil
	}
}

// Enqueue enqueues jobs in the transaction of ctx, so they commit with the change (JOB-007).
func (r *TodoRepository) Enqueue(ctx context.Context, preparedJobs ...*jobs.PreparedJob) error {
	if _, err := r.jobStore.Enqueue(ctx, r.database(ctx), preparedJobs...); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
}

// Summary counts the workspace's open and done todos and its attachments.
func (r *TodoRepository) Summary(ctx context.Context, workspace string) (*model.Summary, error) {
	queries := sqlc.New(r.database(ctx))
	todos, err := queries.CountTodos(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("count todos: %w", err)
	}
	attachments, err := queries.CountAttachments(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("count attachments: %w", err)
	}
	return &model.Summary{Open: int(todos.Open), Done: int(todos.Done), Attachments: int(attachments)}, nil
}

// Attach stores a file with a todo of the workspace, or answers model.ErrNotFound.
func (r *TodoRepository) Attach(ctx context.Context, workspace string, attachment model.Attachment, content []byte) (*model.Attachment, error) {
	row, err := sqlc.New(r.database(ctx)).InsertAttachment(ctx, sqlc.InsertAttachmentParams{
		TodoID: attachment.TodoID, Workspace: workspace, Filename: attachment.Filename, ContentType: attachment.ContentType,
		SizeBytes: int64(len(content)), Content: content,
	})
	if err != nil {
		return nil, fmt.Errorf("attach to todo %s: %w", attachment.TodoID, mapError(err))
	}
	return &model.Attachment{ID: row.ID, TodoID: row.TodoID, Filename: row.Filename, ContentType: row.ContentType, Size: row.SizeBytes, CreatedAt: row.CreatedAt}, nil
}

// RecordActivity inserts an activity line once: a retried job writes nothing twice (JOB-003).
func (r *TodoRepository) RecordActivity(ctx context.Context, workspace string, activity model.Activity) error {
	err := sqlc.New(r.database(ctx)).InsertActivity(ctx, sqlc.InsertActivityParams{ID: activity.ID, Workspace: workspace, TodoID: activity.TodoID, Message: activity.Message})
	if err != nil {
		return fmt.Errorf("record activity %s: %w", activity.ID, err)
	}
	return nil
}

// ListActivity reads the workspace's latest activity, of one todo when todoID is set.
func (r *TodoRepository) ListActivity(ctx context.Context, workspace string, todoID *uuid.UUID) ([]model.Activity, error) {
	rows, err := sqlc.New(r.database(ctx)).ListActivity(ctx, sqlc.ListActivityParams{Workspace: workspace, TodoID: todoID})
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	activities := make([]model.Activity, 0, len(rows))
	for _, row := range rows {
		activities = append(activities, model.Activity{ID: row.ID, TodoID: row.TodoID, Message: row.Message, CreatedAt: row.CreatedAt})
	}
	return activities, nil
}

// hydrateTodos reads a page's todos by id (postgresadapter.Hydrate).
func hydrateTodos(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]model.Todo, error) {
	found, err := sqlc.New(database).ListTodosByIDs(ctx, postgresadapter.PageIDs(rows))
	if err != nil {
		return nil, err
	}
	todos := make(map[uuid.UUID]model.Todo, len(found))
	for _, row := range found {
		todos[row.ID] = *toTodo(row)
	}
	return todos, nil
}

// todoRow is a todos row as todos_history stores it, as jsonb of its columns (D34).
type todoRow struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	Priority  string     `json:"priority"`
	Done      bool       `json:"done"`
	DueAt     *time.Time `json:"due_at"`
	CreatedBy uuid.UUID  `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Version   int32      `json:"version"`
}

func decodeTodoRow(data []byte) (*model.Todo, error) {
	var row todoRow
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, fmt.Errorf("decode a todo version: %w", err)
	}
	return toTodo(sqlc.Todo{
		ID: row.ID, Title: row.Title, Priority: row.Priority, Done: row.Done, DueAt: row.DueAt,
		CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Version: row.Version,
	}), nil
}

func toTodo(row sqlc.Todo) *model.Todo {
	return &model.Todo{
		ID: row.ID, Title: row.Title, Priority: model.Priority(row.Priority), Done: row.Done, DueAt: row.DueAt,
		CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Version: int(row.Version),
	}
}

// mapError maps the driver's errors to the domain's once (DAT-004).
func mapError(err error) error {
	var postgresError *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return model.ErrNotFound
	case errors.As(err, &postgresError) && postgresError.Code == uniqueViolation:
		return model.ErrConflict
	default:
		return err
	}
}

// uniqueViolation is the SQLSTATE of a duplicate key.
const uniqueViolation = "23505"
