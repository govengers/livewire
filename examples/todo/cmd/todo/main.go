// Command todo is the livewire example application: todos of a workspace, served as REST, as
// a live WebSocket and to the generated clients, on gin, govengers/auth, sqlc and goose,
// with the change feed, history, export, presence, the audit and a govengers/jobs worker.
//
// Without DATABASE_URL it starts an embedded PostgreSQL for development; DEMO_PASSWORD seeds
// the users alice and carol (workspace north) and bob (workspace south):
//
//	SESSION_ENCRYPTION_KEY=$(openssl rand -base64 32) SESSION_INSECURE_COOKIE=true \
//	DEMO_PASSWORD=correct-horse-battery go run ./cmd/todo
//	curl -c cookies -d '{"identifier":"alice","password":"correct-horse-battery"}' localhost:8080/auth/password/login
//	curl -b cookies -X POST localhost:8080/api/v1/keys # an API key for the CLI: TODO_API_KEY
//	TODO_BASE_URL=http://localhost:8080/api/v1 TODO_API_KEY=... go run ./cmd/todo-cli todos list
//	curl localhost:9090/metrics
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/gin-gonic/gin"
	"github.com/govengers/auth/password"
	"github.com/govengers/auth/session"
	"github.com/govengers/config/v2"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/database"
)

const (
	shutdownBudget     = 20 * time.Second // the API's drain, within a 30 s termination grace period
	maximumConnections = 20
)

// configuration is every value the service reads from the environment (CFG-001).
type configuration struct {
	databaseURL           string
	embeddedPostgresPath  string
	embeddedPostgresPort  int
	address               string
	metricsAddress        string
	sessionKey            string
	insecureCookie        bool
	allowedOrigins        []string
	demoPassword          string
	developmentLogs       bool
	readinessDelaySeconds int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run bootstraps every layer in order (LAY-002) and serves until SIGTERM.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	values, err := loadConfiguration()
	if err != nil {
		return err
	}
	config.Print()
	gin.SetMode(gin.ReleaseMode) // gin's route listing is not the service's log
	emitter, err := newEmitter(values.developmentLogs)
	if err != nil {
		return err
	}
	sessionKey, err := session.ParseKey(values.sessionKey)
	if err != nil {
		return err
	}
	pool, closeDatabase, err := openDatabase(ctx, values)
	if err != nil {
		return err
	}
	defer closeDatabase()
	metrics := prometheus.NewRegistry() // the service's own registry (MET-002)
	metrics.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	users := password.NewMemoryStorage()
	application, err := newApplication(&applicationOptions{
		Pool: pool, Emitter: emitter, Metrics: metrics, Users: users,
		SessionKey: sessionKey, InsecureCookie: values.insecureCookie, AllowedOrigins: values.allowedOrigins,
	})
	if err != nil {
		return err
	}
	if values.demoPassword != "" {
		if err := seedDemo(ctx, application, values.demoPassword); err != nil {
			return err
		}
	}
	return serve(ctx, application, values)
}

func loadConfiguration() (*configuration, error) {
	values := &configuration{}
	err := config.LoadConfig([]config.Value{
		config.String("DATABASE_URL").Sensitive().Into(&values.databaseURL), // empty: an embedded PostgreSQL for development
		config.String("EMBEDDED_POSTGRES_PATH").Default("sandbox/postgres").Into(&values.embeddedPostgresPath),
		config.Int("EMBEDDED_POSTGRES_PORT").Default(5433).Into(&values.embeddedPostgresPort),
		config.String("LISTEN_ADDRESS").Default(":8080").Into(&values.address),
		config.String("METRICS_ADDRESS").Default(":9090").Into(&values.metricsAddress),          // internal listener (MET-001)
		config.String("SESSION_ENCRYPTION_KEY").NotEmpty().Sensitive().Into(&values.sessionKey), // openssl rand -base64 32
		config.Bool("SESSION_INSECURE_COOKIE").Default(false).Into(&values.insecureCookie),
		config.StringArray("ALLOWED_ORIGINS").Default(nil).Into(&values.allowedOrigins), // empty: the socket's own host only (D9)
		config.String("DEMO_PASSWORD").Sensitive().Into(&values.demoPassword),
		config.Bool("DEVELOPMENT_LOGS").Default(false).Into(&values.developmentLogs),
		config.Int("READINESS_DELAY_SECONDS").Default(5).Into(&values.readinessDelaySeconds), // the load balancer's notice; with the 20 s drain within a 30 s grace period
	})
	return values, err
}

// newEmitter writes the wide events as JSON lines, or for a terminal in development (LOG-013).
func newEmitter(development bool) (wideevent.Emitter, error) {
	if development {
		return wideevent.NewDevEmitter(&wideevent.DevEmitterOptions{Writer: os.Stderr})
	}
	return wideevent.NewJSONEmitter(&wideevent.JSONEmitterOptions{Writer: os.Stderr}), nil
}

