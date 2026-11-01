package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	authginadapter "github.com/govengers/auth/ginadapter"
	"github.com/govengers/auth/password"
	"github.com/govengers/jobs"
	jobsauth "github.com/govengers/jobs/authadapter"
	jobspostgres "github.com/govengers/jobs/postgresadapter"
	jobsprometheus "github.com/govengers/jobs/prometheusadapter"
	"github.com/govengers/jobs/wideeventadapter"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/prometheusadapter"

	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/controller"
	"github.com/govengers/livewire/examples/todo/internal/database"
	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/repository"
	"github.com/govengers/livewire/examples/todo/internal/server"
	"github.com/govengers/livewire/examples/todo/internal/service"
)

// applicationOptions is what the application is built from: the migrated pool, the wide event
// emitter, the metrics registry, the users' storage and the session and socket settings.
type applicationOptions struct {
	Pool           *pgxpool.Pool
	Emitter        wideevent.Emitter
	Metrics        *prometheus.Registry
	Users          password.Storage
	SessionKey     []byte
	InsecureCookie bool
	AllowedOrigins []string
}

// application is the wired service: what serve runs and the tests drive.
type application struct {
	registry *livewire.Registry
	engine   *livewire.Engine
	worker   *jobs.Worker
	router   http.Handler
	metrics  *prometheus.Registry
	users    password.Storage
	accounts *service.AccountService
	jobStore *jobspostgres.Store
	ready    *atomic.Bool // the readiness, true until the drain starts
}

// errDraining is the readiness of a server that drains: it takes no new requests.
var errDraining = errors.New("the server is draining")

// stores are livewire's Postgres adapters (D3).
type stores struct {
	transactor  *postgresadapter.Transactor
	filters     *postgresadapter.FilterStore
	idempotency *postgresadapter.IdempotencyStore
	audit       *postgresadapter.AuditStore
	presences   *postgresadapter.PresenceStore
	listener    *postgresadapter.Listener
}

// newApplication wires repositories, stores, services, the registry and the engine, the job
// worker and the router, in this order (LAY-002).
func newApplication(options *applicationOptions) (*application, error) {
	jobStore, err := jobspostgres.New(&jobspostgres.Options{Pool: options.Pool, Propagators: []jobs.Propagator{jobsauth.Identity()}})
	if err != nil {
		return nil, err
	}
	todoRepository, err := repository.NewTodoRepository(&repository.TodoRepositoryOptions{Pool: options.Pool, JobStore: jobStore})
	if err != nil {
		return nil, err
	}
	accountRepository, err := repository.NewAccountRepository(&repository.AccountRepositoryOptions{Pool: options.Pool})
	if err != nil {
		return nil, err
	}
	livewireStores, err := newStores(options.Pool)
	if err != nil {
		return nil, err
	}
	todoService, err := service.NewTodoService(&service.TodoServiceOptions{TodoRepository: todoRepository})
	if err != nil {
		return nil, err
	}
	accountService, err := service.NewAccountService(&service.AccountServiceOptions{AccountRepository: accountRepository})
	if err != nil {
		return nil, err
	}
	identities, err := authentication.New(&authentication.Options{
		Storage: options.Users, AccountService: accountService, EncryptionKey: options.SessionKey,
		InsecureCookie: options.InsecureCookie, Observer: authginadapter.Observe,
	})
	if err != nil {
		return nil, err
	}
	engine, err := newEngine(options, livewireStores, identities, todoService, accountService)
	if err != nil {
		return nil, err
	}
	worker, err := newWorker(options, jobStore, livewireStores, todoService)
	if err != nil {
		return nil, err
	}
	if err := database.RegisterPoolMetrics(options.Metrics, options.Pool); err != nil {
		return nil, err
	}
	ready := &atomic.Bool{}
	ready.Store(true)
	router, err := server.NewRouter(&server.Options{
		Engine: engine, Authentication: identities, Emitter: options.Emitter, Metrics: options.Metrics,
		Ready: func(ctx context.Context) error {
			if !ready.Load() {
				return errDraining
			}
			return options.Pool.Ping(ctx)
		},
	})
	if err != nil {
		return nil, err
	}
	return &application{
		registry: engine.Registry(), engine: engine, worker: worker, router: router,
		metrics: options.Metrics, users: options.Users, accounts: accountService, jobStore: jobStore, ready: ready,
	}, nil
}

func newStores(pool *pgxpool.Pool) (*stores, error) {
	var created stores
	var failures [6]error
	created.transactor, failures[0] = postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
	created.filters, failures[1] = postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: pool})
	created.idempotency, failures[2] = postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: pool})
	created.audit, failures[3] = postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: pool})
	created.presences, failures[4] = postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: pool})
	created.listener, failures[5] = postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		// the change feed's dedicated connection, configured like the pool's
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			return pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
		},
	})
	return &created, errors.Join(failures[:]...)
}

// newEngine builds the registry with its Observer (Prometheus and wide events) and the
// engine on livewire's Postgres stores.
func newEngine(options *applicationOptions, livewireStores *stores, identities *authentication.Authentication, todoService *service.TodoService, accountService *service.AccountService) (*livewire.Engine, error) {
	metrics, err := prometheusadapter.New(&prometheusadapter.Options{Registerer: options.Metrics, Namespace: "todo"})
	if err != nil {
		return nil, err
	}
	registry, err := controller.NewRegistry(&controller.RegistryOptions{
		TodoService: todoService, AccountService: accountService, Observer: server.NewObserver(metrics, options.Emitter),
	})
	if err != nil {
		return nil, err
	}
	return livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, ChangeFeed: livewireStores.listener, IdentitySource: identities, Transactor: livewireStores.transactor,
		FilterStore: livewireStores.filters, IdempotencyStore: livewireStores.idempotency, AuditStore: livewireStores.audit,
		PresenceStore: livewireStores.presences, AllowedOrigins: options.AllowedOrigins, SameHostOnly: len(options.AllowedOrigins) == 0,
	})
}

// newWorker builds the job worker with the completion and maintenance handlers, one wide
// event per attempt and the queue metrics (JOB-010).
func newWorker(options *applicationOptions, jobStore *jobspostgres.Store, livewireStores *stores, todoService *service.TodoService) (*jobs.Worker, error) {
	wideEvents, err := wideeventadapter.NewMiddleware(&wideeventadapter.Options{Emitter: options.Emitter})
	if err != nil {
		return nil, err
	}
	queues := []string{model.QueueTodos, model.QueueMaintenance}
	jobMetrics, err := jobsprometheus.New(&jobsprometheus.Options{Registerer: options.Metrics, Namespace: "todo", DepthSource: jobStore, Queues: queues})
	if err != nil {
		return nil, err
	}
	worker, err := jobs.NewWorker(&jobs.WorkerOptions{
		Store:        jobStore,
		Propagators:  []jobs.Propagator{jobsauth.Identity()}, // a job a user started runs as that user (JOB-011)
		DrainTimeout: shutdownBudget,
		Queues:       map[string]jobs.QueueOptions{model.QueueTodos: {Concurrency: 4}, model.QueueMaintenance: {Concurrency: 1}},
		Middleware:   []jobs.Middleware{wideEvents},
		Observer:     jobMetrics,
		Schedules:    controller.Schedules(),
	})
	if err != nil {
		return nil, err
	}
	jobController, err := controller.NewJobController(&controller.JobControllerOptions{
		TodoService: todoService, FilterStore: livewireStores.filters, IdempotencyStore: livewireStores.idempotency, AuditStore: livewireStores.audit,
	})
	if err != nil {
		return nil, err
	}
	return worker, jobController.Register(worker)
}
