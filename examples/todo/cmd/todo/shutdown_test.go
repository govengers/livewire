package main

import (
	"context"
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

// TestTheDrainWithdrawsReadinessFirstAndKeepsOneBudget pins LAY-003: readiness fails before
// the server stops accepting, connections still open when the budget is spent are closed, and
// the sockets end within the same budget.
func TestTheDrainWithdrawsReadinessFirstAndKeepsOneBudget(t *testing.T) {
	t.Parallel()
	ready := &atomic.Bool{}
	ready.Store(true)
	server := &stoppingServer{ready: ready}
	sockets := &closingSockets{server: server}
	started := time.Now()
	err := drain(context.Background(), 50*time.Millisecond, ready, server, sockets)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, ready.Load())
	require.Equal(t, []string{"shutdown", "close", "sockets"}, server.steps)
	require.WithinDuration(t, started.Add(50*time.Millisecond), sockets.deadline, 40*time.Millisecond, "one budget for the drain and the sockets")
}
