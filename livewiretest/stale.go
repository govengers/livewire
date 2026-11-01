package livewiretest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/internal/listkey"
)

// Write is one committed write of a fixture the application makes to a table, in its own
// committed transaction, the way its repositories or a job write (a mutation through the
// application's own code, a sqlc query on the pool): Commit returns once it committed.
type Write struct {
	Table  string
	Commit func(ctx context.Context) error
}

// StaleOptions configures Stale. Application, Authenticate (as for ParityOptions) and Writes
// are required: Writes needs a write for every table a live declaration lists in Tables, each
// of a row the fixture identity reads, so it changes a live read of a declaration that lists
// the table. A write to a table no live declaration lists may change nothing; what it changes
// must re-run like any other change, so it is how a test proves that a loader reading an
// undeclared table goes stale. Reads gives the parameters of the live reads that need them, as
// ParityOptions.Reads does. Timeout bounds the wait for a subscription to catch up with one
// write (default 5 s).
type StaleOptions struct {
	Application  Application
	Authenticate func(request *http.Request)
	Reads        []Read
	Writes       []Write
	Timeout      time.Duration
}

// defaultStaleTimeout is how long Stale waits for the subscriptions after a write by default.
const defaultStaleTimeout = 5 * time.Second

// Stale subscribes to every live read of the registry (presence reads aside, whose table is
// the library's own) and runs the writes one after another (D23). After each it reads every
// subscribed read over REST and fails the test, naming the declaration, for every subscription
// whose state does not reach that REST body within the timeout: a write the subscription
// should have re-run on and did not, because the declaration does not list the table the
// write changed, or the table sends no notification (TriggerSQL missing), or the change feed
// does not reach the engine. A write to a declared table that changes no live read of a
// declaration listing it fails too: it proves nothing about that table.
//
// What it cannot catch: a table no Write writes. It requires a write for every declared table,
// but no gate can discover a table a loader reads without declaring it; give the writes of
// every table the loaders read, declared or not, and Stale catches the undeclared ones. It
// also checks only what the fixture identity reads, through the reads it is given.
func Stale(tb testing.TB, options *StaleOptions) {
	tb.Helper()
	if options == nil {
		tb.Fatalf("livewiretest: Stale needs options")
		return
	}
	server := startServer(tb, "Stale", &options.Application, options.Authenticate)
	defer server.close()
	targets := slices.DeleteFunc(readTargets(options.Application.Engine), func(target readTarget) bool {
		return !target.live || target.kind == livewire.ReadPresence
	})
	requireWrites(tb, targets, options.Writes)
	reads := resolveReads(tb, "Stale", targets, options.Reads)
	ctx := context.Background()
	connection, err := server.dial(ctx)
	if err != nil {
		tb.Fatalf("livewiretest: Stale: %v", err)
		return
	}
	defer connection.close()
	subscriptions, err := subscribeAll(ctx, server, connection, reads)
	if err != nil {
		tb.Fatalf("livewiretest: Stale: %v", err)
		return
	}
	run := &staleRun{server: server, connection: connection, subscriptions: subscriptions, timeout: options.Timeout, declared: declaredTables(targets)}
	if run.timeout <= 0 {
		run.timeout = defaultStaleTimeout
	}
	for _, write := range options.Writes {
		run.checkWrite(ctx, tb, write)
	}
}

// staleRun is one run of Stale: the server, the socket and its subscriptions.
type staleRun struct {
	server        *testServer
	connection    *socket
	subscriptions []*liveRead
	timeout       time.Duration
	declared      map[string]bool // the tables a live declaration lists
}

// declaredTables are the tables the live targets' declarations list.
func declaredTables(targets []readTarget) map[string]bool {
	declared := map[string]bool{}
	for _, target := range targets {
		for _, table := range target.declaration.Tables() {
			declared[table] = true
		}
	}
	return declared
}

// requireWrites fails the gate for every table a live declaration lists without a write.
func requireWrites(tb testing.TB, targets []readTarget, writes []Write) {
	tb.Helper()
	var missing []string
	for _, target := range targets {
		for _, table := range target.declaration.Tables() {
			written := slices.ContainsFunc(writes, func(write Write) bool { return write.Table == table })
			if !written && !slices.Contains(missing, table) {
				missing = append(missing, table)
				tb.Errorf("livewiretest: Stale: no Write for the table %s, which %s lists in its Tables", table, target.declaration.Name())
			}
		}
	}
	for _, write := range writes {
		if write.Commit == nil {
			tb.Fatalf("livewiretest: Stale: the Write of %s has no Commit", write.Table)
		}
	}
}

// liveRead is one subscription Stale keeps: its read, its id and the state its pushes built.
type liveRead struct {
	read         resolvedRead
	subscription string
	state        json.RawMessage
}

// subscribeAll subscribes every read and waits until each subscription's state is its REST
// body, so every write starts from subscriptions that are current.
func subscribeAll(ctx context.Context, server *testServer, connection *socket, reads []resolvedRead) ([]*liveRead, error) {
	subscriptions := make([]*liveRead, 0, len(reads))
	for _, read := range reads {
		subscribed, err := connection.payload(livewire.OperationSubscribe, read)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", read.target.name(), err)
		}
		live := &liveRead{read: read, subscription: subscribed.Subscription}
		subscriptions = append(subscriptions, live)
		if err := live.catchUp(ctx, server, connection, time.Now().Add(requestTimeout)); err != nil {
			return nil, fmt.Errorf("%s before any write: %w", read.target.name(), err)
		}
	}
	return subscriptions, nil
}

