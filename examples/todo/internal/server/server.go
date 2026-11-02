// Package server builds the example's HTTP side: the gin router with its recovery, the
// wide-event middleware and the request metrics, the auth routes, the API behind
// authentication with livewire's REST routes and socket, the health routes, and the livewire
// Observer that turns the engine's outcomes into metrics and one wide event per socket or
// background outcome (D30, LOG-001).
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/govengers/auth"
	authginadapter "github.com/govengers/auth/ginadapter"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/prometheusadapter"

	"github.com/govengers/livewire/examples/todo/internal/api"
	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/controller"
)

// SocketPath is the route of the live socket.
const SocketPath = controller.BasePath + "/socket"

// Options configures the router; every field is required. Ready is the readiness check (a
// database ping that fails once the server drains), Metrics the registry the request metrics
// go to.
type Options struct {
	Engine         *livewire.Engine
	Authentication *authentication.Authentication
	Emitter        wideevent.Emitter
	Metrics        prometheus.Registerer
	Ready          func(ctx context.Context) error
}

// NewRouter builds the router of the API; it fails without one of its options or when the
// request metrics are registered already.
func NewRouter(options *Options) (*gin.Engine, error) {
	if options == nil || options.Engine == nil || options.Authentication == nil || options.Emitter == nil || options.Metrics == nil || options.Ready == nil {
		return nil, errors.New("server: Options needs the Engine, the Authentication, the Emitter, the Metrics and Ready")
	}
	ready, engine := options.Ready, options.Engine
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{Emitter: options.Emitter, SkipPaths: []string{"/healthz", "/readyz"}})
	if err != nil {
		return nil, err
	}
	requestMetrics, err := newRequestMetrics(options.Metrics)
	if err != nil {
		return nil, err
	}
	router := gin.New()
	router.Use(recovery(), wideEvents, requestMetrics)
	router.NoRoute(func(ginContext *gin.Context) { writeError(ginContext, livewire.NotFound()) })
	router.GET("/healthz", func(ginContext *gin.Context) { ginContext.Status(http.StatusNoContent) })
	router.GET("/readyz", func(ginContext *gin.Context) {
		if err := ready(ginContext.Request.Context()); err != nil {
			ginContext.Status(http.StatusServiceUnavailable)
			return
		}
		ginContext.Status(http.StatusNoContent)
	})
	authginadapter.RegisterRoutes(router, options.Authentication.Routes())
	authenticated := authginadapter.Middleware(options.Authentication.Middleware)
	router.GET("/auth/me", authenticated, gin.WrapH(auth.IdentityHandler())) // who the session is, for the frontend's pair (D66)
	// snippet:begin routes
	api := router.Group("", authenticated)
	if err := ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: engine}); err != nil {
		return nil, err
	}
	socket := ginadapter.SocketHandler(engine)
	api.GET(SocketPath, func(ginContext *gin.Context) {
		// The socket's units of work are not the upgrade request: they get events of their own
		// (NewObserver), so nothing writes to the upgrade's event after it was emitted (LOG-008).
		ginContext.Request = ginContext.Request.WithContext(wideevent.NewContext(ginContext.Request.Context(), nil))
		socket(ginContext)
	})
	// snippet:end routes
	return router, nil
}

// snippet:begin recovery

// recovery answers a panic with the 500 error body and prints nothing, so no header, cookie or
// API key reaches a log (LOG-009); the wide-event middleware inside it recorded the panic. A
// handler that aborts with http.ErrAbortHandler (an export cut after its first byte, D60)
// panics on, so net/http drops the connection as the handler meant.
func recovery() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if err, isError := recovered.(error); isError && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			if !ginContext.Writer.Written() {
				writeError(ginContext, fmt.Errorf("panic: %v", recovered))
			}
			ginContext.Abort()
		}()
		ginContext.Next()
	}
}

// snippet:end recovery

// writeError answers the one error body (API-001).
func writeError(ginContext *gin.Context, err error) {
	status, body := livewire.ResponseFor(err)
	ginContext.AbortWithStatusJSON(status, body)
}

