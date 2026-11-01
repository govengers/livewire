// Package server builds the example's HTTP side: the gin router with the wide-event
// middleware, the auth routes, the API behind authentication with livewire's REST routes and
// socket, the health routes, and the livewire Observer that turns the engine's outcomes into
// metrics and one wide event per socket frame and background unit of work (D30, LOG-001).
package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	authginadapter "github.com/govengers/auth/ginadapter"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/prometheusadapter"

	"github.com/govengers/livewire/examples/todo/internal/authentication"
	"github.com/govengers/livewire/examples/todo/internal/controller"
)

// SocketPath is the route of the live socket.
const SocketPath = controller.BasePath + "/socket"

// Options configures the router; every field is required.
type Options struct {
	Engine         *livewire.Engine
	Authentication *authentication.Authentication
	Emitter        wideevent.Emitter
	Ready          func(ctx context.Context) error // the readiness check, a database ping
}

// NewRouter builds the router of the API.
func NewRouter(options *Options) (*gin.Engine, error) {
	if options == nil || options.Engine == nil || options.Authentication == nil || options.Emitter == nil || options.Ready == nil {
		return nil, errors.New("server: Options needs the Engine, the Authentication, the Emitter and Ready")
	}
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{Emitter: options.Emitter, SkipPaths: []string{"/healthz", "/readyz"}})
	if err != nil {
		return nil, err
	}
	router := gin.New()
	router.Use(gin.Recovery(), wideEvents)
	router.GET("/healthz", func(ginContext *gin.Context) { ginContext.Status(http.StatusNoContent) })
	router.GET("/readyz", func(ginContext *gin.Context) {
		if err := options.Ready(ginContext.Request.Context()); err != nil {
			ginContext.Status(http.StatusServiceUnavailable)
			return
		}
		ginContext.Status(http.StatusNoContent)
	})
	authginadapter.RegisterRoutes(router, options.Authentication.Routes())
	api := router.Group("", authginadapter.Middleware(options.Authentication.Middleware))
	if err := ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: options.Engine}); err != nil {
		return nil, err
	}
	api.GET(SocketPath, ginadapter.SocketHandler(options.Engine))
	return router, nil
}

// NewObserver is the registry's Observer (D49): every outcome feeds the metrics; an outcome
// without a request's wide event (a socket frame, a re-run, the feed, a heartbeat) is a unit of
// work of its own and gets one event. A REST outcome is on the request's event already, which
// the REST adapter recorded.
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
			String("livewire.tenant", outcome.Tenant).
			Int("livewire.status", outcome.Status).
			Duration("livewire.duration", outcome.Duration).
			Duration("livewire.query_wait", outcome.QueryWait)
		if outcome.Identity != nil {
			we.UserID(outcome.Identity.UserID)
		}
		if outcome.Err != nil {
			we.Failure(outcome.Err)
		} else {
			we.Success()
		}
		we.Emit()
	}
}
