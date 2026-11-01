//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
)

func TestAWriteOnOneEngineReRunsASubscriptionOnTheOther(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pushes, err := client.Subscribe[livewire.ListResponse[note]](ctx, clientOf(t, cluster.b, "alice-key"), "notes", nil)
	require.NoError(t, err)
	require.Empty(t, nextPush(t, pushes).Data.Items)

	created := createNote(t, clientOf(t, cluster.a, "alice-key"), createNoteBody{Title: "from engine a"}, "")
	diff := nextPush(t, pushes)
	require.Equal(t, livewire.PushListChange, diff.Kind, "LISTEN fans the write on engine A out to engine B")
	require.Equal(t, []note{*created}, diff.Data.Items)

	createNote(t, clientOf(t, cluster.a, "bob-key"), createNoteBody{Title: "bob's"}, "")
	createNote(t, clientOf(t, cluster.b, "alice-key"), createNoteBody{Title: "second"}, "")
	require.Len(t, nextPush(t, pushes).Data.Items, 2, "bob's note re-runs the subscription but changes nothing alice reads")
}

func TestAnIdempotencyKeyReplaysOnTheOtherEngine(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	key := client.NewIdempotencyKey()
	first := createNote(t, clientOf(t, cluster.a, "alice-key"), createNoteBody{Title: "once"}, key)
	replayed := createNote(t, clientOf(t, cluster.b, "alice-key"), createNoteBody{Title: "once"}, key)
	require.Equal(t, first, replayed, "engine B answers what engine A recorded (D16)")
	var rows int
	require.NoError(t, cluster.pool.QueryRow(t.Context(), "SELECT count(*) FROM notes").Scan(&rows))
	require.Equal(t, 1, rows)

	other := createNote(t, clientOf(t, cluster.b, "bob-key"), createNoteBody{Title: "bob's"}, key)
	require.NotEqual(t, first.ID, other.ID, "a key of another owner is a fresh key (section 6)")
}

func TestTheGoClientRecoversAFilterTheStoreEvicted(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	alice := clientOf(t, cluster.a, "alice-key")
	createNote(t, alice, createNoteBody{Title: "apples"}, "")
	createNote(t, alice, createNoteBody{Title: "pears"}, "")
	saved, err := alice.SaveFilter(t.Context(), &livewire.FilterRequest{Resource: "notes", Filter: json.RawMessage(`{"search":"apples"}`)})
	require.NoError(t, err)
	for _, search := range []string{"pears", "plums"} { // past the cap of two, the least recently used goes (D54)
		_, err := alice.SaveFilter(t.Context(), &livewire.FilterRequest{Resource: "notes", Filter: json.RawMessage(`{"search":"` + search + `"}`)})
		require.NoError(t, err)
	}
	var stored int
	require.NoError(t, cluster.pool.QueryRow(t.Context(), "SELECT count(*) FROM livewire_filters WHERE id = $1", saved.ID).Scan(&stored))
	require.Zero(t, stored, "the FilterStore evicted the first filter")

	var page livewire.ListResponse[note]
	require.NoError(t, alice.Read(t.Context(), "/notes", client.Parameters{livewire.ParameterFilterID: saved.ID}, &page))
	require.Len(t, page.Items, 1, "the 404 sent the filter inline once")
	require.Equal(t, "apples", page.Items[0].Title)
}

func TestARecreatedNoteStartsANewHistoryAcrossEngines(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	onA, onB := clientOf(t, cluster.a, "alice-key"), clientOf(t, cluster.b, "alice-key")
	id := client.NewUUIDv7()
	createNote(t, onA, createNoteBody{ID: id, Title: "first life"}, "")
	require.NoError(t, onB.Call(t.Context(), &client.Call{Method: http.MethodPatch, Path: "/notes/{id}", Parameters: client.Parameters{"id": id.String()}, Body: renameNoteBody{Title: "renamed"}}, nil))
	require.NoError(t, onA.Call(t.Context(), &client.Call{Method: http.MethodDelete, Path: "/notes/{id}", Parameters: client.Parameters{"id": id.String()}}, nil))
	createNote(t, onB, createNoteBody{ID: id, Title: "second life"}, "") // the same client id after the delete (D18)

	history := readHistory(t, onA, id)
	require.Len(t, history.Items, 1, "the deleted note's versions are not the new note's (D56)")
	require.Equal(t, "createNote", history.Items[0].Mutation)
	require.Equal(t, "second life", history.Items[0].Data.Title)
	var versions int
	require.NoError(t, cluster.pool.QueryRow(t.Context(), "SELECT count(*) FROM notes_history WHERE id = $1", id).Scan(&versions))
	require.Equal(t, 4, versions, "the first life's create, rename and delete stay in the table")
}

