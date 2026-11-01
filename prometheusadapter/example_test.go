package prometheusadapter_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/prometheusadapter"
)

type greeting struct {
	ID string `json:"id"`
}

// noTransactor begins transactions that do nothing; a real application passes
// postgresadapter's Transactor.
type noTransactor struct{}

func (noTransactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return noTransaction{}, nil
}

type noTransaction struct{}

func (noTransaction) Commit(context.Context) error   { return nil }
func (noTransaction) Rollback(context.Context) error { return nil }
func (noTransaction) Handle() any                    { return nil }

// anyone is the IdentitySource of the example; a real application passes session.Guard.
type anyone struct{}

func (anyone) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return &auth.Identity{Provider: "example", Subject: "ada", Active: true}, nil
}

func Example() {
	metricsRegistry := prometheus.NewRegistry() // the application's own, built in main (MET-002)
	metrics, err := prometheusadapter.New(&prometheusadapter.Options{Registerer: metricsRegistry, Namespace: "shop"})
	if err != nil {
		fmt.Println(err)
		return
	}
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{Observer: metrics.Observe}) // the one Observer (D49)
	if err != nil {
		fmt.Println(err)
		return
	}
	err = registry.Add(livewire.NewList("greetings", "/greetings", &livewire.ListOptions[greeting, livewire.NoParameters]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]greeting, error) { return []greeting{{ID: "1"}}, nil },
	}))
	if err != nil {
		fmt.Println(err)
		return
	}
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: anyone{}, Transactor: noTransactor{}, SameHostOnly: true})
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Provider: "example", Subject: "ada", Active: true})
	_, _ = engine.Read(ctx, livewire.ReadRequest{Target: "greetings"})
	_, _ = engine.Read(context.Background(), livewire.ReadRequest{Target: "greetings"})

	fmt.Println(testutil.CollectAndCount(metricsRegistry, "shop_livewire_units_total"))
	// Output: 2
}

func TestInvalidSocketFramesAddNoSeries(t *testing.T) {
	metricsRegistry := prometheus.NewRegistry()
	metrics, err := prometheusadapter.New(&prometheusadapter.Options{Registerer: metricsRegistry})
	require.NoError(t, err)
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{Observer: metrics.Observe})
	require.NoError(t, err)
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: anyone{}, Transactor: noTransactor{}, SameHostOnly: true})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity := &auth.Identity{Provider: "example", Subject: "ada", Active: true}
		engine.Handler().ServeHTTP(writer, request.WithContext(auth.WithIdentity(request.Context(), identity)))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	t.Cleanup(func() { _ = connection.Close() }) // the engine closes it first; the error carries nothing
	for index := range 50 {
		require.NoError(t, connection.WriteJSON(livewire.RequestFrame{ID: strconv.Itoa(index), Operation: livewire.Operation("bogus" + strconv.Itoa(index)), Target: "x" + strconv.Itoa(index)}))
	}
	for range 50 {
		var answer livewire.ResponseFrame
		require.NoError(t, connection.ReadJSON(&answer))
		require.Equal(t, http.StatusBadRequest, answer.Status)
	}
	require.Equal(t, 1, testutil.CollectAndCount(metricsRegistry, "livewire_units_total"), "50 invalid frames, one series")
}
