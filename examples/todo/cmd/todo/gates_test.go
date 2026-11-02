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
	"github.com/govengers/livewire/examples/todo/internal/controller"
	"github.com/govengers/livewire/examples/todo/internal/generated"
	"github.com/govengers/livewire/examples/todo/internal/server"
)

// The contract gates of D23 on the application as main wires it.

// snippet:begin generated-current

func TestGeneratedClientsAreCurrent(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	livewiretest.GeneratedIsCurrentStrict(t, application.registry, generated.Options("../.."))
	contract, err := controller.Contract()
	require.NoError(t, err)
	livewiretest.GeneratedIsCurrentStrict(t, contract, generated.Options("../..")) // what cmd/generate writes from
}

// snippet:end generated-current

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

// seedParity gives every read alice runs something to answer: todos of both priorities, a
// completed one with its activity line and version history, an attachment, and alice's
// presence on the fixture todo and the summary; it answers the fixture todo.
func seedParity(t *testing.T, application *testApplication) *api.TodoItem {
	t.Helper()
	alice := application.client(t, "alice")
	todo := fixtureTodo(t, alice, "parity")
	_, err := alice.CreateTodo(t.Context(), api.CreateTodoRequest{Title: "urgent", Priority: api.PriorityHigh}, nil)
	require.NoError(t, err)
	_, err = alice.UpdateTodo(t.Context(), todoclient.UpdateTodoParameters{ID: todo.ID}, api.UpdateTodoRequest{Done: new(true)}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, jobstest.Drain(t, application.worker))
	_, err = alice.UploadAttachment(t.Context(), todoclient.UploadAttachmentParameters{ID: todo.ID}, textFile("notes"), nil)
	require.NoError(t, err)
	socket := application.dial(t, bearer(application.apiKey(t, "alice")))
	for target, key := range map[string]string{"todos": todo.ID.String(), "summary": "board"} {
		joined, _ := socket.request(t, livewire.RequestFrame{
			Operation: livewire.OperationJoin, Target: target, Parameters: map[string]string{livewire.ParameterPresenceKey: key}, Body: []byte(`{"editing":true}`),
		})
		require.True(t, joined.OK, "%+v", joined.Error)
	}
	todo, err = alice.GetTodosItem(t.Context(), todoclient.TodosItemParameters{ID: todo.ID})
	require.NoError(t, err)
	return todo
}

func (a *testApplication) gateApplication() livewiretest.Application {
	return livewiretest.Application{Engine: a.engine, Handler: a.router, SocketPath: server.SocketPath}
}

// snippet:begin parity

func TestRESTAndSocketAnswerTheSameBytes(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	todo := seedParity(t, application)
	livewiretest.Parity(t, &livewiretest.ParityOptions{
		Application:  application.gateApplication(),
		Authenticate: application.session(t, "alice"), // the browser's way; the gates below use an API key
		Reads:        reads(todo),
	})
}

// snippet:end parity

// snippet:begin stale

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

// snippet:end stale

// snippet:begin tenant-isolation

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

// snippet:end tenant-isolation

// textFile is a text attachment.
func textFile(content string) livewire.File {
	return livewire.File{ContentType: "text/plain", Size: int64(len(content)), Body: strings.NewReader(content), Filename: uuid.NewString() + ".txt"}
}
