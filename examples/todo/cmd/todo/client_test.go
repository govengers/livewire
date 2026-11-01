package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/govengers/jobs"
	"github.com/govengers/jobs/jobstest"
	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"

	"github.com/govengers/livewire/examples/todo/client/todoclient"
	"github.com/govengers/livewire/examples/todo/client/todocommand"
	"github.com/govengers/livewire/examples/todo/internal/api"
	"github.com/govengers/livewire/examples/todo/internal/controller"
)

// The generated Go client and CLI against the application (the gate of WP7, D67, D68).

func TestTheGoClientRunsATodoThroughItsLife(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	alice := application.client(t, "alice")
	ctx := t.Context()

	id := client.NewUUIDv7()
	created, err := alice.CreateTodo(ctx, api.CreateTodoRequest{ID: id, Title: "=buy milk", Priority: api.PriorityHigh}, nil)
	require.NoError(t, err)
	require.Equal(t, id, created.ID, "the client's id (D18)")
	again, err := alice.CreateTodo(ctx, api.CreateTodoRequest{ID: id, Title: "=buy milk"}, nil)
	require.Nil(t, again)
	requireCode(t, err, livewire.CodeConflict)

	done, err := alice.UpdateTodo(ctx, todoclient.UpdateTodoParameters{ID: id}, api.UpdateTodoRequest{Done: new(true)}, nil)
	require.NoError(t, err)
	require.True(t, done.Done)
	require.Equal(t, 2, done.Version)
	require.Equal(t, 1, jobstest.Drain(t, application.worker), "the completion job, enqueued in the update's transaction")
	activity, err := alice.GetActivity(ctx, todoclient.ActivityParameters{TodoID: &id})
	require.NoError(t, err)
	require.Len(t, activity.Items, 1)
	require.Equal(t, `alice completed "=buy milk"`, activity.Items[0].Message)

	history, err := alice.GetTodosHistory(ctx, todoclient.TodosHistoryQuery{ID: id})
	require.NoError(t, err)
	require.Len(t, history.Items, 2)
	require.Equal(t, "createTodo", history.Items[0].Mutation)
	require.Equal(t, "updateTodo", history.Items[1].Mutation)
	require.True(t, history.Items[1].Data.Done)

	attachment, err := alice.UploadAttachment(ctx, todoclient.UploadAttachmentParameters{ID: id}, textFile("two litres"), nil)
	require.NoError(t, err)
	require.Equal(t, int64(len("two litres")), attachment.Size)
	summary, err := alice.GetSummary(ctx)
	require.NoError(t, err)
	require.Equal(t, api.SummaryItem{Open: 0, Done: 1, Attachments: 1}, *summary)

	var exported bytes.Buffer
	rows, err := alice.ExportTodos(ctx, todoclient.TodosExportQuery{}, &exported)
	require.NoError(t, err)
	require.Equal(t, 1, rows)
	records, err := csv.NewReader(&exported).ReadAll()
	require.NoError(t, err)
	require.Contains(t, records[1], "'=buy milk", "a formula is written quoted (D69)")

	require.NoError(t, alice.DeleteTodo(ctx, todoclient.DeleteTodoParameters{ID: id}, nil))
	_, err = alice.GetTodosItem(ctx, todoclient.TodosItemParameters{ID: id})
	requireCode(t, err, livewire.CodeNotFound)
}

func TestTheGoClientBatchesFiltersAndReplays(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	alice := application.client(t, "alice")
	ctx := t.Context()

	first, err := todoclient.BatchCreateTodo(api.CreateTodoRequest{Title: "first"})
	require.NoError(t, err)
	second, err := todoclient.BatchCreateTodo(api.CreateTodoRequest{Title: "second", Priority: api.PriorityLow})
	require.NoError(t, err)
	key := client.NewIdempotencyKey()
	results, err := alice.Batch(ctx, []livewire.BatchCall{first, second}, &client.CallOptions{IdempotencyKey: key})
	require.NoError(t, err)
	require.Len(t, results, 2)
	replayed, err := alice.Batch(ctx, []livewire.BatchCall{first, second}, &client.CallOptions{IdempotencyKey: key})
	require.NoError(t, err)
	require.Len(t, replayed, len(results))
	for index, result := range results {
		require.Equal(t, result.Status, replayed[index].Status)
		require.Equal(t, string(result.Payload), string(replayed[index].Payload), "a replay answers the recorded results byte for byte (D16, D72)")
	}

	filter := todoclient.TodosFilter{Where: new(todoclient.TodosPriorityEqual(todoclient.TodosPriorityLow))}
	document, err := json.Marshal(filter)
	require.NoError(t, err)
	stored, err := alice.SaveFilter(ctx, &livewire.FilterRequest{Resource: "todos", Name: "low", Filter: document})
	require.NoError(t, err)
	low, err := alice.GetTodos(ctx, todoclient.TodosListQuery{FilterID: stored.ID})
	require.NoError(t, err)
	require.Len(t, low.Items, 1)
	require.Equal(t, "second", low.Items[0].Title)
	named, err := alice.ListFilters(ctx, "todos")
	require.NoError(t, err)
	require.Len(t, named.Items, 1)
	distinct, err := alice.GetTodosDistinct(ctx, todoclient.TodosDistinctQuery{Field: todoclient.TodosDistinctPriority})
	require.NoError(t, err)
	require.Len(t, distinct, 2)
}

