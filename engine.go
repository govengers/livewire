package livewire

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// EngineOptions configures the live engine (section 5). Registry, IdentitySource and
// Transactor are required; AllowedOrigins is required unless SameHostOnly says that only the
// request's own host may open a socket (D9). ChangeFeed is optional: without one, a
// subscription re-runs only when its socket's identity changes and, when its filter carries
// a relative time, on the safety tick. FilterStore enables stored filters (D13),
// IdempotencyStore the replay of mutations (D16), AuditStore the action audit (D32),
// PresenceStore the join and leave ops and the presence reads (D37); each is optional and its
// feature is off without it. The engine's outcomes go to the registry's Observer (D49). The
// engine runs no store maintenance: the sweeps and the audit partitions (the postgresadapter's
// FilterStore.Sweep, IdempotencyStore.Sweep and AuditStore.Maintain) are the application's to
// schedule, a periodic govengers/jobs job for instance, so one replica runs them, not every
// engine, and a failure lands where the application handles its jobs.
//
// The limits (D10) default when zero: SubscriptionsPerSocket 64, InFlightPerSocket 16,
// PresencesPerSocket 64 (records one socket may join at once), FiltersPerSocket 64 (the
// distinct inline filters the live subscriptions of one socket hold, D13; it bounds no
// filter rows beyond that socket's live ones), QueuedBytesPerSocket 8 MiB, FrameBytes 64 KiB,
// QueryConcurrency 32 (D52); SocketsPerOwner 0 is unlimited. The intervals default when
// zero: RevalidateInterval 60 s (D6, the cache of a socket's identity for re-runs and the
// tick that re-validates every socket), SafetyTickInterval 30 s (the re-run of
// time-dependent subscriptions), PresenceHeartbeat 30 s and PresenceExpiry 90 s (D37),
// DeliveryBatchWait 100 ms (the longest a push waits after its first change, so a burst
// leaves as one push per subscription), RetryInterval 1 s (the first backoff of a failed
// re-run or change feed, doubling up to 32 times it) and WriteTimeout 10 s (a write the
// client does not take in time closes its socket 1013). No value may be negative;
// PresenceExpiry must exceed PresenceHeartbeat.
type EngineOptions struct {
	Registry               *Registry
	ChangeFeed             ChangeFeed
	IdentitySource         IdentitySource
	Transactor             Transactor
	FilterStore            FilterStore
	IdempotencyStore       IdempotencyStore
	AuditStore             AuditStore
	PresenceStore          PresenceStore
	AllowedOrigins         []string
	SameHostOnly           bool
	SubscriptionsPerSocket int
	InFlightPerSocket      int
	PresencesPerSocket     int
	FiltersPerSocket       int
	QueuedBytesPerSocket   int
	FrameBytes             int64
	QueryConcurrency       int
	SocketsPerOwner        int
	RevalidateInterval     time.Duration
	SafetyTickInterval     time.Duration
	PresenceHeartbeat      time.Duration
	PresenceExpiry         time.Duration
	DeliveryBatchWait      time.Duration
	RetryInterval          time.Duration
	WriteTimeout           time.Duration
}

const (
	defaultSubscriptionsPerSocket = 64
	defaultInFlightPerSocket      = 16
	defaultPresencesPerSocket     = 64
	defaultFiltersPerSocket       = 64
	defaultQueuedBytesPerSocket   = 8 << 20
	defaultFrameBytes             = 64 << 10
	defaultQueryConcurrency       = 32
	defaultRevalidateInterval     = 60 * time.Second
	defaultSafetyTickInterval     = 30 * time.Second
	defaultPresenceHeartbeat      = 30 * time.Second
	defaultPresenceExpiry         = 90 * time.Second
	defaultDeliveryBatchWait      = 100 * time.Millisecond
	defaultRetryInterval          = time.Second
	defaultWriteTimeout           = 10 * time.Second
	// maximumRetryDoublings caps a backoff at RetryInterval << 5.
	maximumRetryDoublings = 5
)

