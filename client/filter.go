package client

import (
	"encoding/json"
	"time"

	"github.com/govengers/livewire/query"
)

// Node is one node of a filter document over the fields Field names (D12): a condition,
// or and, or or not over nodes. Field is the generated field type of one resource, so the
// nodes of two resources do not mix. Build conditions with the generated
// <Resource><Field><Operator> functions (or Condition) and combine them with And, Or and
// Not; the zero Node is no valid node.
type Node[Field ~string] struct {
	value any
}

// MarshalJSON writes the node as the filter document spells it.
func (n Node[Field]) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.value)
}

type condition struct {
	Field    string         `json:"field"`
	Operator query.Operator `json:"op"`
	Value    any            `json:"value"`
}

// Condition is the condition `{field, op, value}`. The generated functions call it with
// the operand type of the field and operator; called directly, the server checks the
// operator and the value (400 invalid_filter).
func Condition[Field ~string](field Field, operator query.Operator, value any) Node[Field] {
	return Node[Field]{value: condition{Field: string(field), Operator: operator, Value: value}}
}

// And matches the rows every node matches.
func And[Field ~string](nodes ...Node[Field]) Node[Field] {
	return Node[Field]{value: map[string][]Node[Field]{"and": append([]Node[Field]{}, nodes...)}}
}

// Or matches the rows any node matches.
func Or[Field ~string](nodes ...Node[Field]) Node[Field] {
	return Node[Field]{value: map[string][]Node[Field]{"or": append([]Node[Field]{}, nodes...)}}
}

// Not matches the rows node does not match.
func Not[Field ~string](node Node[Field]) Node[Field] {
	return Node[Field]{value: map[string]Node[Field]{"not": node}}
}

// Time is the operand of a condition on a time field (D12): an instant, or a time relative
// to the database clock of the read, which makes a subscription re-run on the safety tick.
type Time struct {
	instant  time.Time
	relative string
}

// At is the instant, written as RFC 3339 with nanoseconds.
func At(instant time.Time) Time {
	return Time{instant: instant}
}

// Relative is a time relative to the read, such as "-7d" or "2h" (the units of D12); the
// server checks the form.
func Relative(offset string) Time {
	return Time{relative: offset}
}

// MarshalJSON writes the instant as a string, a relative time as `{"relative": offset}`.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.relative != "" {
		return json.Marshal(map[string]string{"relative": t.relative})
	}
	return json.Marshal(t.instant.Format(time.RFC3339Nano))
}
