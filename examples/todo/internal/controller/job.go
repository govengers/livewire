package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/govengers/jobs"
	"github.com/govengers/livewire/postgresadapter"

	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/service"
)

// The periods of the maintenance schedules: the sweeps hourly, the audit partitions daily
// (AuditStore.Maintain needs to run at least monthly).
const (
	sweepInterval    = time.Hour
	maintainInterval = 24 * time.Hour
)

// JobController translates the jobs into calls (JOB-008): the completion job into the
// TodoService, the maintenance jobs into livewire's stores, which the engine never maintains
// itself (D54, D32), so one replica runs them, not every engine.
type JobController struct {
	todos       *service.TodoService
	filters     *postgresadapter.FilterStore
	idempotency *postgresadapter.IdempotencyStore
	audit       *postgresadapter.AuditStore
}

// JobControllerOptions are the dependencies of a JobController; every field is required.
type JobControllerOptions struct {
	TodoService      *service.TodoService
	FilterStore      *postgresadapter.FilterStore
	IdempotencyStore *postgresadapter.IdempotencyStore
	AuditStore       *postgresadapter.AuditStore
}

// NewJobController fails without one of its dependencies.
func NewJobController(options *JobControllerOptions) (*JobController, error) {
	if options == nil || options.TodoService == nil || options.FilterStore == nil || options.IdempotencyStore == nil || options.AuditStore == nil {
		return nil, errors.New("controller: JobControllerOptions needs the TodoService and the three stores")
	}
	return &JobController{todos: options.TodoService, filters: options.FilterStore, idempotency: options.IdempotencyStore, audit: options.AuditStore}, nil
}

// Schedules are the maintenance jobs every worker enqueues once per period with every other
// worker (JOB-009); pass them as WorkerOptions.Schedules.
func Schedules() []jobs.Schedule {
	return []jobs.Schedule{
		{Name: model.FilterSweepKind.Name(), Next: jobs.Every(sweepInterval), Job: model.FilterSweepKind.New(struct{}{}, nil)},
		{Name: model.IdempotencySweepKind.Name(), Next: jobs.Every(sweepInterval), Job: model.IdempotencySweepKind.New(struct{}{}, nil)},
		{Name: model.AuditMaintainKind.Name(), Next: jobs.Every(maintainInterval), Job: model.AuditMaintainKind.New(struct{}{}, nil)},
	}
}

// Register registers the handler of every job kind on the worker.
func (c *JobController) Register(worker *jobs.Worker) error {
	return errors.Join(
		jobs.Handle(worker, model.TodoCompletedKind, c.recordCompletion),
		jobs.Handle(worker, model.FilterSweepKind, maintenance(func(ctx context.Context) error {
			_, err := c.filters.Sweep(ctx)
			return err
		})),
		jobs.Handle(worker, model.IdempotencySweepKind, maintenance(func(ctx context.Context) error {
			_, err := c.idempotency.Sweep(ctx)
			return err
		})),
		jobs.Handle(worker, model.AuditMaintainKind, maintenance(c.audit.Maintain)),
	)
}

func (c *JobController) recordCompletion(ctx context.Context, job *jobs.Job[model.TodoCompletedPayload]) error {
	if err := c.todos.RecordCompletion(ctx, job.ID, job.Payload); err != nil {
		return fmt.Errorf("record the completion of todo %s: %w", job.Payload.TodoID, err)
	}
	return nil
}

// maintenance is the handler of a maintenance job: the work itself, which is idempotent.
func maintenance(work func(ctx context.Context) error) func(ctx context.Context, job *jobs.Job[struct{}]) error {
	return func(ctx context.Context, _ *jobs.Job[struct{}]) error {
		return work(ctx)
	}
}
