package livewire

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/govengers/livewire/query"
)

// Redact strips the fields whose Read check refuses the identity in ctx from raw, which is
// the marshaled list of items or one marshaled item (D39). Fields that are visible or absent
// from the JSON are left alone; a raw value without guarded fields is returned as it is,
// otherwise the result is re-marshaled with its keys sorted.
func Redact(ctx context.Context, fields query.Fields, raw json.RawMessage) (json.RawMessage, error) {
	hidden := hiddenFields(ctx, fields)
	if len(hidden) == 0 {
		return raw, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		for _, item := range items {
			for name := range hidden {
				delete(item, name)
			}
		}
		return marshalRedacted(items)
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("livewire: redact: the value is neither a list nor an object: %w", err)
	}
	for name := range hidden {
		delete(item, name)
	}
	return marshalRedacted(item)
}

func marshalRedacted(value any) (json.RawMessage, error) {
	redacted, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("livewire: redact: %w", err)
	}
	return redacted, nil
}

// hiddenFields names the fields whose Read check refuses ctx.
func hiddenFields(ctx context.Context, fields query.Fields) map[string]bool {
	hidden := map[string]bool{}
	for _, field := range fields {
		if field.Read != nil && field.Read(ctx) != nil {
			hidden[field.Name] = true
		}
	}
	return hidden
}

func visibleFields(fields query.Fields, hidden map[string]bool) query.Fields {
	visible := make(query.Fields, 0, len(fields))
	for _, field := range fields {
		if !hidden[field.Name] {
			visible = append(visible, field)
		}
	}
	return visible
}

// guardedFields turns the guards of a typed-parameters declaration into fields Redact
// understands.
func guardedFields(guarded []GuardedField) query.Fields {
	fields := make(query.Fields, 0, len(guarded))
	for _, guard := range guarded {
		fields = append(fields, query.Field{Name: guard.Name, Read: guard.Read})
	}
	return fields
}
