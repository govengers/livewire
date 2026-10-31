package livewire_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

const (
	firstKey  = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b4c"
	secondKey = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b4d"
)

func (c *client) call(target string, parameters map[string]string, body any, key string) livewire.ResponseFrame {
	c.t.Helper()
	frame := livewire.RequestFrame{Operation: livewire.OperationCall, Target: target, Parameters: parameters, IdempotencyKey: key}
	if body != nil {
		frame.Body = mustJSON(body)
	}
	return c.response(c.sendFrame(frame))
}

func TestCallRunsInTheEngineTransactionAndReplaysItsKey(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()

	created := socket.call("createGadget", nil, map[string]string{"name": "Anchor", "group": "a"}, firstKey)
	require.True(t, created.OK, "%+v", created.Error)
	require.Equal(t, http.StatusCreated, created.Status)
	require.JSONEq(t, `{"id":"g-Anchor","name":"Anchor","group":"a","secret":""}`, string(created.Payload))
	require.Equal(t, livewire.PushListChange, socket.push().Kind, "the commit's change reaches the subscription")

	replayed := socket.call("createGadget", nil, map[string]string{"name": "Anchor", "group": "a"}, firstKey)
	require.Equal(t, http.StatusCreated, replayed.Status, "a finished key answers its recorded response (D16)")
	require.JSONEq(t, string(created.Payload), string(replayed.Payload))
	conflict := socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, secondKey)
	require.Equal(t, http.StatusConflict, conflict.Status, "a fresh key runs the handler again")

	writes := bed.transactor.writes()
	require.Len(t, writes, 3)
	require.Equal(t, "createGadget", writes[0].Mutation)
	require.Equal(t, "ada", writes[0].Identity.Subject)
	require.NotEmpty(t, writes[0].RequestID)
	require.Len(t, bed.outcomes.of(livewire.OutcomeAfterCommit), 1, "only the first call committed")
	calls := bed.outcomes.of(livewire.OutcomeCall)
	require.Len(t, calls, 3)
	require.Equal(t, livewire.OperationCall, calls[0].Operation)
	require.Equal(t, http.StatusCreated, calls[0].Status)

	invalidKey := socket.call("createGadget", nil, map[string]string{"name": "Bolt"}, "not-a-uuid")
	require.Equal(t, http.StatusBadRequest, invalidKey.Status)
	require.Contains(t, invalidKey.Error.Fields, "idempotencyKey")
	unknownField := socket.call("createGadget", nil, map[string]string{"name": "Bolt", "color": "red"}, "")
	require.Equal(t, http.StatusBadRequest, unknownField.Status, "unknown fields are refused (D40)")
	removed := socket.call("removeGadget", map[string]string{"id": "g-Anchor"}, nil, "")
	require.Equal(t, http.StatusNoContent, removed.Status)
	require.Empty(t, removed.Payload)
}

func TestCallInFlightKeyAnswersInProgress(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	blocked := socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationCall, Target: "blockGadget", IdempotencyKey: firstKey})
	require.Eventually(t, func() bool { return len(bed.transactor.writes()) == 1 }, frameWait, time.Millisecond)

	again := socket.call("blockGadget", nil, nil, firstKey)
	require.Equal(t, http.StatusConflict, again.Status)
	require.Equal(t, livewire.CodeInProgress, again.Error.Code)
	close(bed.release)
	require.True(t, socket.response(blocked).OK)
	require.True(t, socket.call("blockGadget", nil, nil, firstKey).OK, "the finished key replays")
}

func TestCallRefusals(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.identities.set(person("rita", roleReader))
	socket := bed.dial("ada")
	reader := bed.dial("rita")
	testCases := []struct {
		name   string
		socket *client
		target string
		body   any
		status int
	}{
		{"unknown mutation", socket, "nothing", nil, http.StatusNotFound},
		{"a read is no mutation", socket, "gadgets", nil, http.StatusNotFound},
		{"uploads are REST only", socket, "uploadPicture", nil, http.StatusBadRequest},
		{"the static check", reader, "createGadget", map[string]string{"name": "x"}, http.StatusForbidden},
		{"validation", socket, "createGadget", map[string]string{"group": "a"}, http.StatusBadRequest},
		{"a handler refusal rolls back", socket, "failGadget", nil, http.StatusConflict},
		{"a panicking handler", socket, "panicGadget", nil, http.StatusInternalServerError},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			response := testCase.socket.call(testCase.target, nil, testCase.body, "")
			require.Equal(t, testCase.status, response.Status, "%+v", response.Error)
			require.False(t, response.OK)
		})
	}
	require.Equal(t, int64(2), bed.transactor.rollbacks.Load(), "the refusing and the panicking handler rolled back; the others never began")
}