// Engine serves an API's socket (section 4) and runs the read and write pipelines both
// transports share (D23): Read, Call, Batch and Upload are what the REST adapter calls, the
// socket ops run the same code. Build it with NewEngine, serve Handler, run Run, and end it
// with Close. It is safe for concurrent use.
type Engine struct {
	options  EngineOptions
	registry *Registry
	origins  []string
	upgrader websocket.Upgrader
	clock    clock
	// queries bounds the work the engine does for sockets (D52).
	queries   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	running   atomic.Bool
	// lifetimes counts every admitted socket until its goroutines ended and every Run until
	// it returned; Close waits for both.
	lifetimes sync.WaitGroup

	mutex         sync.Mutex
	closing       bool
	sockets       map[*socket]struct{}
	perOwner      map[string]int
	byTable       map[string]map[string]map[*subscription]struct{}
	timeDependent map[*subscription]struct{}
}

// NewEngine validates and copies options; the caller's struct is never written and its
// AllowedOrigins slice is copied.
func NewEngine(options *EngineOptions) (*Engine, error) {
	if options == nil {
		return nil, errors.New("livewire: NewEngine needs options")
	}
	resolved := *options
	resolved.AllowedOrigins = slices.Clone(options.AllowedOrigins)
	if err := resolved.validate(); err != nil {
		return nil, err
	}
	resolved.applyDefaults()
	origins, err := normalizeOrigins(resolved.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	engine := &Engine{
		options:       resolved,
		registry:      resolved.Registry,
		origins:       origins,
		clock:         systemClock{},
		queries:       make(chan struct{}, resolved.QueryConcurrency),
		done:          make(chan struct{}),
		sockets:       map[*socket]struct{}{},
		perOwner:      map[string]int{},
		byTable:       map[string]map[string]map[*subscription]struct{}{},
		timeDependent: map[*subscription]struct{}{},
	}
	engine.upgrader = websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // the handler checked it before the upgrade (D9)
		Error:       writeHandshakeError,
	}
	return engine, nil
}

func (o *EngineOptions) validate() error {
	negative := o.SubscriptionsPerSocket < 0 || o.InFlightPerSocket < 0 || o.PresencesPerSocket < 0 ||
		o.FiltersPerSocket < 0 || o.QueuedBytesPerSocket < 0 || o.FrameBytes < 0 || o.QueryConcurrency < 0 ||
		o.SocketsPerOwner < 0 || o.RevalidateInterval < 0 || o.SafetyTickInterval < 0 || o.PresenceHeartbeat < 0 ||
		o.PresenceExpiry < 0 || o.DeliveryBatchWait < 0 || o.RetryInterval < 0 || o.WriteTimeout < 0
	switch {
	case o.Registry == nil:
		return errors.New("livewire: EngineOptions.Registry is required")
	case o.IdentitySource == nil:
		return errors.New("livewire: EngineOptions.IdentitySource is required: sockets re-validate their identity (D6)")
	case o.Transactor == nil:
		return errors.New("livewire: EngineOptions.Transactor is required: every unit of work runs in a transaction (D31, D45)")
	case len(o.AllowedOrigins) == 0 && !o.SameHostOnly:
		return errors.New("livewire: EngineOptions.AllowedOrigins is required unless SameHostOnly is set (D9)")
	case negative:
		return errors.New("livewire: no limit or interval of EngineOptions may be negative")
	}
	heartbeat := cmp.Or(o.PresenceHeartbeat, defaultPresenceHeartbeat)
	if expiry := cmp.Or(o.PresenceExpiry, defaultPresenceExpiry); expiry <= heartbeat {
		return fmt.Errorf("livewire: PresenceExpiry %s must exceed PresenceHeartbeat %s", expiry, heartbeat)
	}
	return nil
}