func TestABatchNamesEachCallInTheHistory(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	alice := clientOf(t, cluster.a, "alice-key")
	id := client.NewUUIDv7()
	results, err := alice.Batch(t.Context(), []livewire.BatchCall{
		{Target: "createNote", Body: json.RawMessage(`{"id":"` + id.String() + `","title":"batched"}`)},
		{Target: "renameNote", Parameters: map[string]string{"id": id.String()}, Body: json.RawMessage(`{"title":"renamed in the batch"}`)},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, results[0].Status)
	require.Equal(t, http.StatusOK, results[1].Status)

	history := readHistory(t, clientOf(t, cluster.b, "alice-key"), id)
	require.Len(t, history.Items, 2)
	require.Equal(t, "createNote", history.Items[0].Mutation, "CallDescriber names each call (D58)")
	require.Equal(t, "renameNote", history.Items[1].Mutation)
	require.Equal(t, history.Items[0].RequestID, history.Items[1].RequestID, "one batch, one request")
	require.Equal(t, "apikey:alice", history.Items[1].Actor)
}

func TestPresenceJoinedOnOneEngineIsReadOnTheOther(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	created := createNote(t, clientOf(t, cluster.a, "alice-key"), createNoteBody{Title: "shared"}, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	presences, err := client.Subscribe[livewire.ListResponse[livewire.PresenceItem]](ctx, clientOf(t, cluster.b, "alice-key"), "notes.presence",
		client.Parameters{livewire.ParameterPresenceKey: created.ID.String()})
	require.NoError(t, err)
	require.Empty(t, nextPush(t, presences).Data.Items)

	socket := dial(t, cluster.a, "alice-key")
	joined := socket.request(t, livewire.RequestFrame{
		Operation: livewire.OperationJoin, Target: "notes",
		Parameters: map[string]string{livewire.ParameterPresenceKey: created.ID.String()}, Body: json.RawMessage(`{"editing":"title"}`),
	})
	require.True(t, joined.OK, "%+v", joined.Error)
	seen := nextPush(t, presences)
	require.Len(t, seen.Data.Items, 1, "the join on engine A reaches engine B through the change feed")
	require.Equal(t, "Alice", seen.Data.Items[0].Display)
	require.JSONEq(t, `{"editing":"title"}`, string(seen.Data.Items[0].State))

	socket.close(t)
	require.Empty(t, nextPush(t, presences).Data.Items, "the socket's close ends its presence")
}

func TestEveryCallLeavesAnAuditRow(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	key := client.NewIdempotencyKey()
	created := createNote(t, clientOf(t, cluster.a, "alice-key"), createNoteBody{Title: "audited"}, key)
	err := clientOf(t, cluster.b, "bob-key").Call(t.Context(), &client.Call{Method: http.MethodDelete, Path: "/notes/{id}", Parameters: client.Parameters{"id": created.ID.String()}}, nil)
	require.Error(t, err, "bob may not delete alice's note")
	rows, err := cluster.pool.Query(t.Context(), "SELECT mutation, actor, status, coalesce(idempotency_key::text, '') FROM livewire_audit ORDER BY at")
	require.NoError(t, err)
	type auditRow struct {
		Mutation, Actor string
		Status          int
		Key             string
	}
	var audited []auditRow
	for rows.Next() {
		var row auditRow
		require.NoError(t, rows.Scan(&row.Mutation, &row.Actor, &row.Status, &row.Key))
		audited = append(audited, row)
	}
	require.NoError(t, rows.Err())
	require.Len(t, audited, 2, "one row per call whose handler ran, the failed one written after the rollback (D32)")
	require.Equal(t, auditRow{Mutation: "createNote", Actor: "apikey:alice", Status: http.StatusCreated, Key: key}, audited[0])
	require.Equal(t, "deleteNote", audited[1].Mutation)
	require.Equal(t, http.StatusNotFound, audited[1].Status)
}

func TestTheExportStreamsEveryRowWithItsTrailer(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	alice := clientOf(t, cluster.a, "alice-key")
	for _, title := range []string{"=1+1", "b", "c", "d", "e"} { // chunks of two, the first a formula (D69)
		createNote(t, alice, createNoteBody{Title: title}, "")
	}
	var exported bytes.Buffer
	rows, err := clientOf(t, cluster.b, "alice-key").Export(t.Context(), "/notes/export", client.Parameters{livewire.ParameterSort: "title"}, &exported)
	require.NoError(t, err, "the Livewire-Export-Rows trailer arrived (D60)")
	require.Equal(t, 5, rows)
	records, err := csv.NewReader(&exported).ReadAll()
	require.NoError(t, err)
	require.Equal(t, []string{"id", "owner", "title", "version"}, records[0])
	titles := make([]string, 0, len(records)-1)
	for _, record := range records[1:] {
		titles = append(titles, record[2])
	}
	require.Equal(t, []string{"'=1+1", "b", "c", "d", "e"}, titles)
}

func TestTheGoClientReconnectsAcrossADeploy(t *testing.T) {
	t.Parallel()
	cluster := newCluster(t)
	var serving atomic.Pointer[http.Handler]
	serving.Store(&cluster.a.server.Config.Handler)
	front := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		(*serving.Load()).ServeHTTP(writer, request)
	}))
	defer front.Close()
	alice, err := client.New(&client.Options{BaseURL: front.URL + "/api", APIKey: "alice-key", ReconnectDelay: 10 * time.Millisecond, MaximumReconnectDelay: 50 * time.Millisecond})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pushes, err := client.Subscribe[livewire.ListResponse[note]](ctx, alice, "notes", nil)
	require.NoError(t, err)
	require.Equal(t, livewire.PushSnapshot, nextPush(t, pushes).Kind)

	serving.Store(&cluster.b.server.Config.Handler) // the deploy: new sockets reach engine B
	closing, closeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer closeCancel()
	require.NoError(t, cluster.a.engine.Close(closing), "engine A ends its sockets with 1001")
	createNote(t, clientOf(t, cluster.b, "alice-key"), createNoteBody{Title: "after the deploy"}, "")
	for {
		push := nextPush(t, pushes)
		if len(push.Data.Items) == 1 {
			require.Equal(t, "after the deploy", push.Data.Items[0].Title, "the client subscribed again on engine B")
			return
		}
	}
}

