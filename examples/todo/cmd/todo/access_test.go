package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"

	"github.com/govengers/livewire/examples/todo/client/todoclient"
	"github.com/govengers/livewire/examples/todo/internal/api"
	"github.com/govengers/livewire/examples/todo/internal/authentication"
)

// Who may do what (TST-003, AUTH-007): the workspace is read from the current membership on
// every request and socket re-validation, other workspaces stay out of reach, and the refusals
// answer the one error body.

// restRead sends GET with the request's authentication and answers the status and the body.
func (a *testApplication) restRead(t *testing.T, path string, authenticate func(request *http.Request)) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL+path, nil)
	require.NoError(t, err)
	authenticate(request)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	var body bytes.Buffer
	_, err = body.ReadFrom(response.Body)
	require.NoError(t, err)
	return response.StatusCode, body.Bytes()
}

// nested is a field of a wide event's FieldsMap, whose dotted keys are nested maps.
func nested(fields map[string]any, path ...string) any {
	var value any = fields
	for _, key := range path {
		members, isMap := value.(map[string]any)
		if !isMap {
			return nil
		}
		value = members[key]
	}
	return value
}

// titles are the titles of a todo list body.
func titles(t *testing.T, body []byte) []string {
	t.Helper()
	var page livewire.ListResponse[api.TodoItem]
	require.NoError(t, json.Unmarshal(body, &page))
	listed := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		listed = append(listed, item.Title)
	}
	return listed
}

func TestASessionFollowsItsMembershipOnEveryRequest(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	fixtureTodo(t, application.client(t, "alice"), "north's")
	fixtureTodo(t, application.client(t, "bob"), "south's")
	alice := application.session(t, "alice")

	status, body := application.restRead(t, "/api/v1/todos", alice)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"north's"}, titles(t, body))
	socket := application.dial(t, alice)
	queried, _ := socket.request(t, livewire.RequestFrame{Operation: livewire.OperationQuery, Target: "todos"})
	require.Equal(t, []string{"north's"}, titles(t, queried.Payload))

	require.NoError(t, application.accounts.Join(t.Context(), authentication.SeededUserID("alice"), "south"))
	status, body = application.restRead(t, "/api/v1/todos", alice)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"south's"}, titles(t, body), "the session reads its new workspace at once")
	queried, _ = socket.request(t, livewire.RequestFrame{Operation: livewire.OperationQuery, Target: "todos"})
	require.Equal(t, []string{"south's"}, titles(t, queried.Payload), "so does its socket")

	require.NoError(t, application.accounts.Leave(t.Context(), authentication.SeededUserID("alice")))
	status, _ = application.restRead(t, "/api/v1/todos", alice)
	require.Equal(t, http.StatusForbidden, status, "a removed member is refused")
	_, closed := socket.request(t, livewire.RequestFrame{Operation: livewire.OperationQuery, Target: "todos"})
	require.Equal(t, livewire.CloseIdentityGone, closed, "and its socket ends with 4401")
}

func TestAnAPIKeyFollowsItsMembershipAndDiesWithIt(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	fixtureTodo(t, application.client(t, "bob"), "south's")
	key := bearer(application.apiKey(t, "alice"))
	require.NoError(t, application.accounts.Join(t.Context(), authentication.SeededUserID("alice"), "south"))
	status, body := application.restRead(t, "/api/v1/todos", key)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"south's"}, titles(t, body))
	require.NoError(t, application.accounts.Leave(t.Context(), authentication.SeededUserID("alice")))
	status, _ = application.restRead(t, "/api/v1/todos", key)
	require.Equal(t, http.StatusUnauthorized, status, "the membership's keys went with it")
}

func TestOtherWorkspacesAndStrangersAreRefused(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	alice, bob := application.client(t, "alice"), application.client(t, "bob")
	north := fixtureTodo(t, alice, "north's")

	_, err := bob.UpdateTodo(t.Context(), todoclient.UpdateTodoParameters{ID: north.ID}, api.UpdateTodoRequest{Done: new(true)}, nil)
	requireCode(t, err, livewire.CodeNotFound)
	requireCode(t, bob.DeleteTodo(t.Context(), todoclient.DeleteTodoParameters{ID: north.ID}, nil), livewire.CodeNotFound)
	_, err = bob.UploadAttachment(t.Context(), todoclient.UploadAttachmentParameters{ID: north.ID}, textFile("x"), nil)
	requireCode(t, err, livewire.CodeNotFound)
	unchanged, err := alice.GetTodosItem(t.Context(), todoclient.TodosItemParameters{ID: north.ID})
	require.NoError(t, err)
	require.Equal(t, *north, *unchanged)

	for name, authenticate := range map[string]func(*http.Request){
		"no session":        func(*http.Request) {},
		"an unknown key":    bearer("todo_unknown"),
		"a forged cookie":   func(request *http.Request) { request.Header.Set("Cookie", "auth_session=forged") },
		"an unknown header": func(request *http.Request) { request.Header.Set("X-API-Key", "todo_unknown") },
	} {
		status, body := application.restRead(t, "/api/v1/todos", authenticate)
		require.Equal(t, http.StatusUnauthorized, status, name)
		require.Contains(t, string(body), `"code"`, "%s: the one error body", name)
	}
	status, body := application.restRead(t, "/api/v1/unknown", application.session(t, "alice"))
	require.Equal(t, http.StatusNotFound, status)
	require.JSONEq(t, `{"code":"not_found","message":"not found"}`, string(body), "an unknown route answers the error body (API-001)")
}

func TestAFailedBatchEnqueuesNoJob(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	alice := application.client(t, "alice")
	todo := fixtureTodo(t, alice, "to complete")
	complete, err := todoclient.BatchUpdateTodo(todoclient.UpdateTodoParameters{ID: todo.ID}, api.UpdateTodoRequest{Done: new(true)})
	require.NoError(t, err)
	missing, err := todoclient.BatchDeleteTodo(todoclient.DeleteTodoParameters{ID: uuid.New()})
	require.NoError(t, err)
	_, err = alice.Batch(t.Context(), []livewire.BatchCall{complete, missing}, nil)
	requireCode(t, err, livewire.CodeNotFound)
	var queued int
	require.NoError(t, application.pool.QueryRow(t.Context(), "SELECT count(*) FROM govengers_jobs WHERE kind = 'todo.completed'").Scan(&queued))
	require.Zero(t, queued, "the completion rolled back with the batch, its job too (D41)")
	again, err := alice.GetTodosItem(t.Context(), todoclient.TodosItemParameters{ID: todo.ID})
	require.NoError(t, err)
	require.False(t, again.Done)
}

func TestEverySocketOutcomeHasAWideEventOfItsOwn(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	socket := application.dial(t, application.session(t, "alice"))
	queried, _ := socket.request(t, livewire.RequestFrame{Operation: livewire.OperationQuery, Target: "todos"})
	require.True(t, queried.OK)
	refused, _ := socket.request(t, livewire.RequestFrame{Operation: livewire.OperationQuery, Target: "todos.item", Parameters: map[string]string{"id": uuid.NewString()}})
	require.False(t, refused.OK)
	events := application.events.named("livewire.request")
	require.Len(t, events, 2, "one event per socket query (LOG-001), none lost on the upgrade's emitted event")
	require.Equal(t, "todos", nested(events[0], "livewire", "target"))
	require.Equal(t, "success", events[0]["outcome"])
	require.Equal(t, "todos.item", nested(events[1], "livewire", "target"))
	require.Equal(t, "failure", events[1]["outcome"])
}
