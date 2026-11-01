package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/govengers/jobs/jobstest"
	"github.com/govengers/livewire"
	"github.com/govengers/livewire/livewiretest"

	"github.com/govengers/livewire/examples/todo/client/todoclient"
	"github.com/govengers/livewire/examples/todo/internal/api"
	"github.com/govengers/livewire/examples/todo/internal/generated"
	"github.com/govengers/livewire/examples/todo/internal/server"
)

// The contract gates of D23 on the application as main wires it.

func TestGeneratedClientsAreCurrent(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	livewiretest.GeneratedIsCurrent(t, application.registry, generated.Options("../.."))
}

// fixtureTodo creates a todo through the API and answers it.
func fixtureTodo(t *testing.T, todos *todoclient.Client, title string) *api.TodoItem {
	t.Helper()
	created, err := todos.CreateTodo(t.Context(), api.CreateTodoRequest{Title: title}, nil)
	require.NoError(t, err)
	return created
}

// reads are the parameters of the reads that need some, for a fixture todo.
func reads(todo *api.TodoItem) []livewiretest.Read {
	id := map[string]string{"id": todo.ID.String()}
	return []livewiretest.Read{
		{Target: "todos.item", Parameters: id},
		{Target: "todos.history", Parameters: id},
		{Target: "todos.distinct", Parameters: map[string]string{livewire.ParameterField: "priority"}},
		{Target: "todos.presence", Parameters: map[string]string{livewire.ParameterPresenceKey: todo.ID.String()}},
		{Target: "summary.presence", Parameters: map[string]string{livewire.ParameterPresenceKey: "board"}},
	}
}

func (a *testApplication) gateApplication() livewiretest.Application {
	return livewiretest.Application{Engine: a.engine, Handler: a.router, SocketPath: server.SocketPath}
}

func TestRESTAndSocketAnswerTheSameBytes(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	todo := fixtureTodo(t, application.client(t, "alice"), "parity")
	livewiretest.Parity(t, &livewiretest.ParityOptions{
		Application:  application.gateApplication(),
		Authenticate: application.session(t, "alice"), // the browser's way; the gates below use an API key
		Reads:        reads(todo),
	})
}

func TestEveryWriteReachesTheSubscriptionsOfItsTables(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	key := application.apiKey(t, "alice")
	todos := application.client(t, "alice")
	todo := fixtureTodo(t, todos, "stale")
	ctx := context.Background()
	livewiretest.Stale(t, &livewiretest.StaleOptions{
		Application:  application.gateApplication(),
		Authenticate: bearer(key),
		Reads:        reads(todo)[:3], // the presence reads are the library's own table
		Writes: []livewiretest.Write{
			{Table: "todos", Commit: func(ctx context.Context) error {
				_, err := todos.CreateTodo(ctx, api.CreateTodoRequest{Title: "another"}, nil)
				return err
			}},
			{Table: "attachments", Commit: func(ctx context.Context) error {
				_, err := todos.UploadAttachment(ctx, todoclient.UploadAttachmentParameters{ID: todo.ID}, textFile("notes"), nil)
				return err
			}},
			{Table: "activity", Commit: func(context.Context) error {
				// the completion enqueues the job in the mutation's transaction; the job writes the activity
				if _, err := todos.UpdateTodo(ctx, todoclient.UpdateTodoParameters{ID: todo.ID}, api.UpdateTodoRequest{Done: new(true)}, nil); err != nil {
					return err
				}
				jobstest.Drain(t, application.worker)
				return nil
			}},
		},
	})
}

func TestWorkspacesNeverSeeEachOthersTodos(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	fixtureTodo(t, application.client(t, "alice"), "north")
	fixtureTodo(t, application.client(t, "bob"), "south")
	livewiretest.TenantIsolation(t, &livewiretest.TenantIsolationOptions{
		Application: application.gateApplication(),
		Tenants: [2]livewiretest.Tenant{
			{Name: "north", Authenticate: bearer(application.apiKey(t, "alice"))},
			{Name: "south", Authenticate: bearer(application.apiKey(t, "bob"))},
		},
	})
}

// textFile is a text attachment.
func textFile(content string) livewire.File {
	return livewire.File{ContentType: "text/plain", Size: int64(len(content)), Body: strings.NewReader(content), Filename: uuid.NewString() + ".txt"}
}