// methods are the label values of a request's method; any other method is "other" (MET-004).
var methods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}

// newRequestMetrics records the rate, the errors and the duration of every request by route
// template, method and status class, and the requests in flight (MET-003, MET-004). A request
// that panics counts as 5xx. The socket route is left out: its request lives as long as the
// socket, which prometheusadapter measures.
func newRequestMetrics(registerer prometheus.Registerer) (gin.HandlerFunc, error) {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "todo", Subsystem: "http", Name: "requests_total", Help: "HTTP requests by route, method and status class.",
	}, []string{"route", "method", "status"})
	durations := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "todo", Subsystem: "http", Name: "request_duration_seconds", Help: "HTTP request durations by route and method.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	inFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "todo", Subsystem: "http", Name: "requests_in_flight", Help: "HTTP requests being served.",
	})
	for _, collector := range []prometheus.Collector{requests, durations, inFlight} {
		if err := registerer.Register(collector); err != nil {
			return nil, err
		}
	}
	return func(ginContext *gin.Context) {
		route := ginContext.FullPath()
		if route == SocketPath {
			ginContext.Next()
			return
		}
		if route == "" {
			route = "unmatched" // a client's path never becomes a label
		}
		method := ginContext.Request.Method
		if !slices.Contains(methods, method) {
			method = "other"
		}
		started := time.Now()
		inFlight.Inc()
		defer func() {
			inFlight.Dec()
			status := ginContext.Writer.Status()
			recovered := recover()
			if recovered != nil {
				status = http.StatusInternalServerError
			}
			requests.WithLabelValues(route, method, strconv.Itoa(status/100)+"xx").Inc()
			durations.WithLabelValues(route, method).Observe(time.Since(started).Seconds())
			if recovered != nil {
				panic(recovered) // the recovery outside answers it
			}
		}()
		ginContext.Next()
	}, nil
}

// recordIdentifiers puts the business identifiers of a socket unit's decoded parameters on its
// event (LOG-003): the services record them on a REST request's event, a socket unit has none
// for them to write to. A create's id travels in its body, which an outcome does not carry.
func recordIdentifiers(we *wideevent.Event, parameters any) {
	switch typed := parameters.(type) {
	case *api.TodoParameters:
		we.UUID("todo.id", typed.ID)
	case *api.ActivityParameters:
		if typed.TodoID != nil {
			we.UUID("todo.id", *typed.TodoID)
		}
	default:
	}
}

// snippet:begin observer

// NewObserver is the registry's Observer (D49): every outcome feeds the metrics; an outcome
// without a request's wide event (a socket frame, a re-run, the feed, a heartbeat: the socket
// route takes the upgrade's event out of their context) is a unit of work of its own and gets
// one event. A REST outcome is on the request's event already, which the REST adapter recorded.
func NewObserver(metrics *prometheusadapter.Metrics, emitter wideevent.Emitter) livewire.Observer {
	return func(ctx context.Context, outcome livewire.Outcome) {
		metrics.Observe(ctx, outcome)
		if wideevent.FromContext(ctx) != nil {
			return
		}
		we := wideevent.Begin(emitter, "livewire."+string(outcome.Kind))
		we.String("livewire.kind", string(outcome.Kind)).
			String("livewire.operation", string(outcome.Operation)).
			String("livewire.target", outcome.Target).
			String("livewire.subscription", outcome.Subscription).
			String("livewire.tenant", outcome.Tenant).
			Int("livewire.status", outcome.Status).
			Duration("livewire.duration", outcome.Duration).
			Duration("livewire.query_wait", outcome.QueryWait)
		if outcome.Identity != nil {
			we.UserID(outcome.Identity.UserID)
		}
		recordIdentifiers(we, outcome.Parameters)
		if outcome.Err != nil {
			we.Failure(outcome.Err)
		} else {
			we.Success()
		}
		we.Emit()
	}
}

// snippet:end observer
