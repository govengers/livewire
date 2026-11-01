package client_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
)

func TestAConsumerMutatingItsPushLeavesTheStateAlone(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	pushes, err := client.Subscribe[livewire.ListResponse[item]](t.Context(), created, "parts", nil)
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s1", "")
	socket.push("s1", 1, livewire.PushSnapshot, `{"limit":10,"offset":0,"total":2,"items":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}`)
	receive(t, pushes)
	socket.push("s1", 2, livewire.PushListChange, `{"upserts":[{"id":"b","name":"B2"}],"removes":[],"order":["b","a"]}`)
	changed := receive(t, pushes)
	changed.Change.Order[0] = "a"                // the consumer reorders the change it got
	copy(changed.Change.Upserts[0], `{"id":"x"`) // and scribbles over its bytes
	changed.Data.Items[0].Name = "changed"
	socket.push("s1", 3, livewire.PushListChange, `{"upserts":[],"removes":[],"total":3}`)
	next := receive(t, pushes)
	require.Equal(t, []item{{"b", "B2"}, {"a", "A"}}, next.Data.Items, "the live state owns its own storage")
	require.Equal(t, 3, next.Data.Total)
}

// panicking is a Data type whose decoding panics.
type panicking struct{}

func (*panicking) UnmarshalJSON([]byte) error { panic("decoder bug") }

func TestAPanickingDeliveryEndsWithItsReason(t *testing.T) {
	server := newSocketServer(t)
	created := newLiveClient(t, server, 0)
	pushes, err := client.Subscribe[panicking](t.Context(), created, "dashboard", nil)
	require.NoError(t, err)
	socket := server.accept(t)
	socket.subscribed(socket.next(), "s1", "")
	socket.push("s1", 1, livewire.PushReplace, `{}`)
	last := receive(t, pushes)
	require.Equal(t, livewire.PushError, last.Kind)
	require.ErrorContains(t, last.Err, "the delivery failed: decoder bug")
	requireClosed(t, pushes)
}

func TestFailedConnectsAreReported(t *testing.T) {
	server := newSocketServer(t)
	server.refuse <- http.StatusServiceUnavailable
	var mutex sync.Mutex
	var reported []error
	created, err := client.New(&client.Options{
		BaseURL: "http://127.0.0.1:1", APIKey: apiKey, SocketURL: server.url, ReconnectDelay: time.Millisecond,
		ConnectFailed: func(err error) {
			mutex.Lock()
			defer mutex.Unlock()
			reported = append(reported, err)
			panic("a callback that panics costs its report only")
		},
	})
	require.NoError(t, err)
	pushes, err := client.Subscribe[item](t.Context(), created, "dashboard", nil)
	require.NoError(t, err)
	socket := server.accept(t) // the second attempt connects
	socket.subscribed(socket.next(), "s1", "")
	socket.push("s1", 1, livewire.PushReplace, `{"id":"d"}`)
	require.Equal(t, "d", receive(t, pushes).Data.ID)
	mutex.Lock()
	defer mutex.Unlock()
	require.Len(t, reported, 1)
	require.ErrorContains(t, reported[0], "connecting to "+server.url)
}

func TestD54RecoversOnlyA404(t *testing.T) {
	refusals := map[string]string{"/api/refused": "403", "/api/failed": "500"}
	created, server := newRESTClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost:
			answer(http.StatusCreated, `{"id":"filter-1","resource":"parts","filter":{}}`)(writer, request)
		case refusals[request.URL.Path] == "403":
			answer(http.StatusForbidden, `{"code":"not_found","message":"not found"}`)(writer, request)
		default:
			answer(http.StatusInternalServerError, `{"code":"not_found","message":"not found"}`)(writer, request)
		}
	})
	_, err := created.SaveFilter(t.Context(), &livewire.FilterRequest{Resource: "parts", Filter: json.RawMessage(`{}`)})
	require.NoError(t, err)
	for path := range refusals {
		before := server.count()
		err := created.Read(t.Context(), strings.TrimPrefix(path, "/api"), client.Parameters{livewire.ParameterFilterID: "filter-1"}, nil)
		var refusal *client.Error
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, livewire.CodeNotFound, refusal.Code)
		require.Equal(t, refusals[path], strconv.Itoa(refusal.Status))
		require.Equal(t, before+1, server.count(), "%s: a refusal other than 404 is not retried inline", path)
	}
}

func TestAnUploadOfUnknownLengthSendsItsSize(t *testing.T) {
	created, server := newRESTClient(t, answer(http.StatusCreated, `{}`))
	body := io.MultiReader(strings.NewReader("hel"), strings.NewReader("lo")) // net/http knows no length of it
	require.NoError(t, created.Upload(t.Context(), &client.Upload{Path: "/files", File: livewire.File{Body: body, Size: 5, ContentType: "image/png"}}, nil))
	require.Equal(t, int64(5), server.last().length, "the raw body carries its Content-Length (D60)")
}

func TestAnIdempotencyKeyMustBeAUUID(t *testing.T) {
	created, server := newRESTClient(t, answer(http.StatusOK, `{}`))
	for _, call := range []func() error{
		func() error {
			return created.Call(t.Context(), &client.Call{Method: http.MethodPost, Path: "/parts", IdempotencyKey: "retry-1"}, nil)
		},
		func() error {
			return created.Upload(t.Context(), &client.Upload{Path: "/files", File: livewire.File{Body: strings.NewReader(""), ContentType: "a/b"}, IdempotencyKey: "retry-1"}, nil)
		},
		func() error {
			_, err := created.Batch(t.Context(), nil, &client.CallOptions{IdempotencyKey: "retry-1"})
			return err
		},
	} {
		err := call()
		require.ErrorContains(t, err, `the Idempotency-Key "retry-1" is no UUID`)
		var refusal *client.Error
		require.False(t, errors.As(err, &refusal))
	}
	require.Zero(t, server.count(), "nothing went out")
}
