package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"
)

// manualClock is a clock the test moves; AfterFunc work runs when Advance passes its time.
type manualClock struct {
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	at      time.Time
	delay   time.Duration
	work    func()
	stopped bool
}

func (c *manualClock) Now() time.Time { return c.now }

func (c *manualClock) AfterFunc(delay time.Duration, work func()) func() bool {
	timer := &manualTimer{at: c.now.Add(delay), delay: delay, work: work}
	c.timers = append(c.timers, timer)
	return func() bool {
		stopped := !timer.stopped
		timer.stopped = true
		return stopped
	}
}

func (c *manualClock) Advance(duration time.Duration) {
	c.now = c.now.Add(duration)
	for _, timer := range c.timers {
		if !timer.stopped && !timer.at.After(c.now) {
			timer.stopped = true
			timer.work()
		}
	}
}

func TestOutboxDeliversWithinTheWait(t *testing.T) {
	clock := &manualClock{now: time.Unix(1000, 0)}
	box := newOutbox(1<<20, 100*time.Millisecond, clock)
	first, second := &subscription{}, &subscription{}

	box.markPending(first)
	_, due := box.take()
	require.Equal(t, []*subscription{first}, due, "after a quiet period a change leaves at once")

	clock.Advance(10 * time.Millisecond)
	box.markPending(first)
	box.markPending(second)
	box.markPending(first)
	_, due = box.take()
	require.Empty(t, due, "within the wait since the last delivery the pushes are held")
	require.Len(t, clock.timers, 1, "one timer per delivery")
	require.LessOrEqual(t, clock.timers[0].delay, 100*time.Millisecond, "a push waits at most DeliveryBatchWait after its first change")
	require.Equal(t, 90*time.Millisecond, clock.timers[0].delay)

	clock.Advance(89 * time.Millisecond)
	_, due = box.take()
	require.Empty(t, due)
	clock.Advance(time.Millisecond)
	_, due = box.take()
	require.ElementsMatch(t, []*subscription{first, second}, due, "every change of the burst leaves as one push per subscription")
	_, due = box.take()
	require.Empty(t, due)
}

type stubIdentities struct{}

func (stubIdentities) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return &auth.Identity{Provider: "test", Subject: "ada", Active: true}, nil
}

type stubTransactor struct{}

func (stubTransactor) Begin(context.Context, TransactionDetails) (Transaction, error) {
	return stubTransaction{}, nil
}

type stubTransaction struct{}

func (stubTransaction) Commit(context.Context) error   { return nil }
func (stubTransaction) Rollback(context.Context) error { return nil }
func (stubTransaction) Handle() any                    { return nil }

type stubPresences struct{}

func (stubPresences) Join(context.Context, *Presence) error                       { return nil }
func (stubPresences) Leave(context.Context, string, string, string, string) error { return nil }
func (stubPresences) Heartbeat(context.Context, string) error                     { return nil }
func (stubPresences) List(context.Context, string, string, string) ([]Presence, error) {
	return []Presence{{Socket: "elsewhere", Display: "Eve"}}, nil
}
func (stubPresences) Sweep(context.Context, time.Duration) (int, error) { return 0, nil }

type stubItem struct {
	ID string `json:"id"`
}

func stubEngine(t *testing.T) (*Engine, *socket) {
	t.Helper()
	registry, err := NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(NewList("failing", "/failing", &ListOptions[stubItem, NoParameters]{
		Tables: []string{"items"},
		Read:   authorization.IsAuthenticated(),
		Load: func(context.Context, *NoParameters) ([]stubItem, error) {
			return nil, errors.New("the database is down")
		},
	}), NewObject("board", "/board", &ObjectOptions[stubItem, NoParameters]{
		Tables: []string{"boards"},
		Read:   authorization.IsAuthenticated(),
		Load: func(ctx context.Context, _ *NoParameters) (*stubItem, error) {
			if auth.IdentityFrom(ctx).Subject != "ada" {
				return nil, NotFound() // the board is ada's only
			}
			return &stubItem{ID: "board"}, nil
		},
	})))
	engine, err := NewEngine(&EngineOptions{Registry: registry, IdentitySource: stubIdentities{}, Transactor: stubTransactor{}, PresenceStore: stubPresences{}, SameHostOnly: true})
	require.NoError(t, err)
	identity := &auth.Identity{Provider: "test", Subject: "ada", Active: true}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/live", nil)
	clientSocket, err := engine.admit(request, identity)
	require.NoError(t, err)
	t.Cleanup(func() {
		clientSocket.cancel()
		engine.release(clientSocket)
	})
	return engine, clientSocket
}

func TestIndexLeavesOutAnEndedSubscription(t *testing.T) {
	engine, clientSocket := stubEngine(t)
	ended := &subscription{socket: clientSocket, partitions: map[string]string{"items": ""}}
	ended.end() // an unsubscribe that ran before the installation indexed it
	engine.index(ended)
	require.Empty(t, engine.byTable)
}

func TestFailedInstallationEndsItsSubscription(t *testing.T) {
	engine, clientSocket := stubEngine(t)
	opening := &installation{socket: clientSocket, frameID: "1", request: ReadRequest{Target: "failing"}}
	ctx := auth.WithIdentity(t.Context(), clientSocket.lastIdentity())
	require.Error(t, opening.install(ctx))
	require.NotNil(t, opening.subscription)
	require.ErrorIs(t, opening.subscription.ctx.Err(), context.Canceled, "a refused subscribe cancels its lifetime")
	require.Empty(t, clientSocket.subscriptions)
	require.Empty(t, engine.byTable)

	clientSocket.engine.options.SubscriptionsPerSocket = 0
	refused := &installation{socket: clientSocket, frameID: "2", request: ReadRequest{Target: "failing"}}
	require.Error(t, refused.install(ctx))
	require.Nil(t, refused.subscription, "a subscription past the limit never gets a lifetime")
}

