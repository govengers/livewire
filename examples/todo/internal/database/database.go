// Package database opens the pool and migrates the schema with goose (DAT-005): the
// application's tables in migrations/*.sql, livewire's SQL (D22) and the jobs tables of
// govengers/jobs in Go migrations, since both libraries ship their DDL as Go constants.
package database

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/govengers/jobs/postgresadapter"
	livewirepostgres "github.com/govengers/livewire/postgresadapter"
)

//go:embed migrations/*.sql
var migrations embed.FS

// NewPool opens the pool with an explicit size (DAT-020).
func NewPool(ctx context.Context, url string, maximumConnections int32) (*pgxpool.Pool, error) {
	configuration, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse the database URL: %w", err)
	}
	configuration.MaxConns = maximumConnections
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}
	return pool, nil
}

// RegisterPoolMetrics exports the pool's connections as gauges (MET-006): acquired, idle and
// total connections, the maximum, and the acquires that had to wait.
func RegisterPoolMetrics(registerer prometheus.Registerer, pool *pgxpool.Pool) error {
	gauge := func(name, help string, value func(*pgxpool.Stat) float64) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: "todo", Subsystem: "database_pool", Name: name, Help: help},
			func() float64 { return value(pool.Stat()) })
	}
	for _, collector := range []prometheus.Collector{
		gauge("acquired_connections", "Connections in use.", func(stat *pgxpool.Stat) float64 { return float64(stat.AcquiredConns()) }),
		gauge("idle_connections", "Idle connections.", func(stat *pgxpool.Stat) float64 { return float64(stat.IdleConns()) }),
		gauge("total_connections", "Open connections.", func(stat *pgxpool.Stat) float64 { return float64(stat.TotalConns()) }),
		gauge("maximum_connections", "The pool's size.", func(stat *pgxpool.Stat) float64 { return float64(stat.MaxConns()) }),
		gauge("empty_acquires", "Acquires that waited for a connection, since the start.", func(stat *pgxpool.Stat) float64 { return float64(stat.EmptyAcquireCount()) }),
	} {
		if err := registerer.Register(collector); err != nil {
			return fmt.Errorf("pool metrics: %w", err)
		}
	}
	return nil
}

// Migrate applies every migration that has not run yet, under goose's session lock, so two
// replicas starting at once migrate one after the other.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	database := stdlib.OpenDBFromPool(pool)
	defer func() { _ = database.Close() }() // the pool stays open; closing the wrapper only releases it
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, database, files, goose.WithSessionLocker(locker), goose.WithGoMigrations(
		goose.NewGoMigration(2, &goose.GoFunc{RunTx: livewireSchema}, nil),
		goose.NewGoMigration(3, &goose.GoFunc{RunTx: execute(postgresadapter.MigrationV1)}, nil),
	))
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// livewireSchema is livewire's SQL: the trigger function, the triggers that notify the change
// feed, partitioned by the workspace, the tenant (D25, D42), the history of todos (D34) and
// the tables of the stored filters, the idempotency records, the audit and the presences.
func livewireSchema(ctx context.Context, transaction *sql.Tx) error {
	history, err := livewirepostgres.HistorySQL("todos")
	if err != nil {
		return err
	}
	return execute(
		livewirepostgres.TriggerFunctionSQL,
		livewirepostgres.PartitionedTriggerSQL("todos", "workspace"),
		livewirepostgres.PartitionedTriggerSQL("attachments", "workspace"),
		livewirepostgres.PartitionedTriggerSQL("activity", "workspace"),
		history,
		livewirepostgres.FiltersSQL,
		livewirepostgres.IdempotencySQL,
		livewirepostgres.AuditSQL,
		livewirepostgres.PresenceSQL,
	)(ctx, transaction)
}

// execute is a migration that runs the statements in order.
func execute(statements ...string) func(ctx context.Context, transaction *sql.Tx) error {
	return func(ctx context.Context, transaction *sql.Tx) error {
		for _, statement := range statements {
			if _, err := transaction.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}
}
