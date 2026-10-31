package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/govengers/livewire/query"
)

// RedactItems strips the fields whose Read check refuses the identity in ctx from every item
// of raw, a marshaled JSON array of objects (D39). Fields that are visible or absent are left
// alone; a raw value without guarded fields is returned as it is, otherwise the result is
// re-marshaled with its keys sorted. Anything but an array of objects is an error, so a
// wrong caller fails instead of leaking.
func RedactItems(ctx context.Context, fields query.Fields, raw json.RawMessage) (json.RawMessage, error) {
	hidden := hiddenFields(ctx, fields)
	if len(hidden) == 0 {
		return raw, nil
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, errors.New("livewire: redact: the value is not a JSON array of objects")
	}
	for _, item := range items {
		if item == nil {
			return nil, errors.New("livewire: redact: the array holds a null item")
		}
		for name := range hidden {
			delete(item, name)
		}
	}
	return marshalRedacted(items)
}

// RedactItem is RedactItems for one marshaled object.
func RedactItem(ctx context.Context, fields query.Fields, raw json.RawMessage) (json.RawMessage, error) {
	hidden := hiddenFields(ctx, fields)
	if len(hidden) == 0 {
		return raw, nil
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return nil, errors.New("livewire: redact: the value is not a JSON object")
	}
	for name := range hidden {
		delete(item, name)
	}
	return marshalRedacted(item)
}

// hiddenFields names the fields whose Read check refuses ctx; the one visibility rule is
// query.Fields.Visible.
func hiddenFields(ctx context.Context, fields query.Fields) map[string]bool {
	visible := fields.Visible(ctx)
	hidden := map[string]bool{}
	for _, field := range fields {
		if _, ok := visible.Lookup(field.Name); !ok {
			hidden[field.Name] = true
		}
	}
	return hidden
}

func marshalRedacted(value any) (json.RawMessage, error) {
	redacted, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("livewire: redact: %w", err)
	}
	return redacted, nil
}

// guardedFields turns the guards of a typed-parameters declaration into fields the redaction
// understands.
func guardedFields(guarded []GuardedField) query.Fields {
	fields := make(query.Fields, 0, len(guarded))
	for _, guard := range guarded {
		fields = append(fields, query.Field{Name: guard.Name, Read: guard.Read})
	}
	return fields
}
