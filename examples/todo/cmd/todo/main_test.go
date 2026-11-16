package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/govengers/auth/password"
	"github.com/govengers/postgrestest"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"

	"github.com/govengers/livewire/examples/todo/client/todoclient"
	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/database"
	"github.com/govengers/livewire/examples/todo/internal/server"
)

// The tests run the application as main wires it (newApplication) on an embedded PostgreSQL
// (TST-002): one server per test binary, a database per test copied from a migrated template.

// testPassword is every seeded user's password.
const testPassword = "correct horse battery staple"

// postgres is the test binary's one PostgreSQL (govengers/postgrestest). No Fingerprint: the
// migrations are the application's goose files and Go functions, and a fingerprint matters only on
// a long-lived external server (POSTGRESTEST_URL), where an empty one builds a template per run.
var postgres = postgrestest.New(&postgrestest.Options{Migrate: migrateTemplate})

func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	goleak.VerifyTestMain(postgres.Main(m))
}

// migrateTemplate runs the application's migrations on the template database; its pool is closed
// before it returns, as postgrestest requires.
func migrateTemplate(ctx context.Context, connection *pgx.Conn) error {
	pool, err := database.NewPool(ctx, connection.Config().ConnString(), 4, noop.NewTracerProvider())
	if err != nil {
		return err
	}
	defer pool.Close()
	return database.Migrate(ctx, pool)
}

// newPool is a pool on a copy of the template of the test's own, as main opens it, dropped after
// the test.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := database.NewPool(t.Context(), postgres.Database(t), maximumConnections, noop.NewTracerProvider())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// testApplication is the application served on a test server, with the seeded users alice and
// carol in the workspace north and bob in south, and the engine running; its job worker does
// not run: a test drains it (jobstest.Drain).
type testApplication struct {
	*application
	server *httptest.Server
	pool   *pgxpool.Pool
	events *recordedEvents
}

func newTestApplication(t *testing.T) *testApplication {
	t.Helper()
	return newTestApplicationWith(t, nil)
}

