package livewire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ListState is what a list subscription last pushed (the engine keeps one per list
// subscription; a client applies ListChange the same way): every item's JSON by id, the
// order of the ids and the total of the list.
type ListState struct {
	order []string
	items map[string]json.RawMessage
	total int
}

// NewListState indexes the marshaled items (a JSON array) by their "id", a string or a
// number. An item without an id, with an id of another shape, or with the id of an earlier
// item (the number 1 and the string "1" are the same id) is an error: such a list cannot be
// diffed and must not be pushed.
func NewListState(rawItems json.RawMessage, total int) (*ListState, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(rawItems, &items); err != nil {
		return nil, fmt.Errorf("livewire: the items are not a JSON array: %w", err)
	}
	state := &ListState{order: make([]string, 0, len(items)), items: make(map[string]json.RawMessage, len(items)), total: total}
	for _, item := range items {
		id, err := itemID(item)
		if err != nil {
			return nil, err
		}
		if _, duplicate := state.items[id]; duplicate {
			return nil, fmt.Errorf("livewire: two list items carry the id %q", id)
		}
		state.order = append(state.order, id)
		state.items[id] = item
	}
	return state, nil
}

// Snapshot is the items as one JSON array, in order.
func (s *ListState) Snapshot() json.RawMessage {
	var snapshot bytes.Buffer
	snapshot.WriteByte('[')
	for position, id := range s.order {
		if position > 0 {
			snapshot.WriteByte(',')
		}
		snapshot.Write(s.items[id])
	}
	snapshot.WriteByte(']')
	return snapshot.Bytes()
}

// ChangesSince returns what changed from previous to s: the items that are new or whose
// JSON differs, the ids that are gone, the full order when membership or order changed, and
// the total when it changed. It returns nil when nothing changed.
func (s *ListState) ChangesSince(previous *ListState) *ListChange {
	changes := &ListChange{Upserts: []json.RawMessage{}, Removes: []string{}}
	for _, id := range s.order {
		if old, ok := previous.items[id]; !ok || !bytes.Equal(old, s.items[id]) {
			changes.Upserts = append(changes.Upserts, s.items[id])
		}
	}
	for _, id := range previous.order {
		if _, ok := s.items[id]; !ok {
			changes.Removes = append(changes.Removes, id)
		}
	}
	if !slices.Equal(previous.order, s.order) {
		changes.Order = slices.Clone(s.order)
	}
	if previous.total != s.total {
		changes.Total = new(s.total)
	}
	unchanged := len(changes.Upserts) == 0 && len(changes.Removes) == 0 && changes.Order == nil && changes.Total == nil
	if unchanged {
		return nil
	}
	return changes
}

// itemID is the item's id as the key of the list, read from the member spelled exactly "id"
// (no case-insensitive alias): a JSON string as it is, a JSON number as JavaScript's Number
// toString writes it (1, 1.0 and 1e0 are one id; 1234567 stays plain; 1e21 and 1e-7 take
// exponent form; -0 is 0), so the server and the browser key the same.
func itemID(item json.RawMessage) (string, error) {
	var members map[string]json.RawMessage
	err := json.Unmarshal(item, &members)
	rawID, present := members[keyFieldJSON]
	missing := err != nil || !present || bytes.Equal(rawID, []byte("null"))
	if missing {
		return "", fmt.Errorf("livewire: a list item has no id: %s", item)
	}
	var text string
	if err := json.Unmarshal(rawID, &text); err == nil {
		return text, nil
	}
	var number float64
	if err := json.Unmarshal(rawID, &number); err == nil {
		return formatNumberID(number), nil
	}
	return "", fmt.Errorf("livewire: a list item's id is neither a string nor a number: %s", rawID)
}

// formatNumberID writes a number the way JavaScript's Number toString does, so a numeric id
// keys the same on the server and in the browser: plain digits for 1e-6 <= |n| < 1e21,
// exponent form outside, and -0 as 0.
func formatNumberID(number float64) string {
	if number == 0 {
		return "0"
	}
	magnitude := math.Abs(number)
	if magnitude >= 1e-6 && magnitude < 1e21 {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(number, 'e', -1, 64), "e")
	sign, digits := exponent[:1], strings.TrimLeft(exponent[1:], "0")
	return mantissa + "e" + sign + digits
}
