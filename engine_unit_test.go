package livewire_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

// unitKey carries the number of the unit of work StartUnit began (D92).
type unitKey struct{}

// unitLog is a StartUnit that numbers the units it begins and records their finishes, and
// which outcome arrived after its unit finished.
type unitLog struct {
	mutex    sync.Mutex
	started  []livewire.Unit
	finished []int
}

func (l *unitLog) start(ctx context.Context, unit livewire.Unit) (context.Context, func()) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.started = append(l.started, unit)
	number := len(l.started)
	return context.WithValue(ctx, unitKey{}, number), func() {
		l.mutex.Lock()
		defer l.mutex.Unlock()
		l.finished = append(l.finished, number)
	}
}

// unitOf is the number of the first unit begun for kind, operation and target, 0 for none.
func (l *unitLog) unitOf(kind livewire.OutcomeKind, operation livewire.Operation, target string) int {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return slices.Index(l.started, livewire.Unit{Kind: kind, Operation: operation, Target: target}) + 1
}

// begun is a copy of the units begun so far.
func (l *unitLog) begun() []livewire.Unit {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return slices.Clone(l.started)
}

func (l *unitLog) isFinished(number int) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return slices.Contains(l.finished, number)
}

// unitsOf are the units the outcomes of kind were observed in.
func (o *outcomes) unitsOf(kind livewire.OutcomeKind) []any {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	var units []any
	for position, outcome := range o.received {
		if outcome.Kind == kind {
			units = append(units, o.units[position])
		}
	}
	return units
}

func (f *fakeTransactor) unitsBegun() []any {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Clone(f.units)
}

func TestTheEngineBeginsEveryUnitOfWorkItRunsOnItsOwn(t *testing.T) {
	log := &unitLog{}
	bed := newTestbedWith(t, livewire.RegistryOptions{StartUnit: log.start}, nil)
	socket := bed.dial("ada")
	require.Eventually(t, func() bool { return log.unitOf(livewire.OutcomeOpen, "", "") > 0 }, frameWait, time.Millisecond)

	queried := socket.do(livewire.OperationQuery, "gadgets", nil, nil)
	require.True(t, queried.OK)
	query := log.unitOf(livewire.OutcomeRequest, livewire.OperationQuery, "gadgets")
	require.NotZero(t, query, "a socket query is a unit of its own")
	require.Contains(t, bed.transactor.unitsBegun(), query, "the read's transaction, so its loader, runs in the unit")
	require.Contains(t, bed.outcomes.unitsOf(livewire.OutcomeRequest), query, "the outcome is observed in the unit")
	require.Eventually(t, func() bool { return log.isFinished(query) }, frameWait, time.Millisecond, "and the unit finishes")

	id := subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	socket.push()
	created := socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, firstKey)
	require.True(t, created.OK, "%+v", created.Error)
	call := log.unitOf(livewire.OutcomeCall, livewire.OperationCall, "createGadget")
	committed := log.unitOf(livewire.OutcomeAfterCommit, "", "createGadget")
	require.NotZero(t, call)
	require.NotZero(t, committed, "the after-commit report is a unit of its own")
	require.Contains(t, bed.transactor.unitsBegun(), call, "the handler runs in the call's unit")
	require.Equal(t, []any{call}, bed.outcomes.unitsOf(livewire.OutcomeCall))
	require.Equal(t, []any{committed}, bed.outcomes.unitsOf(livewire.OutcomeAfterCommit))

	require.Equal(t, livewire.PushListChange, socket.push().Kind)
	require.Eventually(t, func() bool {
		return slices.Contains(log.begun(), livewire.Unit{Kind: livewire.OutcomeRerun, Target: "gadgets", Subscription: id})
	}, frameWait, time.Millisecond, "a re-run is a unit of its own, named by its subscription")
	require.Eventually(t, func() bool { return len(bed.outcomes.unitsOf(livewire.OutcomeRerun)) == 1 }, frameWait, time.Millisecond)
	require.NotNil(t, bed.outcomes.unitsOf(livewire.OutcomeRerun)[0], "the re-run is observed in its unit")

	identified := auth.WithIdentity(t.Context(), person("ada", roleReader, roleWriter))
	before := len(log.begun())
	_, err := bed.engine.Read(identified, livewire.ReadRequest{Target: "gadgets"})
	require.NoError(t, err)
	_, _, err = bed.engine.Call(identified, livewire.CallRequest{Target: "createGadget", Body: mustJSON(map[string]string{"name": "Bolt"})})
	require.NoError(t, err)
	since := slices.DeleteFunc(log.begun()[before:], func(unit livewire.Unit) bool { return unit.Kind == livewire.OutcomeRerun }) // the subscription's
	require.Equal(t, []livewire.Unit{{Kind: livewire.OutcomeAfterCommit, Target: "createGadget"}}, since,
		"a transport's read and call run on its context; only the after-commit report is begun")

	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool { return log.unitOf(livewire.OutcomeClose, "", "") > 0 }, frameWait, time.Millisecond)
	require.Eventually(t, func() bool {
		log.mutex.Lock()
		defer log.mutex.Unlock()
		return len(log.finished) == len(log.started)
	}, frameWait, time.Millisecond, "every unit begun finishes")
	for kind, units := range map[livewire.OutcomeKind][]any{livewire.OutcomeOpen: bed.outcomes.unitsOf(livewire.OutcomeOpen), livewire.OutcomeEnd: bed.outcomes.unitsOf(livewire.OutcomeEnd)} {
		require.NotEmpty(t, units, kind)
		require.NotContains(t, units, nil, "every outcome of a socket is observed in a unit: %s", kind)
	}
}

func TestAPanickingStartUnitCostsItsUnitOnly(t *testing.T) {
	bed := newTestbedWith(t, livewire.RegistryOptions{StartUnit: func(context.Context, livewire.Unit) (context.Context, func()) {
		panic("the unit starter panicked")
	}}, nil)
	socket := bed.dial("ada")
	require.True(t, socket.do(livewire.OperationQuery, "gadgets", nil, nil).OK)
	require.True(t, socket.call("createGadget", nil, map[string]string{"name": "Anchor"}, "").OK, "the work runs on the context it came with")

	finishing := newTestbedWith(t, livewire.RegistryOptions{StartUnit: func(context.Context, livewire.Unit) (context.Context, func()) {
		return nil, func() { panic("the finish panicked") }
	}}, nil)
	finishingSocket := finishing.dial("ada")
	answered := finishingSocket.do(livewire.OperationQuery, "gadgets", nil, nil)
	require.True(t, answered.OK, "no context is the context the unit came with, a panicking finish is dropped")
	require.Equal(t, http.StatusOK, answered.Status)
}
