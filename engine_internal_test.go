package livewire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	})))
	engine, err := NewEngine(&EngineOptions{Registry: registry, IdentitySource: stubIdentities{}, Transactor: stubTransactor{}, SameHostOnly: true})
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
	ended := &subscription{socket: clientSocket, tables: []string{"items"}}
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