// checkWrite commits one write and requires every subscription to catch up with its REST read
// and, for a table a live declaration lists, at least one read of one of those to have changed.
func (r *staleRun) checkWrite(ctx context.Context, tb testing.TB, write Write) {
	tb.Helper()
	before := make([]json.RawMessage, len(r.subscriptions))
	for index, live := range r.subscriptions {
		before[index] = live.state
	}
	if err := write.Commit(ctx); err != nil {
		tb.Errorf("livewiretest: Stale: the write to %s: %v", write.Table, err)
		return
	}
	deadline := time.Now().Add(r.timeout)
	changed := false
	for index, live := range r.subscriptions {
		err := live.catchUp(ctx, r.server, r.connection, deadline)
		if err != nil {
			tb.Errorf("livewiretest: Stale: %s %v did not re-run after the write to %s: %v",
				live.read.target.name(), live.read.parameters, write.Table, err)
			continue
		}
		listsTable := slices.Contains(live.read.target.declaration.Tables(), write.Table)
		changed = changed || (listsTable && !sameState(before[index], live.state, live.read.target.undiffed()))
	}
	if !changed && r.declared[write.Table] {
		tb.Errorf("livewiretest: Stale: the write to %s changed no live read of a declaration that lists it: write a row the fixture identity reads", write.Table)
	}
}

// catchUp reads the subscription's read over REST and applies its pushes until its state is
// that body or the deadline passed.
func (l *liveRead) catchUp(ctx context.Context, server *testServer, connection *socket, deadline time.Time) error {
	rest, err := server.read(ctx, l.read)
	if err != nil {
		return err
	}
	for l.state == nil || !sameState(l.state, rest, l.read.target.undiffed()) {
		push, err := connection.push(l.subscription, deadline)
		if errors.Is(err, errWaitedTooLong) {
			return fmt.Errorf("its pushed state stays behind its REST read\nREST:   %s\nsocket: %s", abbreviate(rest), abbreviate(l.state))
		}
		if err != nil {
			return err
		}
		if err := l.apply(push); err != nil {
			return err
		}
	}
	return nil
}

// apply builds the state after a push: a snapshot or a replace is the state, a diff changes
// the list (section 4), an error push ends the subscription.
func (l *liveRead) apply(push livewire.PushFrame) error {
	switch push.Kind {
	case livewire.PushSnapshot, livewire.PushReplace:
		l.state = push.Payload
		return nil
	case livewire.PushListChange:
		var change livewire.ListChange
		if err := json.Unmarshal(push.Payload, &change); err != nil {
			return fmt.Errorf("a diff push: %w", err)
		}
		state, err := applyChange(l.state, &change)
		l.state = state
		return err
	case livewire.PushError:
		return fmt.Errorf("the subscription ended with the error push %s", push.Payload)
	default:
		return fmt.Errorf("a push of the unknown kind %q", push.Kind)
	}
}

// listBody is a ListResponse with its items kept as JSON (D14).
type listBody struct {
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
	Total  int               `json:"total"`
	Items  []json.RawMessage `json:"items"`
	Next   *string           `json:"next,omitempty"`
}

// applyChange applies a diff to a list state as a client does: upserts by id, removes, the
// order when the diff carries it, the total when it carries one.
func applyChange(state json.RawMessage, change *livewire.ListChange) (json.RawMessage, error) {
	var list listBody
	if err := json.Unmarshal(state, &list); err != nil {
		return state, fmt.Errorf("a diff for a state that is no list: %w", err)
	}
	byID := map[string]json.RawMessage{}
	order := make([]string, 0, len(list.Items)+len(change.Upserts))
	for _, item := range slices.Concat(list.Items, change.Upserts) {
		id, err := listkey.ItemID(item)
		if err != nil {
			return state, err
		}
		if _, known := byID[id]; !known {
			order = append(order, id)
		}
		byID[id] = item
	}
	order = slices.DeleteFunc(order, func(id string) bool { return slices.Contains(change.Removes, id) })
	if change.Order != nil {
		order = change.Order
	}
	list.Items = make([]json.RawMessage, 0, len(order))
	for _, id := range order {
		item, known := byID[id]
		if !known {
			return state, fmt.Errorf("a diff orders the unknown id %s", id)
		}
		list.Items = append(list.Items, item)
	}
	if change.Total != nil {
		list.Total = *change.Total
	}
	return json.Marshal(list)
}

// undiffed are the members of a read's body that a diff does not carry, so a pushed state
// keeps the snapshot's: a resource list's and a history's next (D64), and a typed list's
// limit, the count of its items (D73). Every other read is compared whole.
func (r readTarget) undiffed() []string {
	switch r.declaration.(type) {
	case *livewire.Resource:
		if r.kind == livewire.ReadMain || r.kind == livewire.ReadHistory {
			return []string{"next"}
		}
	case *livewire.List:
		return []string{"limit"}
	default:
	}
	return nil
}

// sameState reports whether a pushed state and a REST body carry the same JSON, the members a
// diff does not carry aside.
func sameState(state, rest json.RawMessage, undiffed []string) bool {
	if bytes.Equal(state, rest) {
		return true
	}
	stateValue, stateError := decodedJSON(state, undiffed)
	restValue, restError := decodedJSON(rest, undiffed)
	return stateError == nil && restError == nil && reflect.DeepEqual(stateValue, restValue)
}

func decodedJSON(body json.RawMessage, undiffed []string) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if object, isObject := value.(map[string]any); isObject {
		for _, member := range undiffed {
			delete(object, member)
		}
	}
	return value, nil
}
