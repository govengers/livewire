// Package service holds the example's business logic (ARC-001), transport-agnostic: the
// controllers translate livewire's declarations and the jobs into these calls.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	wideevent "github.com/govengers/wide-event-logger"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"

	"github.com/govengers/livewire/examples/todo/internal/actor"
	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/repository"
)

// TodoService runs the todos of a workspace. Every method acts in the workspace of the
// member in ctx; a write runs in the engine's transaction ctx carries (D31).
type TodoService struct {
	todos *repository.TodoRepository
}

// TodoServiceOptions are the dependencies of a TodoService.
type TodoServiceOptions struct {
	TodoRepository *repository.TodoRepository // required
}

// NewTodoService fails without the TodoRepository.
func NewTodoService(options *TodoServiceOptions) (*TodoService, error) {
	if options == nil || options.TodoRepository == nil {
		return nil, errors.New("service: TodoServiceOptions.TodoRepository is required")
	}
	return &TodoService{todos: options.TodoRepository}, nil
}

// Fields are the fields a todo read filters, sorts and searches by.
func (s *TodoService) Fields() query.Fields { return repository.TodoFields() }

// Scope narrows every todo read to the workspace in ctx.
func (s *TodoService) Scope(ctx context.Context) (query.Predicate, error) {
	return repository.WorkspaceScope(ctx)
}

// List reads one page of todos; livewire put the workspace scope into the query.
func (s *TodoService) List(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[model.Todo], error) {
	return s.todos.List(ctx, listQuery)
}

// Get answers the todo the query names; the engine scoped the query to the workspace.
func (s *TodoService) Get(ctx context.Context, listQuery *query.ListQuery) (*model.Todo, error) {
	return s.todos.Get(ctx, listQuery)
}

// Distinct counts a field's values under the scoped query.
func (s *TodoService) Distinct(ctx context.Context, field string, listQuery *query.ListQuery) ([]livewire.DistinctValue, error) {
	return s.todos.Distinct(ctx, field, listQuery)
}

// History answers the versions of a todo the engine let the caller read first (D56).
func (s *TodoService) History(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[model.Todo]], error) {
	return s.todos.History(ctx, listQuery)
}

// Export writes the scoped todos as CSV; a formula is quoted (D69).
func (s *TodoService) Export(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error) {
	return s.todos.Export(ctx, listQuery, columns, writer)
}

// Summary counts the workspace's todos and attachments.
func (s *TodoService) Summary(ctx context.Context) (*model.Summary, error) {
	member, err := actor.Member(ctx)
	if err != nil {
		return nil, err
	}
	return s.todos.Summary(ctx, member.Workspace)
}

// Activity reads the workspace's latest activity, of one todo when todoID is set.
func (s *TodoService) Activity(ctx context.Context, todoID *uuid.UUID) ([]model.Activity, error) {
	member, err := actor.Member(ctx)
	if err != nil {
		return nil, err
	}
	return s.todos.ListActivity(ctx, member.Workspace, todoID)
}

// Create creates a todo: with the client's id when it sent one (D18), normal priority by
// default.
func (s *TodoService) Create(ctx context.Context, todo model.NewTodo) (*model.Todo, error) {
	member, err := actor.Member(ctx)
	if err != nil {
		return nil, err
	}
	if todo.ID == uuid.Nil {
		if todo.ID, err = uuid.NewV7(); err != nil {
			return nil, fmt.Errorf("a todo id: %w", err)
		}
	}
	if todo.Priority == "" {
		todo.Priority = model.PriorityNormal
	}
	wideevent.FromContext(ctx).UUID("todo.id", todo.ID)
	return s.todos.Create(ctx, member, todo)
}

// Update changes a todo under a row lock and skips a change that changes nothing (INT-003).
// Completing an open todo enqueues the completion job in the same transaction (D41).
func (s *TodoService) Update(ctx context.Context, id uuid.UUID, change model.TodoChange) (*model.Todo, error) {
	member, err := actor.Member(ctx)
	if err != nil {
		return nil, err
	}
	wideevent.FromContext(ctx).UUID("todo.id", id)
	current, err := s.todos.Lock(ctx, member.Workspace, id)
	if err != nil {
		return nil, err
	}
	changed := change.Apply(*current)
	if changed.Equal(*current) {
		return current, nil
	}
	saved, err := s.todos.Save(ctx, member.Workspace, changed)
	if err != nil {
		return nil, err
	}
	// snippet:begin completion-job
	if change.Completes(*current) {
		completed := model.TodoCompletedKind.New(model.TodoCompletedPayload{
			TodoID: saved.ID, Workspace: member.Workspace, Title: saved.Title, Username: member.Username,
		}, nil)
		if err := s.todos.Enqueue(ctx, completed); err != nil {
			return nil, err
		}
		wideevent.FromContext(ctx).Bool("todo.completed", true)
	}
	// snippet:end completion-job
	return saved, nil
}

// Delete deletes a todo and its attachments.
func (s *TodoService) Delete(ctx context.Context, id uuid.UUID) error {
	member, err := actor.Member(ctx)
	if err != nil {
		return err
	}
	wideevent.FromContext(ctx).UUID("todo.id", id)
	return s.todos.Delete(ctx, member.Workspace, id)
}

// Attach stores a file with a todo of the workspace.
func (s *TodoService) Attach(ctx context.Context, attachment model.Attachment, content []byte) (*model.Attachment, error) {
	member, err := actor.Member(ctx)
	if err != nil {
		return nil, err
	}
	wideevent.FromContext(ctx).UUID("todo.id", attachment.TodoID).Int("attachment.size", len(content))
	return s.todos.Attach(ctx, member.Workspace, attachment, content)
}

// RecordCompletion writes the activity line of a completed todo; the job's id keys it, so a
// retried job writes it once (JOB-003).
func (s *TodoService) RecordCompletion(ctx context.Context, jobID uuid.UUID, completed model.TodoCompletedPayload) error {
	wideevent.FromContext(ctx).UUID("job.id", jobID).UUID("todo.id", completed.TodoID)
	return s.todos.RecordActivity(ctx, completed.Workspace, model.Activity{
		ID: jobID, TodoID: completed.TodoID, Message: fmt.Sprintf("%s completed %q", completed.Username, completed.Title),
	})
}