// openDatabase opens the pool on DATABASE_URL, or on an embedded PostgreSQL without one, and
// migrates it; the returned function closes both.
func openDatabase(ctx context.Context, values *configuration) (*pgxpool.Pool, func(), error) {
	url, stopEmbedded := values.databaseURL, func() {}
	if url == "" {
		embedded := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Port(uint32(values.embeddedPostgresPort)). //nolint:gosec // G115: a TCP port fits a uint32
			DataPath(values.embeddedPostgresPath))
		if err := embedded.Start(); err != nil {
			return nil, nil, fmt.Errorf("start the embedded PostgreSQL: %w", err)
		}
		url = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres", values.embeddedPostgresPort)
		stopEmbedded = func() { _ = embedded.Stop() } // the process ends; a failed stop leaves the data for the next start
	}
	pool, err := database.NewPool(ctx, url, maximumConnections)
	if err != nil {
		stopEmbedded()
		return nil, nil, err
	}
	closeDatabase := func() {
		pool.Close()
		stopEmbedded()
	}
	if err := database.Migrate(ctx, pool); err != nil {
		closeDatabase()
		return nil, nil, err
	}
	return pool, closeDatabase, nil
}

// seedDemo creates the demo users unless they exist (AUTH-008, INT-007).
func seedDemo(ctx context.Context, application *application, demoPassword string) error {
	for _, user := range []struct{ name, workspace string }{{"alice", "north"}, {"carol", "north"}, {"bob", "south"}} {
		if _, err := authentication.SeedUser(ctx, application.users, application.accounts, authentication.Seed{Username: user.name, Password: demoPassword, Workspace: user.workspace}); err != nil {
			return fmt.Errorf("seed %s: %w", user.name, err)
		}
	}
	return nil
}

// serve runs the engine, the job worker, the API and the metrics listener until ctx ends,
// then drains (LAY-003): the API within shutdownBudget, the worker within its DrainTimeout,
// side by side, so both fit a 30 s termination grace period.
func serve(ctx context.Context, application *application, values *configuration) error {
	apiServer := newServer(values.address, application.router)
	metricsServer := newServer(values.metricsAddress, promhttp.HandlerFor(application.metrics, promhttp.HandlerOpts{}))
	group, groupContext := errgroup.WithContext(ctx)
	group.Go(recovering(func() error { return application.engine.Run(groupContext) }))
	group.Go(recovering(func() error { return application.worker.Run(groupContext) }))
	group.Go(recovering(func() error {
		if err := listen(groupContext, apiServer); err != nil {
			return err
		}
		api := drainer{ready: application.ready, readinessDelay: time.Duration(values.readinessDelaySeconds) * time.Second, budget: shutdownBudget, wait: wait}
		return api.drain(context.WithoutCancel(groupContext), apiServer, application.engine)
	}))
	group.Go(recovering(func() error {
		if err := listen(groupContext, metricsServer); err != nil {
			return err
		}
		metrics := drainer{budget: shutdownBudget, wait: wait}
		return metrics.drain(context.WithoutCancel(groupContext), metricsServer, nil)
	}))
	return group.Wait()
}

// httpServer is what drain stops: an *http.Server.
type httpServer interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// socketCloser is what drain ends the sockets with: the engine.
type socketCloser interface {
	Close(ctx context.Context) error
}

// drainer stops serving in order (LAY-003).
type drainer struct {
	ready          *atomic.Bool                                   // the readiness it withdraws; nil for none
	readinessDelay time.Duration                                  // how long the load balancer gets to notice
	budget         time.Duration                                  // the drain's one budget
	wait           func(ctx context.Context, delay time.Duration) // waits for delay or ctx
}

// wait waits for delay unless ctx ends first.
func wait(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// drain withdraws readiness and waits the readiness delay while the load balancer notices and
// stops sending; then, within one budget, the server stops accepting and waits for its
// requests, closes the connections still open when the budget is spent, and the sockets end
// with 1001. A nil sockets skips its step.
func (d drainer) drain(ctx context.Context, server httpServer, sockets socketCloser) error {
	if d.ready != nil {
		d.ready.Store(false)
		d.wait(ctx, d.readinessDelay)
	}
	bounded, cancel := context.WithTimeout(ctx, d.budget)
	defer cancel()
	err := server.Shutdown(bounded)
	if err != nil {
		err = errors.Join(err, server.Close())
	}
	if sockets != nil {
		err = errors.Join(err, sockets.Close(bounded))
	}
	return err
}

// newServer bounds what one client can hold (LAY-006); the socket is hijacked and keeps no
// deadline of the server's.
func newServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, MaxHeaderBytes: 64 << 10,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
}

// listen serves until ctx ends, or answers why the server failed.
func listen(ctx context.Context, server *http.Server) error {
	failed := make(chan error, 1)
	go func() { failed <- recovering(server.ListenAndServe)() }()
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		return nil
	}
}

// recovering turns a panic in work into an error for its errgroup (CON-001).
func recovering(work func() error) func() error {
	return func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v\n%s", recovered, debug.Stack())
			}
		}()
		return work()
	}
}
