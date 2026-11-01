package ginadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventgin "github.com/govengers/wide-event-logger/ginadapter"

	"github.com/govengers/livewire"
)

// IdempotencyKeyHeader is the request header that carries a mutation's Idempotency-Key
// (D16).
const IdempotencyKeyHeader = "Idempotency-Key"

const jsonContentType = "application/json; charset=utf-8"

// handlers serves the routes of one RegisterRoutes call.
type handlers struct {
	options Options
	engine  *livewire.Engine
}

// source is the ParameterSource of a gin request: the route's path parameters and the query
// string, parsed once.
type source struct {
	parameters gin.Params
	query      url.Values
}

func sourceOf(ginContext *gin.Context) source {
	return source{parameters: ginContext.Params, query: ginContext.Request.URL.Query()}
}

// Path returns the path parameter's value.
func (s source) Path(name string) (string, bool) { return s.parameters.Get(name) }

// Query returns the query parameter's values.
func (s source) Query(name string) ([]string, bool) {
	values, ok := s.query[name]
	return values, ok
}

// unit is what a route records on the wide event.
type unit struct {
	target string
	kind   string
	read   livewire.ReadKind
}

func (h *handlers) read(name string, kind livewire.ReadKind) gin.HandlerFunc {
	recorded := unit{target: name, kind: "read", read: kind}
	return func(ginContext *gin.Context) {
		request := livewire.ReadRequest{Target: name, Kind: kind, Source: sourceOf(ginContext)}
		payload, err := h.engine.Read(ginContext.Request.Context(), request)
		answer(ginContext, recorded, http.StatusOK, payload, err)
	}
}

func (h *handlers) call(mutation *livewire.Mutation) gin.HandlerFunc {
	recorded := unit{target: mutation.Name(), kind: string(livewire.KindMutation)}
	return func(ginContext *gin.Context) {
		ctx := context.WithoutCancel(ginContext.Request.Context())
		request := livewire.CallRequest{
			Target: mutation.Name(), Source: sourceOf(ginContext),
			IdempotencyKey: ginContext.GetHeader(IdempotencyKeyHeader), RequestID: requestID(ginContext),
		}
		if mutation.Check()(ctx) == nil { // a refused call reads no body (D8); the engine answers it
			var err error
			if request.Body, err = h.readBody(ginContext); err != nil {
				answer(ginContext, recorded, 0, nil, err)
				return
			}
		}
		status, payload, err := h.engine.Call(ctx, request)
		answer(ginContext, recorded, status, payload, err)
	}
}

func (h *handlers) batch(ginContext *gin.Context) {
	recorded := unit{target: "batch", kind: "batch"}
	body, err := h.readBody(ginContext)
	if err != nil {
		answer(ginContext, recorded, 0, nil, err)
		return
	}
	request := livewire.BatchRequest{Body: body, IdempotencyKey: ginContext.GetHeader(IdempotencyKeyHeader), RequestID: requestID(ginContext)}
	payload, err := h.engine.Batch(context.WithoutCancel(ginContext.Request.Context()), request)
	answer(ginContext, recorded, http.StatusOK, payload, err)
}

func (h *handlers) upload(upload *livewire.Upload) gin.HandlerFunc {
	recorded := unit{target: upload.Name(), kind: string(livewire.KindUpload)}
	return func(ginContext *gin.Context) {
		ctx := ginContext.Request.Context()
		request := livewire.UploadRequest{
			Target: upload.Name(), Source: sourceOf(ginContext),
			IdempotencyKey: ginContext.GetHeader(IdempotencyKeyHeader), RequestID: requestID(ginContext),
		}
		if upload.Check()(ctx) == nil { // a refused upload reads no body (D8); the engine answers it
			file, err := readFile(ginContext, upload)
			if err != nil {
				answer(ginContext, recorded, 0, nil, err)
				return
			}
			request.File = file
		}
		status, payload, err := h.engine.Upload(ctx, request)
		answer(ginContext, recorded, status, payload, err)
	}
}

const filtersTarget = "filters"

func (h *handlers) saveFilter(ginContext *gin.Context) {
	recorded := unit{target: filtersTarget, kind: filtersTarget}
	body, err := h.readBody(ginContext)
	if err != nil {
		answer(ginContext, recorded, 0, nil, err)
		return
	}
	payload, err := h.engine.SaveFilter(ginContext.Request.Context(), body)
	answer(ginContext, recorded, http.StatusCreated, payload, err)
}

func (h *handlers) listFilters(ginContext *gin.Context) {
	payload, err := h.engine.ListFilters(ginContext.Request.Context(), ginContext.Query("resource"))
	answer(ginContext, unit{target: filtersTarget, kind: filtersTarget}, http.StatusOK, payload, err)
}

func (h *handlers) deleteFilter(ginContext *gin.Context) {
	err := h.engine.DeleteFilter(ginContext.Request.Context(), ginContext.Param("id"))
	answer(ginContext, unit{target: filtersTarget, kind: filtersTarget}, http.StatusNoContent, nil, err)
}

// readBody reads a JSON body of at most MaximumBodyBytes.
func (h *handlers) readBody(ginContext *gin.Context) ([]byte, error) {
	limit := h.options.MaximumBodyBytes
	body, err := io.ReadAll(http.MaxBytesReader(ginContext.Writer, ginContext.Request.Body, limit))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, livewire.Invalid(map[string]string{"body": "larger than " + strconv.FormatInt(limit, 10) + " bytes"})
	case err != nil:
		return nil, livewire.Invalid(map[string]string{"body": "could not be read"})
	}
	return body, nil
}

// requestID is the request id the wide-event middleware answers with, "" without it; the
// engine makes one then.
func requestID(ginContext *gin.Context) string {
	return ginContext.Writer.Header().Get(wideeventgin.RequestIDHeader)
}

// answer writes the engine's answer, a JSON payload with its status (none for nil) or the
// error body, and records the outcome on the request's wide event.
func answer(ginContext *gin.Context, recorded unit, status int, payload json.RawMessage, err error) {
	switch {
	case err != nil:
		status = writeError(ginContext, err)
	case payload == nil:
		ginContext.Status(status)
	default:
		ginContext.Data(status, jsonContentType, payload)
	}
	record(ginContext, recorded, status, err)
}

// writeError answers err with livewire.ResponseFor's status and body and returns the status.
func writeError(ginContext *gin.Context, err error) int {
	status, body := livewire.ResponseFor(err)
	encoded, _ := json.Marshal(body) //nolint:errchkjson // an ErrorBody always marshals
	ginContext.Data(status, jsonContentType, encoded)
	return status
}

// record sets the request's livewire fields and its success or failure on its wide event;
// without the wide-event middleware it records nothing.
func record(ginContext *gin.Context, recorded unit, status int, err error) *wideevent.Event {
	we := wideeventgin.FromContext(ginContext)
	we.String("livewire.target", recorded.target).String("livewire.kind", recorded.kind).Int("livewire.status", status)
	if recorded.kind == "read" && recorded.read != livewire.ReadMain {
		we.String("livewire.read", string(recorded.read))
	}
	if err == nil {
		return we.Success()
	}
	_, body := livewire.ResponseFor(err)
	return we.String("livewire.code", string(body.Code)).Failure(err)
}
