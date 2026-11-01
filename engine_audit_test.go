package livewire_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestCallsAreAuditedInTheirTransaction(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter))
	call := func(target string, body any, key string) error {
		request := livewire.CallRequest{Target: target, IdempotencyKey: key, RequestID: "request-" + target}
		if body != nil {
			request.Body = mustJSON(body)
		}
		_, _, err := bed.engine.Call(ctx, request)
		return err
	}

	require.NoError(t, call("createGadget", map[string]string{"name": "Anchor"}, firstKey))
	entries := bed.audit.landed()
	require.Len(t, entries, 1)
	require.True(t, entries[0].inTransaction, "written in the mutation's transaction, committed with it")
	entry := entries[0].entry
	require.Equal(t, "createGadget", entry.Mutation)
	require.Equal(t, http.StatusCreated, entry.Status)
	require.Equal(t, "ada", entry.Identity.Subject)
	require.Equal(t, "request-createGadget", entry.RequestID)
	require.Equal(t, firstKey, entry.IdempotencyKey)
	require.IsType(t, &livewire.NoParameters{}, entry.Parameters)
	require.Positive(t, entry.Duration)

	require.NoError(t, call("createGadget", map[string]string{"name": "Anchor"}, firstKey), "a replay")
	requireStatus(t, call("createGadget", map[string]string{"group": "a"}, ""), http.StatusBadRequest)
	require.Len(t, bed.audit.landed(), 1, "a replay and a refusal before the handler write none")

	requireStatus(t, call("failGadget", nil, ""), http.StatusConflict)
	requireStatus(t, call("panicGadget", nil, ""), http.StatusInternalServerError)
	entries = bed.audit.landed()
	require.Len(t, entries, 3)
	for _, failed := range entries[1:] {
		require.False(t, failed.inTransaction, "a failed call is audited after the rollback")
	}
	require.Equal(t, []int{http.StatusConflict, http.StatusInternalServerError}, []int{entries[1].entry.Status, entries[2].entry.Status})

	file := &livewire.File{ContentType: "image/png", Size: 3, Body: strings.NewReader("png")}
	_, _, err := bed.engine.Upload(ctx, livewire.UploadRequest{Target: "uploadPicture", File: file})
	require.NoError(t, err)
	require.Equal(t, "uploadPicture", bed.audit.landed()[3].entry.Mutation, "an upload is a write")
}

func TestBatchCallsAreAuditedOnePerCall(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	committed := socket.batch([]livewire.BatchCall{
		{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Anchor"})},
		{Target: "removeGadget", Parameters: map[string]string{"id": "x"}},
	}, firstKey)
	require.True(t, committed.OK, "%+v", committed.Error)
	entries := bed.audit.landed()
	require.Len(t, entries, 2)
	require.Equal(t, []string{"createGadget", "removeGadget"}, []string{entries[0].entry.Mutation, entries[1].entry.Mutation})
	require.Equal(t, []int{http.StatusCreated, http.StatusNoContent}, []int{entries[0].entry.Status, entries[1].entry.Status})
	require.Equal(t, entries[0].entry.RequestID, entries[1].entry.RequestID, "one batch, one request id")
	require.Equal(t, firstKey, entries[1].entry.IdempotencyKey)

	failed := socket.batch([]livewire.BatchCall{
		{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Bolt"})},
		{Target: "failGadget"},
		{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Never"})},
	}, secondKey)
	require.Equal(t, http.StatusConflict, failed.Status)
	entries = bed.audit.landed()[2:]
	require.Len(t, entries, 2, "the calls that ran, after the rollback")
	for _, entry := range entries {
		require.False(t, entry.inTransaction)
		require.Equal(t, http.StatusConflict, entry.entry.Status, "with the status the batch answered")
	}
}

func TestAFailingAuditFailsTheCall(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.audit.failWrite.Store(true)
	socket := bed.dial("ada")
	refused := socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, "")
	require.Equal(t, http.StatusInternalServerError, refused.Status)
	_, exists := bed.world.lookup("g-Anchor")
	require.False(t, exists, "the audit is part of the transaction: no entry, no write")
	outcome := bed.outcomes.of(livewire.OutcomeCall)[0]
	require.ErrorContains(t, outcome.Err, "the audit table is gone")
}

func TestAFailedCallIsAuditedThoughItsCallerWentAway(t *testing.T) {
	bed := newTestbed(t, nil)
	cancelled := func(run func(ctx context.Context) error) {
		t.Helper()
		ctx, cancel := context.WithCancel(auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter)))
		defer cancel()
		bed.cancelCall.Store(&cancel)
		defer bed.cancelCall.Store(nil)
		requireStatus(t, run(ctx), http.StatusConflict)
	}
	cancelled(func(ctx context.Context) error {
		_, _, err := bed.engine.Call(ctx, livewire.CallRequest{Target: "cancelGadget"})
		return err
	})
	cancelled(func(ctx context.Context) error {
		_, err := bed.engine.Batch(ctx, livewire.BatchRequest{Body: mustJSON([]livewire.BatchCall{{Target: "cancelGadget"}})})
		return err
	})
	entries := bed.audit.landed()
	require.Len(t, entries, 2, "the call and the batch call are audited after their caller cancelled")
	for _, entry := range entries {
		require.Equal(t, "cancelGadget", entry.entry.Mutation)
		require.Equal(t, http.StatusConflict, entry.entry.Status)
		require.Equal(t, "ada", entry.entry.Identity.Subject, "the identity survives the cancellation")
	}
}