func TestOwnerMigrationKeepsTheNewestAdopt(t *testing.T) {
	engine, clientSocket := stubEngine(t)
	engine.migrateOwner(clientSocket, "test:newer", 2)
	engine.migrateOwner(clientSocket, "test:older", 1) // the older adopt's migration ran last
	require.Equal(t, "test:newer", clientSocket.ownerKey)
	require.Equal(t, map[string]int{"test:newer": 1}, engine.perOwner)
}

func TestRetryOwnsOneTimerAndASuccessEndsIt(t *testing.T) {
	engine, clientSocket := stubEngine(t)
	clock := &manualClock{now: time.Unix(1000, 0)}
	engine.clock = clock
	failing := &subscription{socket: clientSocket, dirty: make(chan struct{}, 1), delivered: &view{payload: []byte(`{}`)}}

	failing.signal() // a mark that landed during the failed re-run
	failing.retryLater(rerunMarks{})
	require.Empty(t, failing.dirty, "the retry drains the mark: it covers it")
	failing.markDirty()
	require.Empty(t, failing.dirty, "a mark during the backoff waits for the retry")

	failing.retryLater(rerunMarks{record: true})
	require.Len(t, clock.timers, 2)
	require.True(t, clock.timers[0].stopped, "a second failure stops the first timer")
	require.True(t, failing.recordDue, "a record read it owed stays owed")

	failing.settle(&view{payload: []byte(`{}`)}, false)
	require.True(t, clock.timers[1].stopped, "a success ends the pending retry")
	failing.markDirty()
	require.Len(t, failing.dirty, 1, "and marks signal again")
}

func TestNormalizeOriginsDropsDefaultPortsOnly(t *testing.T) {
	normalized, err := normalizeOrigins([]string{"https://[::1]:443", "http://[::1]:8080", "HTTPS://App.Example:443", "http://app.example:80", "https://app.example:8443"})
	require.NoError(t, err)
	require.Equal(t, []string{"https://[::1]", "http://[::1]:8080", "https://app.example", "http://app.example", "https://app.example:8443"}, normalized)
}

func TestPresenceReadUnderANewIdentityRunsTheRecordRead(t *testing.T) {
	engine, clientSocket := stubEngine(t)
	ctx := auth.WithIdentity(t.Context(), clientSocket.lastIdentity())
	resolved, err := engine.resolve(ctx, ReadRequest{Target: "board", Kind: ReadPresence, Source: MapSource{"key": "k"}})
	require.NoError(t, err)
	watching, err := clientSocket.newSubscription(ctx, resolved)
	require.NoError(t, err)
	require.NoError(t, clientSocket.addSubscription(watching))
	engine.index(watching)
	result, err := engine.load(ctx, resolved)
	require.NoError(t, err)
	_, err = watching.firstPush(result, auth.IdentityFrom(ctx))
	require.NoError(t, err)

	marks := watching.takeMarks() // a presence-only re-run took its marks
	clientSocket.adopt(&auth.Identity{Provider: "test", Subject: "bob", Active: true}, 1)
	watching.rerunWith(marks) // and then reads under the identity the adopt brought
	require.True(t, watching.isEnded(), "the record read ran under the new identity and refused it")
	frames, _ := clientSocket.outbox.take()
	require.Len(t, frames, 1)
	require.Contains(t, string(frames[0]), `"kind":"error"`, "no presence push went out under the new identity first")
}

// TestErrorPushIsTheLastSequence overlaps endWithError with a writer that keeps computing
// pushes of a changing subscription: no push may carry a sequence after the error's. The
// overlap is a race, so the test repeats it; the old split of sequence and end failed it.
func TestErrorPushIsTheLastSequence(t *testing.T) {
	_, clientSocket := stubEngine(t)
	for attempt := range 200 {
		ending := &subscription{
			socket: clientSocket, id: strconv.Itoa(attempt), dirty: make(chan struct{}, 1), sequence: 1,
			current: &view{payload: []byte(`{"n":0}`)}, delivered: &view{payload: []byte(`{"n":0}`)},
		}
		started := make(chan struct{})
		var pushed []int64
		pushing := make(chan struct{})
		go func() {
			defer close(pushing)
			close(started)
			for change := 1; !ending.isEnded(); change++ {
				ending.mutex.Lock()
				ending.current = &view{payload: []byte(`{"n":` + strconv.Itoa(change) + `}`)}
				ending.mutex.Unlock()
				if push := ending.nextPush(); push != nil {
					var frame PushFrame
					_ = json.Unmarshal(push, &frame) // the engine encoded it
					pushed = append(pushed, frame.Sequence)
				}
			}
		}()
		<-started
		ending.endWithError(NotFound())
		<-pushing
		frames, _ := clientSocket.outbox.take()
		require.Len(t, frames, 1)
		var terminal PushFrame
		require.NoError(t, json.Unmarshal(frames[0], &terminal))
		require.Equal(t, PushError, terminal.Kind)
		for _, sequence := range pushed {
			require.Less(t, sequence, terminal.Sequence, "attempt %d: a push after the error", attempt)
		}
	}
}
