package ginadapter

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/govengers/livewire"
)

// Options configures RegisterRoutes. Engine is required: every route runs through its read
// and write pipelines, and its registry names the routes. MaximumBodyBytes bounds the JSON
// body of a mutation, a batch and a stored filter (default 1 MiB, not negative); a larger
// one is 400 invalid_request at "body". An upload is bounded by its declared MaximumBytes.
type Options struct {
	Engine           *livewire.Engine
	MaximumBodyBytes int64
}

const defaultMaximumBodyBytes = 1 << 20

// RegisterRoutes mounts every REST route of the engine's registry on router, below the
// registry's BasePath, with `{name}` placeholders as gin's `:name`:
//
//   - a resource: GET Path (list), ItemPath, DistinctPath, HistoryPath, PresencePath and
//     ExportPath as far as it declares them;
//   - a list: GET Path; an object: GET Path and PresencePath;
//   - a mutation: its method and Path; an upload: POST Path;
//   - the library's own: POST BatchPath, POST and GET FiltersPath, DELETE FiltersPath/:id.
//
// Every read is an Engine.Read of the ReadRequest the socket's query sends for the same
// target (D46), every write an Engine.Call, Batch or Upload, so a REST body is byte for byte
// the socket's payload (D23). A mutation and an upload run their static Check before the
// body is read (D8): a refused request reads no body and is answered by the engine, which
// runs the same Check first and reports the outcome. Mutations and batches run on a context
// the client's disconnect does not cancel, so a write that started finishes as what it is.
// The Idempotency-Key header is the call's key (D16), the X-Request-ID the wide-event
// middleware answers with is its request id (D31). Every refusal and failure is answered
// with livewire.ResponseFor's status and body. Each request's outcome is recorded on the
// request's wide event (D4): livewire.target, livewire.kind, livewire.read for a read kind,
// livewire.status, livewire.code on a refusal, livewire.rows for an export, and the event's
// success or failure.
//
// It takes its dependencies and settings in one Options value, validated and copied
// (DES-001); it is a function, not a component, because it builds nothing that outlives the
// call but the handlers, which close over the copy. A route gin refuses (one that conflicts
// with a route already on router) is returned as the error, with the routes registered
// before it left in place; register on a router of the API's own.
func RegisterRoutes(router gin.IRoutes, options *Options) error {
	if router == nil || options == nil || options.Engine == nil {
		return errors.New("ginadapter: RegisterRoutes needs a router and options with an Engine")
	}
	resolved := *options
	switch {
	case resolved.MaximumBodyBytes < 0:
		return errors.New("ginadapter: Options.MaximumBodyBytes must not be negative")
	case resolved.MaximumBodyBytes == 0:
		resolved.MaximumBodyBytes = defaultMaximumBodyBytes
	}
	handlers := &handlers{options: resolved, engine: resolved.Engine}
	return handlers.register(router, resolved.Engine.Registry())
}

// SocketHandler is the engine's socket endpoint as a gin handler; mount it behind the same
// authentication as the REST routes. The engine must not be nil.
func SocketHandler(engine *livewire.Engine) gin.HandlerFunc {
	return gin.WrapH(engine.Handler())
}

// register mounts the routes of every declaration, then the library's own; a panic of gin's
// router, which is how it refuses a route, is returned as the error.
func (h *handlers) register(router gin.IRoutes, registry *livewire.Registry) (err error) {
	current := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("ginadapter: register %s: %v", current, recovered)
		}
	}()
	handle := func(method, path string, handler gin.HandlerFunc) {
		current = method + " " + registry.BasePath() + path
		router.Handle(method, ginPath(registry.BasePath()+path), handler)
	}
	for _, declaration := range registry.Declarations() {
		h.registerDeclaration(handle, declaration)
	}
	handle(http.MethodPost, livewire.BatchPath, h.batch)
	handle(http.MethodPost, livewire.FiltersPath, h.saveFilter)
	handle(http.MethodGet, livewire.FiltersPath, h.listFilters)
	handle(http.MethodDelete, livewire.FiltersPath+"/{id}", h.deleteFilter)
	return nil
}

type handleFunc func(method, path string, handler gin.HandlerFunc)

func (h *handlers) registerDeclaration(handle handleFunc, declaration livewire.Declaration) {
	name := declaration.Name()
	switch typed := declaration.(type) {
	case *livewire.Resource:
		handle(http.MethodGet, typed.Path(), h.read(name, livewire.ReadMain))
		reads := []struct {
			path string
			kind livewire.ReadKind
		}{
			{typed.ItemPath(), livewire.ReadItem},
			{typed.DistinctPath(), livewire.ReadDistinct},
			{typed.HistoryPath(), livewire.ReadHistory},
			{typed.PresencePath(), livewire.ReadPresence},
		}
		for _, read := range reads {
			if read.path != "" {
				handle(http.MethodGet, read.path, h.read(name, read.kind))
			}
		}
		if path := typed.ExportPath(); path != "" {
			handle(http.MethodGet, path, h.export(name))
		}
	case *livewire.List:
		handle(http.MethodGet, typed.Path(), h.read(name, livewire.ReadMain))
	case *livewire.Object:
		handle(http.MethodGet, typed.Path(), h.read(name, livewire.ReadMain))
		handle(http.MethodGet, typed.PresencePath(), h.read(name, livewire.ReadPresence))
	case *livewire.Mutation:
		handle(typed.Method(), typed.Path(), h.call(typed))
	case *livewire.Upload:
		handle(typed.Method(), typed.Path(), h.upload(typed))
	}
}

// ginPath writes the `{name}` placeholders of a livewire path as gin's `:name`.
func ginPath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		if name, ok := strings.CutPrefix(segment, "{"); ok {
			segments[index] = ":" + strings.TrimSuffix(name, "}")
		}
	}
	return strings.Join(segments, "/")
}
