package database

import (
	"context"
	"sync"
	"time"

	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
)

// QueryCount sums the queries of one unit of work (a REST request, a socket frame, a
// re-run) for its wide event (LOG-004, LOG-006): the pool's tracer adds every query it
// traces on a context that carries one.
type QueryCount struct {
	mutex    sync.Mutex
	queries  int
	failed   int
	duration time.Duration
}

type queryCountKey struct{}

// CountQueries starts counting the queries that run on ctx; Record writes the sums.
func CountQueries(ctx context.Context) (context.Context, *QueryCount) {
	count := &QueryCount{}
	return context.WithValue(ctx, queryCountKey{}, count), count
}

// Record writes db.queries, db.failed and db.duration on the event.
func (c *QueryCount) Record(we *wideevent.Event) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	we.Int("db.queries", c.queries).Int("db.failed", c.failed).Duration("db.duration", c.duration)
}

// queryTracer is the pgx tracer that counts on the context's QueryCount.
type queryTracer struct{}

type queryStartKey struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStartKey{}, time.Now())
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	count, ok := ctx.Value(queryCountKey{}).(*QueryCount)
	started, timed := ctx.Value(queryStartKey{}).(time.Time)
	if !ok || !timed {
		return
	}
	count.mutex.Lock()
	defer count.mutex.Unlock()
	count.queries++
	count.duration += time.Since(started)
	if data.Err != nil {
		count.failed++
	}
}