func TestTheGoClientSubscriptionSeesAnotherClientsWrite(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	alice := application.client(t, "alice")
	carol := application.client(t, "carol")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	pushes, err := alice.SubscribeTodos(ctx, todoclient.TodosListQuery{})
	require.NoError(t, err)
	snapshot := nextPush(t, pushes)
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.Empty(t, snapshot.Data.Items)

	created, err := carol.CreateTodo(t.Context(), api.CreateTodoRequest{Title: "from carol"}, nil)
	require.NoError(t, err)
	change := nextPush(t, pushes)
	require.Equal(t, livewire.PushListChange, change.Kind)
	require.Len(t, change.Data.Items, 1)
	require.Equal(t, created.ID, change.Data.Items[0].ID, "the same workspace, another user, over the change feed")
	cancel()
	for range pushes { // the channel closes with the subscription
	}
}

func TestTheCommandLineClientRunsAgainstTheApplication(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	key := application.apiKey(t, "alice")
	run := func(arguments ...string) (int, string, string) {
		var output, diagnostics bytes.Buffer
		full := append([]string{"todo", "--base-url", application.baseURL(), "--api-key", key}, arguments...)
		code := todocommand.Run(t.Context(), full, strings.NewReader(""), &output, &diagnostics)
		return code, output.String(), diagnostics.String()
	}

	code, output, diagnostics := run("create-todo", "--body", `{"title":"from the command line"}`)
	require.Equal(t, 0, code, diagnostics)
	var created api.TodoItem
	require.NoError(t, json.Unmarshal([]byte(output), &created))
	require.Equal(t, "from the command line", created.Title)

	code, output, diagnostics = run("todos", "list", "--sort", "-createdAt")
	require.Equal(t, 0, code, diagnostics)
	var listed livewire.ListResponse[api.TodoItem]
	require.NoError(t, json.Unmarshal([]byte(output), &listed))
	require.Len(t, listed.Items, 1)

	code, output, diagnostics = run("todos", "export")
	require.Equal(t, 0, code, diagnostics)
	require.Contains(t, output, "from the command line")

	code, _, diagnostics = run("update-todo", "--id", uuid.NewString(), "--body", `{"done":true}`)
	require.Equal(t, 3, code, "a refusal exits 3")
	require.Contains(t, diagnostics, "not_found")
}

func TestTheMaintenanceJobsRun(t *testing.T) {
	t.Parallel()
	application := newTestApplication(t)
	var maintenance []*jobs.PreparedJob
	for _, schedule := range controller.Schedules() {
		maintenance = append(maintenance, schedule.Job) // what the schedules enqueue once per period
	}
	_, err := application.jobStore.Enqueue(t.Context(), application.pool, maintenance...)
	require.NoError(t, err)
	require.Equal(t, 3, jobstest.Drain(t, application.worker))
	month := "livewire_audit_" + time.Now().UTC().Format("200601")
	var exists bool
	require.NoError(t, application.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", month).Scan(&exists))
	require.True(t, exists, "the audit maintenance created this month's partition (D32)")
}

func nextPush[Data any](t *testing.T, pushes <-chan client.Push[Data]) client.Push[Data] {
	t.Helper()
	select {
	case push, open := <-pushes:
		require.True(t, open, "the subscription ended")
		require.NoError(t, push.Err)
		return push
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no push in time")
		return client.Push[Data]{}
	}
}

func requireCode(t *testing.T, err error, code livewire.Code) {
	t.Helper()
	var refused *client.Error
	require.ErrorAs(t, err, &refused)
	require.Equal(t, code, refused.Code)
}