// readHistory reads a note's versions over REST.
func readHistory(t *testing.T, runtime *client.Client, id uuid.UUID) livewire.ListResponse[livewire.Version[note]] {
	t.Helper()
	var history livewire.ListResponse[livewire.Version[note]]
	require.NoError(t, runtime.Read(t.Context(), "/notes/{id}/history", client.Parameters{"id": id.String()}, &history))
	return history
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

// socket is a raw socket for the ops the Go client has no method for (join).
type socket struct {
	connection *websocket.Conn
	sequence   int
}

func dial(t *testing.T, replica *replica, key string) *socket {
	t.Helper()
	address := "ws" + strings.TrimPrefix(replica.server.URL, "http") + "/api/socket"
	connection, response, err := websocket.DefaultDialer.DialContext(t.Context(), address, http.Header{"Authorization": {"Bearer " + key}})
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	opened := &socket{connection: connection}
	t.Cleanup(func() { _ = connection.Close() }) // closed already when the test closed it
	return opened
}

// request sends a frame and answers its response, skipping pushes.
func (s *socket) request(t *testing.T, frame livewire.RequestFrame) livewire.ResponseFrame {
	t.Helper()
	s.sequence++
	frame.ID = "request-" + strings.Repeat("x", s.sequence)
	require.NoError(t, s.connection.SetWriteDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, s.connection.WriteJSON(frame))
	require.NoError(t, s.connection.SetReadDeadline(time.Now().Add(10*time.Second)))
	for {
		var response livewire.ResponseFrame
		require.NoError(t, s.connection.ReadJSON(&response))
		if response.ID == frame.ID {
			return response
		}
	}
}

func (s *socket) close(t *testing.T) {
	t.Helper()
	message := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done")
	require.NoError(t, s.connection.WriteControl(websocket.CloseMessage, message, time.Now().Add(5*time.Second)))
	require.NoError(t, s.connection.Close())
}
