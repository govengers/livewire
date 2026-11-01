package postgresadapter_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
)

var _ livewire.ChangeFeed = (*postgresadapter.Listener)(nil)

// feed records what a Listener delivers.
type feed struct {
	mutex   sync.Mutex
	changes []livewire.Change
	resyncs int
	errors  []error
}

func (f *feed) change(change livewire.Change) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.changes = append(f.changes, change)
}

func (f *feed) resync() {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.resyncs++
}

func (f *feed) failed(err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.errors = append(f.errors, err)
}

func (f *feed) snapshot() (changes []livewire.Change, resyncs int, failures []error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]livewire.Change(nil), f.changes...), f.resyncs, append([]error(nil), f.errors...)
}

// runListener runs listener until the test ends and returns what it delivers.
func runListener(t *testing.T, listener *postgresadapter.Listener) *feed {
	t.Helper()
	delivered := &feed{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, delivered.change, delivered.resync) }()
	t.Cleanup(func() {
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
	return delivered
}

func insertPart(t *testing.T, database *testDatabase, name string) string {
	t.Helper()
	var id string
	require.NoError(t, database.pool.QueryRow(t.Context(), "INSERT INTO parts (name) VALUES ($1) RETURNING id::text", name).Scan(&id))
	return id
}

func TestListenerResynchronizesOnEveryConnectAndDeliversChanges(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	delivered := &feed{}
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		DSN: database.dsn() + "?application_name=livewire_listener", RetryInterval: 10 * time.Millisecond, OnError: delivered.failed,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx, delivered.change, delivered.resync) }()
	require.Eventually(t, func() bool { _, resyncs, _ := delivered.snapshot(); return resyncs == 1 }, 5*time.Second, 10*time.Millisecond)

	first := insertPart(t, database, "bolt")
	require.Eventually(t, func() bool { changes, _, _ := delivered.snapshot(); return len(changes) == 1 }, 5*time.Second, 10*time.Millisecond)

	_, err = database.pool.Exec(t.Context(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database() AND application_name = 'livewire_listener'")
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, resyncs, _ := delivered.snapshot(); return resyncs == 2 }, 5*time.Second, 10*time.Millisecond)
	second := insertPart(t, database, "nut")
	require.Eventually(t, func() bool { changes, _, _ := delivered.snapshot(); return len(changes) == 2 }, 5*time.Second, 10*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	changes, _, failures := delivered.snapshot()
	require.Equal(t, []livewire.Change{
		{Table: "parts", Operation: livewire.ChangeInsert, ID: first},
		{Table: "parts", Operation: livewire.ChangeInsert, ID: second},
	}, changes)
	require.Len(t, failures, 1, "the terminated connection is reported")
}

func TestListenerPingsASilentConnection(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		DSN: database.dsn() + "?application_name=livewire_pinged", PingInterval: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	runListener(t, listener)
	require.Eventually(t, func() bool {
		var pinged bool
		err := database.pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT FROM pg_stat_activity WHERE datname = current_database() AND application_name = 'livewire_pinged' AND query = '-- ping')").Scan(&pinged)
		return err == nil && pinged
	}, 5*time.Second, 10*time.Millisecond)
}

func TestListenerDropsAndReportsMalformedNotifications(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	reported := &feed{}
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: database.dsn(), OnError: reported.failed})
	require.NoError(t, err)
	delivered := runListener(t, listener)
	require.Eventually(t, func() bool { _, resyncs, _ := delivered.snapshot(); return resyncs == 1 }, 5*time.Second, 10*time.Millisecond)
	malformed := []string{
		`not json`,
		`{"table":"parts","op":"insert","id":"1","partition":"","extra":1}`,
		`{"table":"parts","op":"insert"} {}`,
		`{"op":"insert"}`,
		`{"table":"parts","op":"truncate"}`,
		`["parts"]`,
	}
	for _, payload := range malformed {
		_, err := database.pool.Exec(t.Context(), "SELECT pg_notify($1, $2)", postgresadapter.Channel, payload)
		require.NoError(t, err)
	}
	_, err = database.pool.Exec(t.Context(), "SELECT pg_notify($1, $2)", postgresadapter.Channel, `{"table":"gadgets","op":"delete","id":"7","partition":"north"}`)
	require.NoError(t, err)
	require.Eventually(t, func() bool { changes, _, _ := delivered.snapshot(); return len(changes) == 1 }, 5*time.Second, 10*time.Millisecond)
	changes, _, _ := delivered.snapshot()
	require.Equal(t, livewire.Change{Table: "gadgets", Operation: livewire.ChangeDelete, ID: "7", Partition: "north"}, changes[0])
	_, _, failures := reported.snapshot()
	require.Len(t, failures, len(malformed))
	for _, failure := range failures {
		require.ErrorIs(t, failure, postgresadapter.ErrMalformedNotification)
	}
}

