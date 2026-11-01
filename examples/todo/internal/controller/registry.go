// Package controller is the example's transport layer (ARC-001): the livewire registry, whose
// declarations translate REST, the socket and the generated clients into service calls, and
// the job controller, which does the same for govengers/jobs.
package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"

	"github.com/govengers/livewire/examples/todo/internal/actor"
	"github.com/govengers/livewire/examples/todo/internal/api"
	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/service"
)

// BasePath is the prefix of every route of the API.
const BasePath = "/api/v1"

// maximumAttachmentBytes bounds one upload.
const maximumAttachmentBytes = 1 << 20

// RegistryOptions configures the registry. TodoService and AccountService are required;
// Observer receives every outcome of the engine and the REST adapter (D49).
type RegistryOptions struct {
	TodoService    *service.TodoService
	AccountService *service.AccountService
	Observer       livewire.Observer
}

// NewRegistry declares the API (section 3): the todos (a filterable, versioned resource with
// history, distinct and export), the activity (a typed list the completion job fills), the
// summary (an object), the mutations, the upload and the API keys.
func NewRegistry(options *RegistryOptions) (*livewire.Registry, error) {
	if options == nil || options.TodoService == nil || options.AccountService == nil {
		return nil, errors.New("controller: RegistryOptions.TodoService and AccountService are required")
	}
	return newRegistry(&todoController{todos: options.TodoService}, &accountController{accounts: options.AccountService}, options.Observer)
}

// Contract is the API's registry without services, for the generator (cmd/generate): it
// describes every declaration, and none of its handlers or loaders may run.
func Contract() (*livewire.Registry, error) {
	return newRegistry(&todoController{}, &accountController{}, nil)
}

func newRegistry(todos *todoController, accounts *accountController, observer livewire.Observer) (*livewire.Registry, error) {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: BasePath, Tenant: actor.Workspace, Observer: observer})
	if err != nil {
		return nil, err
	}
	return registry, registry.Add(
		todos.resource(),
		livewire.NewList("activity", "/activity", &livewire.ListOptions[api.ActivityItem, api.ActivityParameters]{
			Tables: []string{"activity"}, Read: authorization.IsUser(), Load: todos.activity,
		}),
		livewire.NewObject("summary", "/summary", &livewire.ObjectOptions[api.SummaryItem, livewire.NoParameters]{
			Tables: []string{"todos", "attachments"}, Read: authorization.IsUser(), Load: todos.summary,
		}),
		livewire.NewMutation("createTodo", http.MethodPost, "/todos", &livewire.MutationOptions[livewire.NoParameters, api.CreateTodoRequest, api.TodoItem]{
			Check: authorization.IsUser(), Status: http.StatusCreated, ClientID: livewire.UUIDv7, Effect: livewire.Upserts("todos"),
			Handle: todos.create,
		}),
		livewire.NewMutation("updateTodo", http.MethodPatch, "/todos/{id}", &livewire.MutationOptions[api.TodoParameters, api.UpdateTodoRequest, api.TodoItem]{
			Check: authorization.IsUser(), Resource: "todos", Effect: livewire.Upserts("todos"),
			Handle: todos.update,
		}),
		livewire.NewMutation("deleteTodo", http.MethodDelete, "/todos/{id}", &livewire.MutationOptions[api.TodoParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Effect: livewire.Removes("todos"),
			Handle: todos.delete,
		}),
		livewire.NewUpload("uploadAttachment", "/todos/{id}/attachments", &livewire.UploadOptions[api.TodoParameters, api.AttachmentItem]{
			Check: authorization.IsUser(), Status: http.StatusCreated,
			ContentTypes: []string{"text/plain", "image/png", "application/pdf"}, MaximumBytes: maximumAttachmentBytes,
			Handle: todos.attach,
		}),
		livewire.NewMutation("createKey", http.MethodPost, "/keys", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, api.KeyItem]{
			Check: authorization.IsUser(), Status: http.StatusCreated,
			Handle: accounts.createKey,
		}),
	)
}

// todoController translates the todo declarations into TodoService calls.
type todoController struct {
	todos *service.TodoService
}

func (c *todoController) resource() *livewire.Resource {
	return livewire.NewResource("todos", "/todos", &livewire.ResourceOptions[api.TodoItem]{
		Tables:       []string{"todos"},
		Read:         authorization.IsUser(),
		Fields:       c.todos.Fields(),
		DefaultSort:  []query.SortTerm{{Field: "createdAt", Descending: true}},
		Scope:        c.todos.Scope,
		MaximumLimit: 100,
		CountCap:     10000,
		Item:         &livewire.ItemKey{Parameter: "id", Field: "id"},
		Versioned:    true,
		List: func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[api.TodoItem], error) {
			page, err := c.todos.List(ctx, listQuery)
			if err != nil {
				return nil, responseError(err)
			}
			return &livewire.Page[api.TodoItem]{Items: mapItems(page.Items, toTodoItem), Total: page.Total, Next: page.Next}, nil
		},
		Get: func(ctx context.Context, listQuery *query.ListQuery) (*api.TodoItem, error) {
			todo, err := c.todos.Get(ctx, listQuery)
			if err != nil {
				return nil, responseError(err)
			}
			return new(toTodoItem(*todo)), nil
		},
		Distinct: c.todos.Distinct,
		History:  c.history,
		Export:   c.todos.Export,
	})
}

