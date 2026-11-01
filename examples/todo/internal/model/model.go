// Package model holds the domain types and errors every layer of the example shares.
package model

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/govengers/jobs"
)

// The domain errors; the repository maps the driver's errors to them once (DAT-004).
var (
	ErrNotFound = errors.New("not found")
	ErrNoMember = errors.New("the user is in no workspace")
	ErrConflict = errors.New("conflict")
)

// Priority is how urgent a todo is.
type Priority string

// The priorities.
const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// Todo is one todo of a workspace.
type Todo struct {
	ID        uuid.UUID
	Title     string
	Priority  Priority
	Done      bool
	DueAt     *time.Time
	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int
}

// Equal reports whether two todos carry the same values.
func (t Todo) Equal(other Todo) bool {
	sameDueAt := (t.DueAt == nil && other.DueAt == nil) || (t.DueAt != nil && other.DueAt != nil && t.DueAt.Equal(*other.DueAt))
	t.DueAt, other.DueAt = nil, nil
	return sameDueAt && t == other
}

// NewTodo is a todo to create; ID is the client's (D18) or a new UUID v7.
type NewTodo struct {
	ID       uuid.UUID
	Title    string
	Priority Priority
	DueAt    *time.Time
}

// TodoChange is a partial update: a nil field keeps its value; ClearDueAt removes the due
// time (DAT-012).
type TodoChange struct {
	Title      *string
	Priority   *Priority
	Done       *bool
	DueAt      *time.Time
	ClearDueAt bool
}

// Apply returns the todo with the change applied.
func (c TodoChange) Apply(todo Todo) Todo {
	if c.Title != nil {
		todo.Title = *c.Title
	}
	if c.Priority != nil {
		todo.Priority = *c.Priority
	}
	if c.Done != nil {
		todo.Done = *c.Done
	}
	if c.DueAt != nil {
		todo.DueAt = c.DueAt
	}
	if c.ClearDueAt {
		todo.DueAt = nil
	}
	return todo
}

// Completes reports whether the change marks an open todo done, which enqueues the
// completion job.
func (c TodoChange) Completes(todo Todo) bool {
	return !todo.Done && c.Done != nil && *c.Done
}

// Summary counts a workspace's todos and attachments.
type Summary struct {
	Open        int
	Done        int
	Attachments int
}

// Attachment is a file stored with a todo.
type Attachment struct {
	ID          uuid.UUID
	TodoID      uuid.UUID
	Filename    string
	ContentType string
	Size        int64
	CreatedAt   time.Time
}

// Activity is one line of a workspace's activity, which the completion job writes.
type Activity struct {
	ID        uuid.UUID
	TodoID    uuid.UUID
	Message   string
	CreatedAt time.Time
}

// APIKey is a key the CLI and the Go client authenticate with (D20); Key is shown once.
type APIKey struct {
	ID        uuid.UUID
	Key       string
	CreatedAt time.Time
}

// Member is who a request acts as: the user, its name and its workspace, the tenant (D42).
type Member struct {
	UserID    uuid.UUID
	Username  string
	Workspace string
}

// TodoCompletedPayload is the payload of the job a completed todo enqueues (JOB-006).
type TodoCompletedPayload struct {
	TodoID    uuid.UUID `json:"todoId"`
	Workspace string    `json:"workspace"`
	Title     string    `json:"title"`
	Username  string    `json:"username"`
}

// TodoCompletedKind is the job that records a completion in the workspace's activity: the
// mutation enqueues it in its own transaction (D41, JOB-007).
var TodoCompletedKind = jobs.NewKind[TodoCompletedPayload]("todo.completed", &jobs.KindOptions{Queue: QueueTodos, MaximumAttempts: 5})

// The maintenance jobs of livewire's stores, which the application schedules (D54, D32).
var (
	FilterSweepKind      = jobs.NewKind[struct{}]("livewire.filters.sweep", &jobs.KindOptions{Queue: QueueMaintenance})
	IdempotencySweepKind = jobs.NewKind[struct{}]("livewire.idempotency.sweep", &jobs.KindOptions{Queue: QueueMaintenance})
	AuditMaintainKind    = jobs.NewKind[struct{}]("livewire.audit.maintain", &jobs.KindOptions{Queue: QueueMaintenance})
)

// The job queues.
const (
	QueueTodos       = "todos"
	QueueMaintenance = "maintenance"
)