func TestListenerRetriesAFailedConnect(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	reported := &feed{}
	attempts := 0
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			attempts++ // Run calls Connect from its one goroutine
			if attempts < 3 {
				return nil, errors.New("refused")
			}
			return pgx.Connect(ctx, database.dsn())
		},
		RetryInterval: time.Millisecond,
		OnError:       reported.failed,
	})
	require.NoError(t, err)
	delivered := runListener(t, listener)
	require.Eventually(t, func() bool { _, resyncs, _ := delivered.snapshot(); return resyncs == 1 }, 5*time.Second, 10*time.Millisecond)
	_, _, failures := reported.snapshot()
	require.Len(t, failures, 2)
}

func TestListenerReportsAFailedListen(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	reported := make(chan error, 1)
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			connection, err := pgx.Connect(ctx, database.dsn())
			if err != nil {
				return nil, err
			}
			_, _ = connection.Exec(ctx, "BEGIN; SELECT 1/0") // fails on purpose: LISTEN in the aborted transaction fails
			return connection, nil
		},
		RetryInterval: time.Hour,
		OnError: func(err error) {
			select {
			case reported <- err:
			default:
			}
		},
	})
	require.NoError(t, err)
	runListener(t, listener)
	select {
	case err := <-reported:
		require.ErrorContains(t, err, "listen on livewire")
	case <-time.After(5 * time.Second):
		t.Fatal("the failed LISTEN was not reported")
	}
}

func TestListenerOptions(t *testing.T) {
	t.Parallel()
	connect := func(context.Context) (*pgx.Conn, error) { return nil, errors.New("unused") }
	for name, options := range map[string]*postgresadapter.ListenerOptions{
		"nil":                nil,
		"neither":            {},
		"both":               {DSN: "postgres://localhost/x", Connect: connect},
		"bad DSN":            {DSN: "postgres://localhost:notaport/x"},
		"negative retry":     {Connect: connect, RetryInterval: -time.Second},
		"negative heartbeat": {Connect: connect, PingInterval: -time.Second},
	} {
		_, err := postgresadapter.NewListener(options)
		require.Error(t, err, name)
	}
}

func TestListenerStartsTheBackoffOverAfterASuccessfulListen(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	const retryInterval = 100 * time.Millisecond
	var attempts atomic.Int64
	var pid atomic.Uint32
	reconnected := make(chan time.Time, 1)
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			switch attempts.Add(1) {
			case 1, 2, 3: // backoff grows to 8 * retryInterval
				return nil, errors.New("refused")
			case 4:
				connection, err := pgx.Connect(ctx, database.dsn())
				if err == nil {
					pid.Store(connection.PgConn().PID())
				}
				return connection, err
			default:
				select {
				case reconnected <- time.Now():
				default:
				}
				return nil, errors.New("refused again")
			}
		},
		RetryInterval: retryInterval,
	})
	require.NoError(t, err)
	delivered := runListener(t, listener)
	require.Eventually(t, func() bool { _, resyncs, _ := delivered.snapshot(); return resyncs == 1 }, 5*time.Second, 10*time.Millisecond)
	terminated := time.Now()
	_, err = database.pool.Exec(t.Context(), "SELECT pg_terminate_backend($1)", pid.Load())
	require.NoError(t, err)
	select {
	case at := <-reconnected:
		require.Less(t, at.Sub(terminated), 4*retryInterval, "the delay after a lost connection is the first one again, not 8 times it")
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not reconnect")
	}
}
