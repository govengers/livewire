package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/govengers/livewire"
)

// Push is one change of a subscription as Subscribe delivers it (section 4). Kind is the
// push's kind; Data is the read's state after it, so a consumer may ignore the kinds: a
// snapshot's or a replace's payload, a list with a diff applied. Change is the diff as it
// arrived, for kind diff only. A push of kind error is the last: Err is an *Error (the
// server ended the subscription with an error push, or refused or did not answer the
// subscribe), an *EndedError (the client's live side ended) or the failure to decode the
// state into Data; the channel closes after it.
type Push[Data any] struct {
	Kind   livewire.PushKind
	Data   Data
	Change *livewire.ListChange
	Err    error
}

// maximumQueuedPushes bounds the pushes one subscription holds for a consumer that does not
// keep up: beyond it they collapse into one push of the current state, a snapshot (or a
// replace), so the socket never waits for a consumer.
const maximumQueuedPushes = 64

// Subscribe subscribes to a live read on the client's socket (D2, D46): target is the
// declaration's name with the read kind after a dot ("parts", "parts.item"), parameters
// its wire parameters. The socket opens with the first subscription of the client and
// closes after the last; after a reconnect every subscription subscribes again and starts
// with a snapshot. The channel delivers the pushes in order and closes when ctx ends or
// after a push of kind error; a consumer that falls behind gets the current state instead
// of every step. A subscribe answered 404 for a filterId the client stored or registered
// without a name (D54) subscribes once more with the filter inline. Subscribe itself fails
// only when the live side ended (*EndedError).
func Subscribe[Data any](ctx context.Context, client *Client, target string, parameters Parameters) (<-chan Push[Data], error) {
	subscription, err := client.live.add(target, parameters)
	if err != nil {
		return nil, err
	}
	pushes := make(chan Push[Data])
	go deliver(ctx, client.live, subscription, pushes)
	return pushes, nil
}

// deliver hands the subscription's pushes to the consumer until ctx ends or a push of kind
// error was delivered, then removes the subscription and closes the channel.
func deliver[Data any](ctx context.Context, owner *live, subscription *subscription, pushes chan<- Push[Data]) {
	defer close(pushes)
	defer owner.remove(subscription)
	defer func() {
		if recovered := recover(); recovered != nil { // a bug: the consumer still gets a last push with its reason
			select {
			case pushes <- Push[Data]{Kind: livewire.PushError, Err: fmt.Errorf("livewire: the delivery failed: %v", recovered)}:
			case <-ctx.Done():
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-subscription.signal:
		}
		for {
			next, ok := owner.next(subscription)
			if !ok {
				break
			}
			push := decodePush[Data](next)
			select {
			case pushes <- push:
			case <-ctx.Done():
				return
			}
			if push.Kind == livewire.PushError {
				return
			}
		}
	}
}

func decodePush[Data any](next rawPush) Push[Data] {
	push := Push[Data]{Kind: next.kind, Change: next.change, Err: next.err}
	if next.kind == livewire.PushError {
		return push
	}
	if err := json.Unmarshal(next.data, &push.Data); err != nil {
		return Push[Data]{Kind: livewire.PushError, Err: fmt.Errorf("livewire: a %s push does not decode: %w", next.kind, err)}
	}
	return push
}

// rawPush is a push in its wire form with the state after it.
type rawPush struct {
	kind   livewire.PushKind
	data   json.RawMessage
	change *livewire.ListChange
	err    error
}

// subscription is one Subscribe call's subscription. Everything but signal is guarded by
// the live's mutex.
type subscription struct {
	target        string
	parameters    Parameters
	serverID      string
	sequence      int64
	list          *listState
	retriedInline bool
	ended         bool
	queue         []rawPush
	// signal wakes the delivery when the queue grew; it holds one wake at most.
	signal chan struct{}
}

// enqueue queues a push for the consumer; beyond maximumQueuedPushes the queue collapses
// into the latest state (a push of kind error is always kept).
func (s *subscription) enqueue(push rawPush) {
	if push.kind != livewire.PushError && len(s.queue) >= maximumQueuedPushes {
		if push.kind == livewire.PushListChange {
			push.kind, push.change = livewire.PushSnapshot, nil
		}
		s.queue = s.queue[:0]
	}
	s.queue = append(s.queue, push)
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// listState is what a list subscription last received: the page's head, the order of the
// item ids and every item's JSON by id (section 4).
type listState struct {
	limit  int
	offset int
	total  int
	next   *string
	order  []string
	items  map[string]json.RawMessage
}

type listPayload struct {
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	Total  int               `json:"total"`
	Items  []json.RawMessage `json:"items"`
	Next   *string           `json:"next,omitempty"`
}

// newListState indexes a snapshot's items by id.
func newListState(payload json.RawMessage) (*listState, error) {
	var page listPayload
	if err := json.Unmarshal(payload, &page); err != nil {
		return nil, fmt.Errorf("livewire: a snapshot is no list response: %w", err)
	}
	state := &listState{limit: page.Limit, offset: page.Offset, total: page.Total, next: page.Next, items: map[string]json.RawMessage{}}
	for _, item := range page.Items {
		id, err := livewire.ItemID(item)
		if err != nil {
			return nil, err
		}
		if _, repeated := state.items[id]; repeated {
			return nil, fmt.Errorf("livewire: a snapshot repeats the id %s", id)
		}
		state.order = append(state.order, id)
		state.items[id] = item
	}
	return state, nil
}

// apply applies a diff: upserts by id, removes, the order when sent (else the old order
// with new items appended), the total when sent.
func (s *listState) apply(change *livewire.ListChange) error {
	items := maps.Clone(s.items)
	var added []string
	for _, item := range change.Upserts {
		id, err := livewire.ItemID(item)
		if err != nil {
			return err
		}
		if _, known := items[id]; !known {
			added = append(added, id)
		}
		items[id] = bytes.Clone(item) // the delivered change is the consumer's: the state owns its own bytes
	}
	for _, id := range change.Removes {
		delete(items, id)
	}
	order := slices.Clone(change.Order)
	if order == nil {
		for _, id := range slices.Concat(s.order, added) {
			if _, present := items[id]; present {
				order = append(order, id)
			}
		}
	}
	s.items, s.order = items, order
	if change.Total != nil {
		s.total = *change.Total
	}
	return nil
}

// payload is the state as a list response.
func (s *listState) payload() (json.RawMessage, error) {
	page := listPayload{Limit: s.limit, Offset: s.offset, Total: s.total, Next: s.next, Items: []json.RawMessage{}}
	for _, id := range s.order {
		if item, present := s.items[id]; present {
			page.Items = append(page.Items, item)
		}
	}
	return json.Marshal(page)
}
