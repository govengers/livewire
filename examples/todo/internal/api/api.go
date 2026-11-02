// Package api holds the payload types of the todo API (API-004): what the livewire registry
// answers and accepts, and so what the generated clients carry. The generated Go client
// imports this package; it imports nothing of the server.
package api

import (
	"time"

	"github.com/google/uuid"
)

// Priority is how urgent a todo is.
type Priority string

// The priorities.
const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// Priorities are every priority, in the order the generated clients list them.
func Priorities() []Priority { return []Priority{PriorityLow, PriorityNormal, PriorityHigh} }

// TodoItem is a todo as every read answers it; Version counts its changes (D34).
type TodoItem struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	Priority  Priority   `json:"priority"`
	Done      bool       `json:"done"`
	DueAt     *time.Time `json:"dueAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Version   int        `json:"version"`
}

// snippet:begin requests

// CreateTodoRequest creates a todo; ID is a UUID v7 the client may choose (D18), so an
// optimistic item and its retry carry the same id.
type CreateTodoRequest struct {
	ID       uuid.UUID  `json:"id,omitzero"`
	Title    string     `json:"title" validate:"required,max=200"`
	Priority Priority   `json:"priority,omitempty" validate:"omitempty,oneof=low normal high"`
	DueAt    *time.Time `json:"dueAt,omitempty"`
}

// UpdateTodoRequest changes the fields it carries; clearDueAt removes the due time.
type UpdateTodoRequest struct {
	Title      *string    `json:"title,omitempty" validate:"omitempty,min=1,max=200"`
	Priority   *Priority  `json:"priority,omitempty" validate:"omitempty,oneof=low normal high"`
	Done       *bool      `json:"done,omitempty"`
	DueAt      *time.Time `json:"dueAt,omitempty"`
	ClearDueAt bool       `json:"clearDueAt,omitempty"`
}

// TodoParameters name one todo.
type TodoParameters struct {
	ID uuid.UUID `path:"id"`
}

// snippet:end requests

// SummaryItem counts the workspace's todos and attachments.
type SummaryItem struct {
	Open        int `json:"open"`
	Done        int `json:"done"`
	Attachments int `json:"attachments"`
}

// ActivityItem is one line of the workspace's activity.
type ActivityItem struct {
	ID        uuid.UUID `json:"id"`
	TodoID    uuid.UUID `json:"todoId"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

// snippet:begin activity-parameters

// ActivityParameters narrow the activity to one todo.
type ActivityParameters struct {
	TodoID *uuid.UUID `query:"todoId"`
}

// snippet:end activity-parameters

// AttachmentItem is a file stored with a todo.
type AttachmentItem struct {
	ID          uuid.UUID `json:"id"`
	TodoID      uuid.UUID `json:"todoId"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

// KeyItem is a new API key; Key is shown once.
type KeyItem struct {
	ID        uuid.UUID `json:"id"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"createdAt"`
}