func (o *EngineOptions) applyDefaults() {
	o.SubscriptionsPerSocket = cmp.Or(o.SubscriptionsPerSocket, defaultSubscriptionsPerSocket)
	o.InFlightPerSocket = cmp.Or(o.InFlightPerSocket, defaultInFlightPerSocket)
	o.PresencesPerSocket = cmp.Or(o.PresencesPerSocket, defaultPresencesPerSocket)
	o.FiltersPerSocket = cmp.Or(o.FiltersPerSocket, defaultFiltersPerSocket)
	o.QueuedBytesPerSocket = cmp.Or(o.QueuedBytesPerSocket, defaultQueuedBytesPerSocket)
	o.FrameBytes = cmp.Or(o.FrameBytes, defaultFrameBytes)
	o.QueryConcurrency = cmp.Or(o.QueryConcurrency, defaultQueryConcurrency)
	o.RevalidateInterval = cmp.Or(o.RevalidateInterval, defaultRevalidateInterval)
	o.SafetyTickInterval = cmp.Or(o.SafetyTickInterval, defaultSafetyTickInterval)
	o.PresenceHeartbeat = cmp.Or(o.PresenceHeartbeat, defaultPresenceHeartbeat)
	o.PresenceExpiry = cmp.Or(o.PresenceExpiry, defaultPresenceExpiry)
	o.DeliveryBatchWait = cmp.Or(o.DeliveryBatchWait, defaultDeliveryBatchWait)
	o.RetryInterval = cmp.Or(o.RetryInterval, defaultRetryInterval)
	o.WriteTimeout = cmp.Or(o.WriteTimeout, defaultWriteTimeout)
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

// normalizeOrigins checks every allowed origin is a bare http or https origin and returns
// them as browsers send the Origin header: lower case scheme://host[:port], the scheme's
// default port dropped.
func normalizeOrigins(origins []string) ([]string, error) {
	normalized := make([]string, 0, len(origins))
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		bareOrigin := err == nil && defaultPorts[parsed.Scheme] != "" && parsed.Host != "" &&
			(parsed.Path == "" || parsed.Path == "/") && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.User == nil
		if !bareOrigin {
			return nil, fmt.Errorf("livewire: the allowed origin %q is not scheme://host[:port] with http or https", origin)
		}
		host := parsed.Host
		if parsed.Port() == defaultPorts[parsed.Scheme] {
			host = strings.TrimSuffix(host, ":"+parsed.Port()) // an IPv6 host keeps its brackets
		}
		normalized = append(normalized, strings.ToLower(parsed.Scheme+"://"+host))
	}
	return normalized, nil
}

var errEngineRunning = errors.New("livewire: the engine is already running")

// Run consumes the change feed and drives the ticks until ctx ends or Close is called, and
// returns nil then. Every successful connect of the feed, the first included, re-runs every
// subscription with a snapshot (section 5); a feed that fails is restarted after a backoff
// with jitter, from RetryInterval doubling up to 32 times it, reset by a successful connect,
// and reported as an OutcomeFeed. The ticks re-validate every socket's identity
// (RevalidateInterval), re-run the time-dependent subscriptions (SafetyTickInterval), and
// refresh and sweep presences (PresenceHeartbeat, PresenceExpiry). Run returns an error when
// it is already running; after Close it returns nil at once. Close waits for it.
func (e *Engine) Run(ctx context.Context) error {
	if !e.running.CompareAndSwap(false, true) {
		return errEngineRunning
	}
	defer e.running.Store(false)
	e.mutex.Lock()
	closing := e.closing
	if !closing {
		e.lifetimes.Add(1)
	}
	e.mutex.Unlock()
	if closing {
		return nil
	}
	defer e.lifetimes.Done()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var work sync.WaitGroup
	work.Go(func() { // Close cancels Run's work at once, a tick's store call included
		select {
		case <-e.done:
			stop()
		case <-ctx.Done():
		}
	})
	if e.options.ChangeFeed != nil {
		work.Go(func() { e.consumeFeed(ctx) })
	}
	e.tick(ctx)
	stop()
	work.Wait()
	return nil
}

// consumeFeed runs the feed until ctx ends, restarting it with a backoff when it fails or
// panics.
func (e *Engine) consumeFeed(ctx context.Context) {
	var failures atomic.Int64 // the feed may call onResync from a goroutine of its own
	connected := func() {
		failures.Store(0)
		e.resynchronize()
	}
	for ctx.Err() == nil {
		started := time.Now()
		err := recovering(func() error { return e.options.ChangeFeed.Run(ctx, e.route, connected) })
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("livewire: the change feed returned before its context ended")
		}
		e.observe(ctx, Outcome{Kind: OutcomeFeed, Status: http.StatusInternalServerError, Err: err, Duration: time.Since(started)})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff(e.options.RetryInterval, int(failures.Add(1)-1))):
		}
	}
}