// newTestApplicationWith is newTestApplication with the options change makes (the agents).
func newTestApplicationWith(t *testing.T, change func(options *applicationOptions)) *testApplication {
	t.Helper()
	pool := newPool(t)
	users := password.NewMemoryStorage()
	events := &recordedEvents{}
	options := &applicationOptions{
		Pool: pool, Emitter: events, Metrics: prometheus.NewRegistry(), Traces: noop.NewTracerProvider(), Users: users,
		SessionKey: sessionKey(t), InsecureCookie: true,
	}
	if change != nil {
		change(options)
	}
	application, err := newApplication(options)
	require.NoError(t, err)
	for _, user := range []struct{ name, workspace string }{{"alice", "north"}, {"carol", "north"}, {"bob", "south"}} {
		_, err := authentication.SeedUser(t.Context(), users, application.accounts, authentication.Seed{Username: user.name, Password: testPassword, Workspace: user.workspace})
		require.NoError(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan error, 1)
	go func() { running <- application.engine.Run(ctx) }()
	server := httptest.NewServer(application.router)
	t.Cleanup(func() {
		closing, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		require.NoError(t, application.engine.Close(closing))
		server.Close()
		cancel()
		require.NoError(t, <-running)
	})
	return &testApplication{application: application, server: server, pool: pool, events: events}
}

// sessionKey is a random session encryption key.
func sessionKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

// baseURL is the API's URL with its base path.
func (a *testApplication) baseURL() string { return a.server.URL + "/api/v1" }

// login signs the user in with the password route and answers a client that sends the
// session cookie.
func (a *testApplication) login(t *testing.T, username string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := &http.Client{Jar: jar}
	body, err := json.Marshal(map[string]string{"identifier": username, "password": testPassword})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/auth/password/login", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := browser.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	return browser
}

// apiKey signs the user in and creates an API key with the session (POST /api/v1/keys).
func (a *testApplication) apiKey(t *testing.T, username string) string {
	t.Helper()
	browser := a.login(t, username)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.baseURL()+"/keys", nil)
	require.NoError(t, err)
	response, err := browser.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var created struct {
		Key string `json:"key"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&created))
	return created.Key
}

// client is the generated Go client with the user's API key.
func (a *testApplication) client(t *testing.T, username string) *todoclient.Client {
	t.Helper()
	// snippet:begin go-client-new
	todos, err := todoclient.New(&client.Options{BaseURL: a.baseURL(), APIKey: a.apiKey(t, username)})
	// snippet:end go-client-new
	require.NoError(t, err)
	return todos
}

// bearer authenticates a gate's requests with an API key.
func bearer(key string) func(request *http.Request) {
	return func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+key) }
}

// session authenticates a gate's requests with the session cookie of a login.
func (a *testApplication) session(t *testing.T, username string) func(request *http.Request) {
	t.Helper()
	server, err := url.Parse(a.server.URL)
	require.NoError(t, err)
	cookies := a.login(t, username).Jar.Cookies(server)
	require.NotEmpty(t, cookies)
	return func(request *http.Request) {
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
	}
}

// recordedEvents keeps every wide event the application emitted.
type recordedEvents struct {
	mutex  sync.Mutex
	events []map[string]any
}

func (r *recordedEvents) Emit(we *wideevent.Event) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.events = append(r.events, we.FieldsMap())
}

// named are the fields of the events of a name.
func (r *recordedEvents) named(name string) []map[string]any {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	var named []map[string]any
	for _, fields := range r.events {
		if fields["name"] == name {
			named = append(named, fields)
		}
	}
	return named
}

// rawSocket is a socket of the application for the ops the Go client has none for (join) and
// for a session cookie, which the Go client does not send.
type rawSocket struct {
	connection *websocket.Conn
	sequence   int
}

// dial opens the socket with the request's authentication.
func (a *testApplication) dial(t *testing.T, authenticate func(request *http.Request)) *rawSocket {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.server.URL, nil)
	require.NoError(t, err)
	authenticate(request)
	request.Header.Set("Origin", a.server.URL)
	connection, response, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(a.server.URL, "http")+server.SocketPath, request.Header)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	t.Cleanup(func() { _ = connection.Close() }) // closed already when the server closed it
	return &rawSocket{connection: connection}
}

// pushWait bounds the wait for a push: far below the socket's 60 s re-validation tick.
const pushWait = 3 * time.Second

// push answers the next push of a subscription within pushWait, skipping other frames.
func (s *rawSocket) push(t *testing.T, subscription string) livewire.PushFrame {
	t.Helper()
	require.NoError(t, s.connection.SetReadDeadline(time.Now().Add(pushWait)))
	for {
		var frame livewire.PushFrame
		require.NoError(t, s.connection.ReadJSON(&frame), "no push of %s within %s", subscription, pushWait)
		if frame.Subscription == subscription {
			return frame
		}
	}
}

// request sends a frame and answers its response, skipping pushes; it fails the test on a
// closed socket, unless closed is set, which then receives the close code.
func (s *rawSocket) request(t *testing.T, frame livewire.RequestFrame) (livewire.ResponseFrame, int) {
	t.Helper()
	s.sequence++
	frame.ID = strconv.Itoa(s.sequence)
	require.NoError(t, s.connection.SetWriteDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, s.connection.WriteJSON(frame))
	require.NoError(t, s.connection.SetReadDeadline(time.Now().Add(10*time.Second)))
	for {
		var response livewire.ResponseFrame
		if err := s.connection.ReadJSON(&response); err != nil {
			var closed *websocket.CloseError
			require.ErrorAs(t, err, &closed)
			return livewire.ResponseFrame{}, closed.Code
		}
		if response.ID == frame.ID {
			return response, 0
		}
	}
}
