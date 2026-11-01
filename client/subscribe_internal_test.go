package client

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

// TestListStateKeysItemsAsTheCore applies the core's own diffs to the client's list state:
// numbers past 2^53, one number in two spellings and an item with both "id" and "ID" key the
// same on both sides, so the client ends where the core's ListState is.
func TestListStateKeysItemsAsTheCore(t *testing.T) {
	testCases := []struct {
		name     string
		previous string
		current  string
	}{
		{"a number past 2^53", `[{"id":2,"n":1}]`, `[{"id":9007199254740993,"n":2},{"id":2,"n":1}]`},
		{"a number past 2^53 removed", `[{"id":9007199254740993},{"id":2}]`, `[{"id":2}]`},
		{"one number in two spellings", `[{"id":1.0,"v":1},{"id":2}]`, `[{"id":2},{"id":1e0,"v":2}]`},
		{"an exponent", `[{"id":1e21}]`, `[{"id":3},{"id":1e21}]`},
		{"id and ID", `[{"id":"a","ID":"b"}]`, `[{"id":"a","ID":"b"},{"id":"d"}]`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			previous, err := livewire.NewListState(json.RawMessage(testCase.previous), 1)
			require.NoError(t, err)
			current, err := livewire.NewListState(json.RawMessage(testCase.current), 2)
			require.NoError(t, err)
			change := current.ChangesSince(previous)
			require.NotNil(t, change)
			encoded, err := json.Marshal(change)
			require.NoError(t, err)
			var wire livewire.ListChange
			require.NoError(t, json.Unmarshal(encoded, &wire))

			state, err := newListState(json.RawMessage(`{"limit":10,"offset":0,"total":1,"items":` + testCase.previous + `}`))
			require.NoError(t, err)
			require.NoError(t, state.apply(&wire))
			payload, err := state.payload()
			require.NoError(t, err)
			var applied struct {
				Items json.RawMessage `json:"items"`
			}
			require.NoError(t, json.Unmarshal(payload, &applied))
			require.JSONEq(t, string(current.Snapshot()), string(applied.Items))
		})
	}
}

func TestAQueueThatFallsBehindCollapsesIntoASnapshot(t *testing.T) {
	queued := &subscription{signal: make(chan struct{}, 1)}
	change := &livewire.ListChange{Upserts: []json.RawMessage{}, Removes: []string{}}
	for sequence := range maximumQueuedPushes + 1 {
		queued.enqueue(rawPush{kind: livewire.PushListChange, data: json.RawMessage(strconv.Itoa(sequence)), change: change})
	}
	require.Equal(t, []rawPush{{kind: livewire.PushSnapshot, data: json.RawMessage(strconv.Itoa(maximumQueuedPushes))}}, queued.queue)
	queued.enqueue(rawPush{kind: livewire.PushError})
	require.Len(t, queued.queue, 2, "the error push is always kept")
}

// TestReconnectDelaysStayBounded runs every attempt of a long outage through delay: no
// option overflows it, a going-away close waits at most ReconnectDelay, any other doubles up
// to MaximumReconnectDelay.
func TestReconnectDelaysStayBounded(t *testing.T) {
	for _, options := range []Options{
		{ReconnectDelay: 30 * time.Second, MaximumReconnectDelay: 30 * time.Second},
		{ReconnectDelay: 10 * time.Second, MaximumReconnectDelay: time.Hour},
		{ReconnectDelay: time.Hour, MaximumReconnectDelay: time.Second},
		{ReconnectDelay: time.Millisecond, MaximumReconnectDelay: 1<<63 - 1},
	} {
		owner := newLive(&Client{options: options})
		for attempt := range 200 {
			delay := owner.delay(attempt, websocket.CloseAbnormalClosure)
			require.Positive(t, delay, "attempt %d of %+v", attempt, options)
			require.LessOrEqual(t, delay, options.MaximumReconnectDelay)
			require.LessOrEqual(t, owner.delay(attempt, websocket.CloseGoingAway), options.ReconnectDelay)
		}
	}
	owner := newLive(&Client{options: Options{ReconnectDelay: time.Second, MaximumReconnectDelay: time.Hour}})
	require.GreaterOrEqual(t, owner.delay(10, websocket.CloseAbnormalClosure), 512*time.Second, "a failure doubles the wait")
	require.LessOrEqual(t, owner.delay(10, websocket.CloseGoingAway), time.Second, "a going-away close reconnects soon")
}