func (c *todoController) history(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[api.TodoItem]], error) {
	page, err := c.todos.History(ctx, listQuery)
	if err != nil {
		return nil, responseError(err)
	}
	return &livewire.Page[livewire.Version[api.TodoItem]]{Items: mapItems(page.Items, toTodoVersion), Total: page.Total}, nil
}

func (c *todoController) activity(ctx context.Context, parameters *api.ActivityParameters) ([]api.ActivityItem, error) {
	activities, err := c.todos.Activity(ctx, parameters.TodoID)
	if err != nil {
		return nil, responseError(err)
	}
	return mapItems(activities, func(activity model.Activity) api.ActivityItem {
		return api.ActivityItem{ID: activity.ID, TodoID: activity.TodoID, Message: activity.Message, CreatedAt: activity.CreatedAt}
	}), nil
}

func (c *todoController) summary(ctx context.Context, _ *livewire.NoParameters) (*api.SummaryItem, error) {
	summary, err := c.todos.Summary(ctx)
	if err != nil {
		return nil, responseError(err)
	}
	return &api.SummaryItem{Open: summary.Open, Done: summary.Done, Attachments: summary.Attachments}, nil
}

func (c *todoController) create(ctx context.Context, _ *livewire.NoParameters, body *api.CreateTodoRequest) (*api.TodoItem, error) {
	todo, err := c.todos.Create(ctx, model.NewTodo{ID: body.ID, Title: body.Title, Priority: model.Priority(body.Priority), DueAt: body.DueAt})
	if err != nil {
		return nil, responseError(err)
	}
	return new(toTodoItem(*todo)), nil
}

func (c *todoController) update(ctx context.Context, parameters *api.TodoParameters, body *api.UpdateTodoRequest) (*api.TodoItem, error) {
	change := model.TodoChange{Title: body.Title, Done: body.Done, DueAt: body.DueAt, ClearDueAt: body.ClearDueAt}
	if body.Priority != nil {
		change.Priority = new(model.Priority(*body.Priority))
	}
	todo, err := c.todos.Update(ctx, parameters.ID, change)
	if err != nil {
		return nil, responseError(err)
	}
	return new(toTodoItem(*todo)), nil
}

func (c *todoController) delete(ctx context.Context, parameters *api.TodoParameters, _ *livewire.NoBody) (*livewire.NoContent, error) {
	if err := c.todos.Delete(ctx, parameters.ID); err != nil {
		return nil, responseError(err)
	}
	return &livewire.NoContent{}, nil
}

func (c *todoController) attach(ctx context.Context, parameters *api.TodoParameters, file *livewire.File) (*api.AttachmentItem, error) {
	content, err := io.ReadAll(io.LimitReader(file.Body, maximumAttachmentBytes))
	if err != nil {
		return nil, fmt.Errorf("read the attachment: %w", err)
	}
	attachment, err := c.todos.Attach(ctx, model.Attachment{TodoID: parameters.ID, Filename: file.Filename, ContentType: file.ContentType}, content)
	if err != nil {
		return nil, responseError(err)
	}
	return &api.AttachmentItem{
		ID: attachment.ID, TodoID: attachment.TodoID, Filename: attachment.Filename, ContentType: attachment.ContentType,
		Size: attachment.Size, CreatedAt: attachment.CreatedAt,
	}, nil
}

// accountController translates the key declaration into AccountService calls.
type accountController struct {
	accounts *service.AccountService
}

func (c *accountController) createKey(ctx context.Context, _ *livewire.NoParameters, _ *livewire.NoBody) (*api.KeyItem, error) {
	created, err := c.accounts.CreateAPIKey(ctx)
	if err != nil {
		return nil, responseError(err)
	}
	return &api.KeyItem{ID: created.ID, Key: created.Key, CreatedAt: created.CreatedAt}, nil
}

// responseError maps the domain's errors to livewire's refusals in one place (API-002); any
// other error stays as it is, an *auth.Error answers its kind, the rest a 500 without text.
func responseError(err error) error {
	switch {
	case errors.Is(err, model.ErrNotFound):
		return livewire.NotFound()
	case errors.Is(err, model.ErrConflict):
		return livewire.Conflict()
	default:
		return err
	}
}

func toTodoItem(todo model.Todo) api.TodoItem {
	return api.TodoItem{
		ID: todo.ID, Title: todo.Title, Priority: api.Priority(todo.Priority), Done: todo.Done, DueAt: todo.DueAt,
		CreatedAt: todo.CreatedAt, UpdatedAt: todo.UpdatedAt, Version: todo.Version,
	}
}

func toTodoVersion(version livewire.Version[model.Todo]) livewire.Version[api.TodoItem] {
	mapped := livewire.Version[api.TodoItem]{
		ID: version.ID, Operation: version.Operation, ValidFrom: version.ValidFrom, ValidTo: version.ValidTo,
		Actor: version.Actor, RealActor: version.RealActor, Mutation: version.Mutation, RequestID: version.RequestID,
	}
	if version.Data != nil {
		mapped.Data = new(toTodoItem(*version.Data))
	}
	return mapped
}

func mapItems[From, To any](items []From, mapping func(From) To) []To {
	mapped := make([]To, 0, len(items))
	for _, item := range items {
		mapped = append(mapped, mapping(item))
	}
	return mapped
}
