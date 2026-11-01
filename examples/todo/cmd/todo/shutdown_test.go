package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stoppingServer records drain's steps; its Shutdown waits for its context like a server
// whose requests outlive the budget.
type stoppingServer struct {
	ready *atomic.Bool
	steps []string
}

func (s *stoppingServer) Shutdown(ctx context.Context) error {
	if s.ready.Load() {
		s.steps = append(s.steps, "shutdown while ready")
	}
	s.steps = append(s.steps, "shutdown")
	<-ctx.Done()
	return ctx.Err()
}

func (s *stoppingServer) Close() error {
	s.steps = append(s.steps, "close")
	return nil
}

type closingSockets struct {
	server   *stoppingServer
	deadline time.Time
}

func (c *closingSockets) Close(ctx context.Context) error {
	c.server.steps = append(c.server.steps, "sockets")
	c.deadline, _ = ctx.Deadline()
	return nil
}

// TestTheDrainWithdrawsReadinessFirstAndKeepsOneBudget pins LAY-003: readiness fails and the
// readiness delay passes before the server stops accepting, connections still open when the budget is spent are closed, and
// the sockets end within the same budget.
func TestTheDrainWithdrawsReadinessFirstAndKeepsOneBudget(t *testing.T) {
	t.Parallel()
	ready := &atomic.Bool{}
	ready.Store(true)
	server := &stoppingServer{ready: ready}
	sockets := &closingSockets{server: server}
	started := time.Now()
	drainer := drainer{ready: ready, readinessDelay: 5 * time.Second, budget: 50 * time.Millisecond, wait: func(_ context.Context, delay time.Duration) {
		server.steps = append(server.steps, fmt.Sprintf("ready %t, wait %s", ready.Load(), delay))
	}}
	err := drainer.drain(context.Background(), server, sockets)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, ready.Load())
	require.Equal(t, []string{"ready false, wait 5s", "shutdown", "close", "sockets"}, server.steps, "readiness, its pause, then the drain")
	require.WithinDuration(t, started.Add(50*time.Millisecond), sockets.deadline, 40*time.Millisecond, "one budget for the drain and the sockets")
}