// tick runs the periodic work until ctx ends or the engine closes; a panic in one tick's
// work costs that tick.
func (e *Engine) tick(ctx context.Context) {
	revalidate := time.NewTicker(e.options.RevalidateInterval)
	defer revalidate.Stop()
	safety := time.NewTicker(e.options.SafetyTickInterval)
	defer safety.Stop()
	presence := time.NewTicker(e.options.PresenceHeartbeat)
	defer presence.Stop()
	for {
		var work func()
		select {
		case <-ctx.Done():
			return
		case <-e.done:
			return
		case <-revalidate.C:
			work = e.RevalidateAll
		case <-safety.C:
			work = e.markTimeDependent
		case <-presence.C:
			work = func() { e.refreshPresences(ctx) }
		}
		if err := recovering(func() error { work(); return nil }); err != nil {
			e.observe(ctx, Outcome{Kind: OutcomeHeartbeat, Status: http.StatusInternalServerError, Err: err})
		}
	}
}

// backoff is the delay before attempt failures+1: base doubling per failure up to
// maximumRetryDoublings, plus up to 20 % jitter.
func backoff(base time.Duration, failures int) time.Duration {
	delay := base << min(failures, maximumRetryDoublings)
	return delay + rand.N(delay/5+1) //nolint:gosec // jitter spreads retries, it guards nothing
}

// recovering runs work and turns a panic into its error (CON-001).
func recovering(work func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("livewire: panic: %v\n%s", recovered, debug.Stack())
		}
	}()
	return work()
}

// Close ends every socket with CloseGoingAway, stops Run, refuses new sockets with 503 and
// waits until every socket's goroutines and Run ended or ctx is done, which it returns then;
// a close frame a client does not take is given up at ctx's deadline at the latest.
// Close may be called more than once.
func (e *Engine) Close(ctx context.Context) error {
	e.closeOnce.Do(func() { close(e.done) })
	e.mutex.Lock()
	e.closing = true
	open := e.openSockets()
	e.mutex.Unlock()
	closeFrameDeadline := time.Now().Add(closeFrameTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(closeFrameDeadline) {
		closeFrameDeadline = deadline
	}
	var closing sync.WaitGroup
	for _, clientSocket := range open {
		closing.Go(func() {
			_ = recovering(func() error { // a panic here is already contained; the socket is closed or closing
				clientSocket.closeWithin(CloseGoingAway, "the server is shutting down", closeFrameDeadline)
				return nil
			})
		})
	}
	ended := make(chan struct{})
	go func() { // only waits; nothing in it can panic
		closing.Wait()
		e.lifetimes.Wait()
		close(ended)
	}()
	select {
	case <-ended:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("livewire: close the engine: %w", ctx.Err())
	}
}

// RevalidateAll re-validates the identity of every socket now, through the IdentitySource
// (section 6, push-driven revocation): a gone identity closes its socket 4401, a changed one
// re-runs its subscriptions. It returns at once; the checks run on each socket's own
// goroutine.
func (e *Engine) RevalidateAll() {
	e.mutex.Lock()
	open := e.openSockets()
	e.mutex.Unlock()
	for _, clientSocket := range open {
		clientSocket.requestRevalidation()
	}
}

// RevalidateOwner is RevalidateAll for the sockets whose effective or real identity has the
// owner key (OwnerKey).
func (e *Engine) RevalidateOwner(key string) {
	e.mutex.Lock()
	open := e.openSockets()
	e.mutex.Unlock()
	for _, clientSocket := range open {
		if clientSocket.concerns(key) {
			clientSocket.requestRevalidation()
		}
	}
}

// openSockets lists the open sockets; e.mutex is held.
func (e *Engine) openSockets() []*socket {
	open := make([]*socket, 0, len(e.sockets))
	for clientSocket := range e.sockets {
		open = append(open, clientSocket)
	}
	return open
}

// inSlot runs work holding a query slot (D52), released whatever work does.
func (e *Engine) inSlot(ctx context.Context, work func() error) error {
	select {
	case e.queries <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("livewire: wait for a query slot: %w", ctx.Err())
	}
	defer func() { <-e.queries }()
	if err := ctx.Err(); err != nil { // a slot and the cancellation were both ready; the work is over
		return fmt.Errorf("livewire: wait for a query slot: %w", err)
	}
	return work()
}

// index makes a subscription reachable by the changes of each of its tables in that
// table's partition (D25) and by the safety tick when it is time-dependent. An ended
// subscription is left out: its unindex may have run already.
func (e *Engine) index(liveSubscription *subscription) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.indexLocked(liveSubscription)
}