func TestBatchIsAllOrNothing(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	failing := []livewire.BatchCall{
		{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})},
		{Target: "failGadget"},
	}
	refused := socket.response(socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(failing), IdempotencyKey: firstKey}))
	require.Equal(t, http.StatusConflict, refused.Status)
	require.Equal(t, livewire.CodeConflict, refused.Error.Code)
	require.Equal(t, "conflict", refused.Error.Fields["calls.1"], "the refusal names the failing call")
	_, exists := bed.world.lookup("g-Anchor")
	require.False(t, exists, "the first call rolled back with the second")

	succeeding := []livewire.BatchCall{
		{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})},
		{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Bolt"})},
	}
	committed := socket.response(socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(succeeding), IdempotencyKey: firstKey}))
	require.True(t, committed.OK, "%+v", committed.Error)
	var results []livewire.BatchResult
	require.NoError(t, json.Unmarshal(committed.Payload, &results))
	require.Len(t, results, 2)
	require.Equal(t, http.StatusCreated, results[1].Status)
	_, exists = bed.world.lookup("g-Bolt")
	require.True(t, exists)
	require.Len(t, bed.outcomes.of(livewire.OutcomeAfterCommit), 2, "one per committed call")
	require.Empty(t, bed.transactor.writes()[1].Mutation, "a batch's outer transaction names no mutation")

	invalid := []livewire.BatchCall{{Target: "createGadget", Body: mustJSON(map[string]string{"group": "x"})}}
	badCall := socket.response(socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: mustJSON(invalid)}))
	require.Equal(t, http.StatusBadRequest, badCall.Status)
	require.Contains(t, badCall.Error.Fields, "calls.0.name")
	for _, body := range []string{`{}`, `[]`, `[{"target":"createGadget","extra":1}]`, `[` + strings.Repeat(`{"target":"failGadget"},`, 100) + `{"target":"failGadget"}]`} {
		response := socket.response(socket.sendFrame(livewire.RequestFrame{Operation: livewire.OperationBatch, Body: json.RawMessage(body)}))
		require.Equal(t, http.StatusBadRequest, response.Status, body)
	}
}

func TestEngineReadCallBatchAndUploadForREST(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter))
	status, payload, err := bed.engine.Call(ctx, livewire.CallRequest{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	require.Contains(t, string(payload), "g-Anchor")

	read, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets", Source: livewire.MapSource{"limit": "5"}})
	require.NoError(t, err)
	list := listOf(t, read)
	require.Equal(t, 5, list.Limit)
	require.Equal(t, 1, list.Total)
	_, err = bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets", Kind: livewire.ReadItem, Source: livewire.MapSource{"id": "nope"}})
	requireStatus(t, err, http.StatusNotFound)
	_, err = bed.engine.Read(context.Background(), livewire.ReadRequest{Target: "gadgets"})
	requireStatus(t, err, http.StatusUnauthorized)

	batch, err := bed.engine.Batch(ctx, livewire.BatchRequest{Body: mustJSON([]livewire.BatchCall{{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Bolt"})}})})
	require.NoError(t, err)
	require.Contains(t, string(batch), `"status":201`)

	file := &livewire.File{ContentType: "image/png", Size: 3, Body: strings.NewReader("png")}
	status, payload, err = bed.engine.Upload(ctx, livewire.UploadRequest{Target: "uploadPicture", File: file})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"bytes":3}`, string(payload))
	refusals := []*livewire.File{
		nil,
		{ContentType: "text/plain", Size: 3, Body: strings.NewReader("txt")},
		{ContentType: "image/png", Size: 4096, Body: strings.NewReader("big")},
	}
	for _, refused := range refusals {
		_, _, err = bed.engine.Upload(ctx, livewire.UploadRequest{Target: "uploadPicture", File: refused})
		requireStatus(t, err, http.StatusBadRequest)
	}
	_, _, err = bed.engine.Upload(ctx, livewire.UploadRequest{Target: "createGadget", File: file})
	requireStatus(t, err, http.StatusNotFound)
	require.Empty(t, bed.outcomes.of(livewire.OutcomeRequest)[0].Operation, "REST outcomes carry no socket op")
}

func TestTransactionFailuresAnswer500(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter))
	bed.transactor.failBegin.Store(true)
	_, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets"})
	requireStatus(t, err, http.StatusInternalServerError)
	_, _, err = bed.engine.Call(ctx, livewire.CallRequest{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})})
	requireStatus(t, err, http.StatusInternalServerError)
}

func TestTenantIsResolvedForReadsAndWrites(t *testing.T) {
	feed := newFakeFeed()
	bed := &testbed{t: t, world: newWorld(feed), feed: feed, identities: newFakeIdentities(), outcomes: &outcomes{}, release: make(chan struct{})}
	bed.transactor = &fakeTransactor{world: bed.world}
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{
		Observer: bed.outcomes.observe,
		Tenant: func(ctx context.Context, _ any) (string, error) {
			return "tenant-" + auth.IdentityFrom(ctx).Subject, nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, registry.Add(bed.gadgets(), bed.createGadget()))
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: bed.identities, Transactor: bed.transactor, SameHostOnly: true})
	require.NoError(t, err)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter))
	_, err = engine.Read(ctx, livewire.ReadRequest{Target: "gadgets"})
	require.NoError(t, err)
	_, _, err = engine.Call(ctx, livewire.CallRequest{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})})
	require.NoError(t, err)
	for _, details := range bed.transactor.details {
		require.Equal(t, "tenant-ada", details.Tenant, "every engine transaction carries the tenant (D42)")
	}
	require.Equal(t, "tenant-ada", bed.outcomes.of(livewire.OutcomeRequest)[0].Tenant)
	require.NoError(t, engine.Close(t.Context()))
}

func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	require.Error(t, err)
	got, _ := livewire.ResponseFor(err)
	require.Equal(t, status, got, "%v", err)
}
