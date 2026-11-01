package postgresadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/govengers/livewire"
)

// ErrMalformedNotification marks a notification on Channel that is not a change the
// triggers send; the Listener drops it and reports it to OnError.
var ErrMalformedNotification = errors.New("postgresadapter: malformed notification")

// ListenerOptions configures a Listener. Exactly one of DSN and Connect is required: DSN is
// parsed by pgx.ParseConfig, Connect opens the dedicated connection itself (a pool's
// configuration, a token-based login). RetryInterval is the first delay after a failed or
// lost connection (default 1 s), doubling up to 32 times it, with up to 20 % jitter.
// PingInterval (default 30 s) is how long the connection may stay silent before a ping
// proves it alive; it also bounds a connect and a ping. OnError receives every connection
// failure and every dropped notification (errors.Is ErrMalformedNotification), from Run's
// goroutine; nil reports nothing. No value may be negative.
type ListenerOptions struct {
	DSN           string
	Connect       func(ctx context.Context) (*pgx.Conn, error)
	RetryInterval time.Duration
	PingInterval  time.Duration
	OnError       func(err error)
}

const (
	defaultRetryInterval = time.Second
	defaultPingInterval  = 30 * time.Second
	// maximumRetryDoublings caps a reconnect delay at RetryInterval << 5.
	maximumRetryDoublings = 5
	closeTimeout          = time.Second
)

// Listener is the change feed of one replica (section 5, livewire.ChangeFeed): one dedicated
// connection that LISTENs on Channel, reconnects with backoff and calls onResync after
// every successful LISTEN, the first included. Run it once at a time.
type Listener struct {
	options ListenerOptions
}

// NewListener validates and copies options; it opens no connection.
func NewListener(options *ListenerOptions) (*Listener, error) {
	if options == nil {
		return nil, errors.New("postgresadapter: NewListener needs options")
	}
	resolved := *options
	switch {
	case (resolved.DSN == "") == (resolved.Connect == nil):
		return nil, errors.New("postgresadapter: ListenerOptions needs exactly one of DSN and Connect")
	case resolved.RetryInterval < 0 || resolved.PingInterval < 0:
		return nil, errors.New("postgresadapter: ListenerOptions intervals must not be negative")
	}
	if resolved.DSN != "" {
		configuration, err := pgx.ParseConfig(resolved.DSN)
		if err != nil {
			return nil, fmt.Errorf("postgresadapter: parse ListenerOptions.DSN: %w", err)
		}
		resolved.DSN = ""
		resolved.Connect = func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, configuration) }
	}
	if resolved.RetryInterval == 0 {
		resolved.RetryInterval = defaultRetryInterval
	}
	if resolved.PingInterval == 0 {
		resolved.PingInterval = defaultPingInterval
	}
	return &Listener{options: resolved}, nil
}

// Run listens until ctx ends and returns ctx.Err() then; it implements livewire.ChangeFeed.
// A lost connection is reported to OnError and opened again after a backoff that starts
// over once a LISTEN succeeded.
func (l *Listener) Run(ctx context.Context, onChange func(livewire.Change), onResync func()) error {
	failures := 0
	for {
		listened, err := l.listen(ctx, onChange, onResync)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if listened {
			failures = 0
		}
		l.report(err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.backoff(failures)):
		}
		failures++
	}
}

// listen runs one connection until it fails; listened reports whether its LISTEN succeeded.
func (l *Listener) listen(ctx context.Context, onChange func(livewire.Change), onResync func()) (listened bool, err error) {
	connection, err := l.connect(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		_ = connection.Close(closing) // the connection failed or ctx ended; its close error adds nothing
	}()
	onResync()
	for {
		notification, err := l.wait(ctx, connection)
		if err != nil {
			return true, err
		}
		change, err := decodeChange(notification.Payload)
		if err != nil {
			l.report(err)
			continue
		}
		onChange(change)
	}
}

// connect opens the connection and LISTENs on Channel within PingInterval.
func (l *Listener) connect(ctx context.Context) (*pgx.Conn, error) {
	connecting, cancel := context.WithTimeout(ctx, l.options.PingInterval)
	defer cancel()
	connection, err := l.options.Connect(connecting)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: connect the listener: %w", err)
	}
	if _, err := connection.Exec(connecting, "LISTEN "+identifier(Channel)); err != nil {
		_ = connection.Close(context.WithoutCancel(connecting)) // LISTEN failed; that error is the one to report
		return nil, fmt.Errorf("postgresadapter: listen on %s: %w", Channel, err)
	}
	return connection, nil
}

// wait returns the next notification; a connection silent for PingInterval must answer a
// ping within PingInterval.
func (l *Listener) wait(ctx context.Context, connection *pgx.Conn) (*pgconn.Notification, error) {
	for {
		waiting, cancel := context.WithTimeout(ctx, l.options.PingInterval)
		notification, err := connection.WaitForNotification(waiting)
		cancel()
		switch {
		case err == nil:
			return notification, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case !pgconn.Timeout(err):
			return nil, fmt.Errorf("postgresadapter: the listener's connection: %w", err)
		}
		pinging, cancel := context.WithTimeout(ctx, l.options.PingInterval)
		err = connection.Ping(pinging)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("postgresadapter: ping the listener's connection: %w", err)
		}
	}
}

func (l *Listener) report(err error) {
	if l.options.OnError != nil {
		l.options.OnError(err)
	}
}

// backoff is the delay before the attempt after failures failed ones.
func (l *Listener) backoff(failures int) time.Duration {
	delay := l.options.RetryInterval << min(failures, maximumRetryDoublings)
	return delay + rand.N(delay/5+1) //nolint:gosec // jitter spreads reconnects, it guards nothing
}

// changePayload is the notification TriggerFunctionSQL sends.
type changePayload struct {
	Table     string `json:"table"`
	Operation string `json:"op"`
	ID        string `json:"id"`
	Partition string `json:"partition"`
}

// decodeChange reads a payload strictly: one JSON object with the known members only, a
// table and one of the operations.
func decodeChange(payload string) (livewire.Change, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.DisallowUnknownFields()
	var decoded changePayload
	if err := decoder.Decode(&decoded); err != nil {
		return livewire.Change{}, fmt.Errorf("%w: %q: %v", ErrMalformedNotification, payload, err) //nolint:errorlint // the decode error is detail, not API
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return livewire.Change{}, fmt.Errorf("%w: %q: data after the object", ErrMalformedNotification, payload)
	}
	operation := livewire.ChangeOperation(decoded.Operation)
	switch {
	case decoded.Table == "":
		return livewire.Change{}, fmt.Errorf("%w: %q: no table", ErrMalformedNotification, payload)
	case operation != livewire.ChangeInsert && operation != livewire.ChangeUpdate && operation != livewire.ChangeDelete:
		return livewire.Change{}, fmt.Errorf("%w: %q: unknown operation", ErrMalformedNotification, payload)
	}
	return livewire.Change{Table: decoded.Table, Operation: operation, ID: decoded.ID, Partition: decoded.Partition}, nil
}
