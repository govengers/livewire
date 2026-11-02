package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/govengers/auth"
	"github.com/govengers/auth/password"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"

	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/controller"
	"github.com/govengers/livewire/examples/todo/internal/repository"
	"github.com/govengers/livewire/examples/todo/internal/service"
)

// The router as NewRouter builds it, on an engine without declarations and an authentication
// whose pool is never used; its readiness check panics as the test says.

type noIdentity struct{}

func (noIdentity) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return nil, auth.ErrUnauthenticated
}

type noTransactor struct{}

func (noTransactor) Begin(context.Context, livewire.TransactionDetails) (livewire.Transaction, error) {
	return nil, auth.ErrUnauthenticated
}

// newTestRouter answers the router, its metrics and what it printed.
func newTestRouter(t *testing.T, ready func(ctx context.Context) error) (*httptest.Server, *prometheus.Registry, *bytes.Buffer) {
	t.Helper()
	printed := &bytes.Buffer{}
	gin.DefaultErrorWriter = printed
	t.Cleanup(func() { gin.DefaultErrorWriter = gin.DefaultWriter })
	gin.SetMode(gin.ReleaseMode)
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: controller.BasePath})
	require.NoError(t, err)
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: noIdentity{}, Transactor: noTransactor{}, SameHostOnly: true})
	require.NoError(t, err)
	pool, err := pgxpool.New(t.Context(), "postgres://127.0.0.1:1/unused") // connects on first use, which never comes
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	accounts, err := repository.NewAccountRepository(&repository.AccountRepositoryOptions{Pool: pool})
	require.NoError(t, err)
	accountService, err := service.NewAccountService(&service.AccountServiceOptions{AccountRepository: accounts})
	require.NoError(t, err)
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	identities, err := authentication.New(&authentication.Options{Storage: password.NewMemoryStorage(), AccountService: accountService, EncryptionKey: key, InsecureCookie: true})
	require.NoError(t, err)
	metrics := prometheus.NewRegistry()
	router, err := NewRouter(&Options{
		Engine: engine, Authentication: identities, Metrics: metrics, Ready: ready,
		Emitter: wideevent.EmitterFunc(func(we *wideevent.Event) { fmt.Fprintln(printed, we.FieldsMap()) }),
	})
	require.NoError(t, err)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, metrics, printed
}

func withCredentials(t *testing.T, method, address string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, address, nil)
	require.NoError(t, err)
	request.Header.Set("Cookie", "auth_session=secret-session")
	request.Header.Set("X-API-Key", "todo_secret-key")
	return request
}

// TestTheRouterRecoversAPanicWithoutPrintingCredentials pins LOG-009 through NewRouter: a
// handler's panic answers the error body and prints neither the cookie nor the API key, and an
// abort reaches net/http, which drops the connection.
func TestTheRouterRecoversAPanicWithoutPrintingCredentials(t *testing.T) {
	var panicWith any = "the readiness check failed"
	server, _, printed := newTestRouter(t, func(context.Context) error { panic(panicWith) })

	response, err := server.Client().Do(withCredentials(t, http.MethodGet, server.URL+"/readyz"))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)

	panicWith = http.ErrAbortHandler
	response, err = server.Client().Do(withCredentials(t, http.MethodGet, server.URL+"/readyz"))
	if err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.NotContains(t, printed.String(), "secret-session")
	require.NotContains(t, printed.String(), "secret-key")
	require.Error(t, err, "the abort drops the connection (D60)")
}

// TestTheRequestMetricsKeepTheirLabelsBounded pins MET-004: a method outside the known set is
// "other", an unknown path "unmatched", a panic a 5xx, the socket's lifetime no request, and
// the gauge of requests in flight ends at zero.
func TestTheRequestMetricsKeepTheirLabelsBounded(t *testing.T) {
	server, metrics, _ := newTestRouter(t, func(context.Context) error { panic("the readiness check failed") })
	for _, request := range []*http.Request{
		withCredentials(t, "BREW", server.URL+"/coffee"),
		withCredentials(t, http.MethodGet, server.URL+"/readyz"),
		withCredentials(t, http.MethodGet, server.URL+SocketPath),
	} {
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	families, err := metrics.Gather()
	require.NoError(t, err)
	var series []string
	inFlight := -1.0
	for _, family := range families {
		if family.GetName() == "todo_http_requests_in_flight" {
			inFlight = family.GetMetric()[0].GetGauge().GetValue()
		}
		if family.GetName() != "todo_http_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			series = append(series, labels["route"]+" "+labels["method"]+" "+labels["status"])
		}
	}
	require.ElementsMatch(t, []string{"unmatched other 4xx", "/readyz GET 5xx"}, series)
	require.Zero(t, inFlight, "every request in flight ended, the panicking one included")
}
