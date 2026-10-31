package livewire

import (
	"sync"
	"time"
)

// outbox is what a socket's writer has to send: the queued frames (responses, first pushes,
// error pushes) in order, and the subscriptions with a pending push. A pending push is
// computed when it leaves, from what the subscription delivered last, so the changes of a
// burst merge into one push per subscription. The pending set leaves at most wait after its
// first mark: at once when the last delivery is longer than wait ago, else when wait since
// the last delivery has passed, so a constant stream of changes still leaves once per wait.
type outbox struct {
	limit int
	wait  time.Duration
	clock clock
	// wake has one slot: the writer has frames to send or a delivery is due.
	wake chan struct{}

	mutex       sync.Mutex
	closed      bool
	frames      [][]byte
	queuedBytes int
	pending     map[*subscription]struct{}
	marked      bool
	lastFlush   time.Time
	flushDue    bool
	stopTimer   func() bool
}

func newOutbox(limit int, wait time.Duration, clock clock) *outbox {
	return &outbox{limit: limit, wait: wait, clock: clock, wake: make(chan struct{}, 1), pending: map[*subscription]struct{}{}}
}

// enqueue queues frames in order and wakes the writer. It reports false when the frames
// already waiting are past the limit: the writer fell behind by more than the limit, the
// client does not read what it asks for, a slow consumer (D10). One frame larger than the
// limit alone is no sign of a slow client and is sent.
func (o *outbox) enqueue(frames ...[]byte) bool {
	o.mutex.Lock()
	if o.closed {
		o.mutex.Unlock()
		return true
	}
	withinLimit := o.queuedBytes <= o.limit
	o.frames = append(o.frames, frames...)
	for _, frame := range frames {
		o.queuedBytes += len(frame)
	}
	o.mutex.Unlock()
	o.signal()
	return withinLimit
}

// markPending asks for the subscription's push within the delivery wait.
func (o *outbox) markPending(liveSubscription *subscription) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if o.closed {
		return
	}
	o.pending[liveSubscription] = struct{}{}
	if o.marked {
		return
	}
	o.marked = true
	if delay := o.lastFlush.Add(o.wait).Sub(o.clock.Now()); delay > 0 {
		o.stopTimer = o.clock.AfterFunc(delay, o.flushNow)
		return
	}
	o.flushDue = true
	o.signal()
}

func (o *outbox) flushNow() {
	o.mutex.Lock()
	o.flushDue = true
	o.mutex.Unlock()
	o.signal()
}

func (o *outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// take empties the queue and, when a delivery is due, the pending set.
func (o *outbox) take() (frames [][]byte, due []*subscription) {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	frames, o.frames, o.queuedBytes = o.frames, nil, 0
	if !o.flushDue {
		return frames, nil
	}
	for liveSubscription := range o.pending {
		due = append(due, liveSubscription)
	}
	clear(o.pending)
	o.marked, o.flushDue, o.lastFlush, o.stopTimer = false, false, o.clock.Now(), nil
	return frames, due
}

// shut drops everything and refuses what comes later.
func (o *outbox) shut() {
	o.mutex.Lock()
	defer o.mutex.Unlock()
	o.closed = true
	o.frames, o.queuedBytes = nil, 0
	clear(o.pending)
	if o.stopTimer != nil {
		o.stopTimer()
		o.stopTimer = nil
	}
}
