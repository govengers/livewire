package main

import (
	"context"
	"errors"
	"net/http"

	authginadapter "github.com/govengers/auth/ginadapter"
	"github.com/govengers/auth/password"
	"github.com/govengers/jobs"
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
}

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
	jobStore, err := jobspostgres.New(&jobspostgres.Options{Pool: options.Pool})
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
	router, err := server.NewRouter(&server.Options{Engine: engine, Authentication: identities, Emitter: options.Emitter, Ready: options.Pool.Ping})
	if err != nil {
		return nil, err
	}
	return &application{
		registry: engine.Registry(), engine: engine, worker: worker, router: router,
		metrics: options.Metrics, users: options.Users, accounts: accountService, jobStore: jobStore,
	}, nil
}

func newStores(pool *pgxpool.Pool) (*stores, error) {
	var created stores
	var errs [6]error
	created.transactor, errs[0] = postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
	created.filters, errs[1] = postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: pool})
	created.idempotency, errs[2] = postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: pool})
	created.audit, errs[3] = postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: pool})
	created.presences, errs[4] = postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: pool})
	created.listener, errs[5] = postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		// the change feed's dedicated connection, configured like the pool's
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			return pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
		},
	})
	return &created, errors.Join(errs[:]...)
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
		Store:      jobStore,
		Queues:     map[string]jobs.QueueOptions{model.QueueTodos: {Concurrency: 4}, model.QueueMaintenance: {Concurrency: 1}},
		Middleware: []jobs.Middleware{wideEvents},
		Observer:   jobMetrics,
		Schedules:  controller.Schedules(),
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
