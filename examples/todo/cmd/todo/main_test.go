package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/gin-gonic/gin"
	"github.com/govengers/auth/password"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire/client"

	"github.com/govengers/livewire/examples/todo/client/todoclient"
	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/database"
)

// The tests run the application as main wires it (newApplication) on an embedded PostgreSQL
// (TST-002): one server per test binary, a database per test cloned from a migrated template.

// testPassword is every seeded user's password.
const testPassword = "correct horse battery staple"

var postgres struct {
	port     int
	mutex    sync.Mutex // CREATE DATABASE ... TEMPLATE refuses a template another clone reads
	sequence atomic.Int64
}

const templateDatabase = "todo_template"

func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return failed("pick a port", err)
	}
	postgres.port = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return failed("free the port", err)
	}
	runtimePath, err := os.MkdirTemp("", "todo-example-")
	if err != nil {
		return failed("create the runtime directory", err)
	}
	defer func() { _ = os.RemoveAll(runtimePath) }() // a temporary directory; a leftover is harmless
	server := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(postgres.port)). //nolint:gosec // G115: a TCP port fits a uint32
		RuntimePath(runtimePath).
		// ponytail: the binaries are extracted once per machine (xz under -race takes a minute); a run
		// killed mid-extraction leaves a broken directory, which deleting it repairs.
		BinariesPath(filepath.Join(os.TempDir(), "livewire-todo-postgres-binaries")).
		Logger(io.Discard))
	if err := server.Start(); err != nil {
		return failed("start embedded postgres", err)
	}
	defer func() { _ = server.Stop() }()      // the result is decided; a failed stop changes nothing
	http.DefaultClient.CloseIdleConnections() // the binaries' download would outlive the tests
	if err := migrateTemplate(); err != nil {
		return failed("migrate the template database", err)
	}
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			return failed("goroutines leaked", err)
		}
	}
	return code
}

func failed(what string, err error) int {
	fmt.Fprintf(os.Stderr, "todo tests: %s: %v\n", what, err)
	return 1
}

func databaseURL(name string) string {
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s", postgres.port, name)
}

// migrateTemplate runs the application's migrations on the template database.
func migrateTemplate() error {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL("postgres"))
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }() // the template exists or the error is returned
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDatabase); err != nil {
		return err
	}
	pool, err := database.NewPool(ctx, databaseURL(templateDatabase), 4)
	if err != nil {
		return err
	}
	defer pool.Close()
	return database.Migrate(ctx, pool)
}

// newPool clones the template into a database of the test's own and drops it after.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	name := "test_" + strconv.FormatInt(postgres.sequence.Add(1), 10)
	admin, err := pgx.Connect(t.Context(), databaseURL("postgres"))
	require.NoError(t, err)
	postgres.mutex.Lock()
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+name+" TEMPLATE "+templateDatabase)
	postgres.mutex.Unlock()
	require.NoError(t, admin.Close(t.Context()))
	require.NoError(t, err)
	pool, err := database.NewPool(t.Context(), databaseURL(name), maximumConnections)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, databaseURL("postgres"))
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
		require.NoError(t, admin.Close(ctx))
	})
	return pool
}

// testApplication is the application served on a test server, with the seeded users alice and
// carol in the workspace north and bob in south, and the engine running; its job worker does
// not run: a test drains it (jobstest.Drain).
type testApplication struct {
	*application
	server *httptest.Server
	pool   *pgxpool.Pool
}

func newTestApplication(t *testing.T) *testApplication {
	t.Helper()
	pool := newPool(t)
	users := password.NewMemoryStorage()
	application, err := newApplication(&applicationOptions{
		Pool: pool, Emitter: wideevent.EmitterFunc(func(*wideevent.Event) {}), Metrics: prometheus.NewRegistry(), Users: users,
		SessionKey: sessionKey(t), InsecureCookie: true,
	})
	require.NoError(t, err)
	for _, user := range []struct{ name, workspace string }{{"alice", "north"}, {"carol", "north"}, {"bob", "south"}} {
		require.NoError(t, authentication.SeedUser(t.Context(), users, application.accounts, authentication.Seed{Username: user.name, Password: testPassword, Workspace: user.workspace}))
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
	return &testApplication{application: application, server: server, pool: pool}
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
	todos, err := todoclient.New(&client.Options{BaseURL: a.baseURL(), APIKey: a.apiKey(t, username)})
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