// repartition moves a subscription's declaration tables to another partition, the presence
// table staying in the tenant; the next change of the new partition reaches it.
func (e *Engine) repartition(liveSubscription *subscription, tables []string, partition string) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.unindexLocked(liveSubscription)
	for _, table := range tables {
		liveSubscription.partitions[table] = partition
	}
	e.indexLocked(liveSubscription)
}

// indexLocked is index; e.mutex is held.
func (e *Engine) indexLocked(liveSubscription *subscription) {
	if liveSubscription.isEnded() {
		return
	}
	for table, partition := range liveSubscription.partitions {
		partitions := e.byTable[table]
		if partitions == nil {
			partitions = map[string]map[*subscription]struct{}{}
			e.byTable[table] = partitions
		}
		if partitions[partition] == nil {
			partitions[partition] = map[*subscription]struct{}{}
		}
		partitions[partition][liveSubscription] = struct{}{}
	}
	if liveSubscription.timeDependent {
		e.timeDependent[liveSubscription] = struct{}{}
	}
}

// unindex removes a subscription; no change reaches it afterwards.
func (e *Engine) unindex(liveSubscription *subscription) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.unindexLocked(liveSubscription)
}

// unindexLocked is unindex; e.mutex is held.
func (e *Engine) unindexLocked(liveSubscription *subscription) {
	for table, partition := range liveSubscription.partitions {
		partitions := e.byTable[table]
		delete(partitions[partition], liveSubscription)
		if len(partitions[partition]) == 0 {
			delete(partitions, partition)
		}
		if len(partitions) == 0 {
			delete(e.byTable, table)
		}
	}
	delete(e.timeDependent, liveSubscription)
}

// route marks dirty every subscription a change reaches: a change without a partition
// reaches every subscription of its table, one with a partition the subscriptions of that
// partition and those without one.
func (e *Engine) route(change Change) {
	e.mutex.Lock()
	partitions := e.byTable[change.Table]
	var reached []*subscription
	if change.Partition == "" {
		for _, subscriptions := range partitions {
			reached = appendSubscriptions(reached, subscriptions)
		}
	} else {
		reached = appendSubscriptions(reached, partitions[change.Partition])
		reached = appendSubscriptions(reached, partitions[""])
	}
	e.mutex.Unlock()
	for _, liveSubscription := range reached {
		liveSubscription.markChanged(change.Table)
	}
}

func appendSubscriptions(reached []*subscription, subscriptions map[*subscription]struct{}) []*subscription {
	for liveSubscription := range subscriptions {
		reached = append(reached, liveSubscription)
	}
	return reached
}

// resynchronize re-runs every subscription with a snapshot: the feed (re)connected and may
// have missed changes.
func (e *Engine) resynchronize() {
	e.mutex.Lock()
	var all []*subscription
	for _, partitions := range e.byTable {
		for _, subscriptions := range partitions {
			all = appendSubscriptions(all, subscriptions)
		}
	}
	e.mutex.Unlock()
	for _, liveSubscription := range all {
		liveSubscription.markResynchronize()
	}
}

// markTimeDependent re-runs the subscriptions whose filter carries a relative time.
func (e *Engine) markTimeDependent() {
	e.mutex.Lock()
	reached := appendSubscriptions(nil, e.timeDependent)
	e.mutex.Unlock()
	for _, liveSubscription := range reached {
		liveSubscription.markDirty()
	}
}

// clock is the time the delivery batch waits on; the engine's tests replace it.
type clock interface {
	Now() time.Time
	AfterFunc(delay time.Duration, work func()) func() bool
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(delay time.Duration, work func()) func() bool {
	return time.AfterFunc(delay, work).Stop
}
