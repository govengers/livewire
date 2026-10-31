package livewire

import (
	"errors"
	"maps"
	"net/http"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/query"
)

// Code is the stable client-facing code of an error body. The set is closed: generated
// clients switch on it.
type Code string

// The error codes. The invalid_* codes of the query package are repeated here so the set
// is complete in one place; ResponseFor maps a *query.Error to its code.
const (
	CodeInvalidRequest  Code = "invalid_request"
	CodeInvalidFilter   Code = Code(query.CodeInvalidFilter)
	CodeInvalidSort     Code = Code(query.CodeInvalidSort)
	CodeInvalidPage     Code = Code(query.CodeInvalidPage)
	CodeInvalidCursor   Code = Code(query.CodeInvalidCursor)
	CodeForbiddenField  Code = "forbidden_field"
	CodeUnauthenticated Code = "unauthenticated"
	CodeForbidden       Code = "forbidden"
	CodeNotFound        Code = "not_found"
	CodeConflict        Code = "conflict"
	CodeInProgress      Code = "in_progress"
	CodeInternal        Code = "internal"
)

// ErrorBody is the one JSON error shape of every REST response and socket error. Message
// is fixed per code and safe to show; Fields names the offending parameters or body fields
// with what is wrong with each.
type ErrorBody struct {
	Code    Code              `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Error is a refusal the core or a handler answers with its Status and Body. Match it with
// errors.As; build it with the constructors below.
type Error struct {
	Status int
	Body   ErrorBody
}

func (e *Error) Error() string {
	text := string(e.Body.Code) + ": " + e.Body.Message
	for name, message := range e.Body.Fields {
		text += " (" + name + ": " + message + ")"
	}
	return text
}

// NotFound is the 404 a loader or handler returns when the addressed item does not exist
// or the identity may not learn that it does.
func NotFound() *Error {
	return newError(http.StatusNotFound, CodeNotFound, nil)
}

// Conflict is the 409 of a write that contradicts the current state (a duplicate, a stale
// version).
func Conflict() *Error {
	return newError(http.StatusConflict, CodeConflict, nil)
}

// InProgress is the 409 of a replayed idempotency key whose first call has not finished
// (D16).
func InProgress() *Error {
	return newError(http.StatusConflict, CodeInProgress, nil)
}

// Invalid is the 400 of a request whose parameters or body are wrong; fields maps each
// offending name to what is wrong with it and is copied.
func Invalid(fields map[string]string) *Error {
	return newError(http.StatusBadRequest, CodeInvalidRequest, fields)
}

// ForbiddenField is the 403 of a filter, sort, search, distinct or body write on a field
// whose column check refuses the identity (D39).
func ForbiddenField(name string) *Error {
	return newError(http.StatusForbidden, CodeForbiddenField, map[string]string{name: "forbidden"})
}

// ResponseFor maps any error to the status and body a client gets. A *Error answers as it
// is; a *query.Error answers 400 with its code and fields; the auth refusals (*auth.Error
// and the bare sentinels) answer through auth.ResponseFor; anything else is a 500 with the
// code internal and no text of the cause.
func ResponseFor(err error) (int, ErrorBody) {
	var coreError *Error
	if errors.As(err, &coreError) {
		return coreError.Status, coreError.Body
	}
	var queryError *query.Error
	if errors.As(err, &queryError) {
		return http.StatusBadRequest, ErrorBody{Code: Code(queryError.Code), Message: queryError.Message, Fields: queryError.Fields}
	}
	status, response := auth.ResponseFor(err)
	return status, ErrorBody{Code: Code(response.Code), Message: response.Message}
}

var messages = map[Code]string{
	CodeInvalidRequest: "the request is not valid",
	CodeForbiddenField: "a field of the request is not accessible",
	CodeNotFound:       "not found",
	CodeConflict:       "the request conflicts with the current state",
	CodeInProgress:     "the same request is still in progress",
}

func newError(status int, code Code, fields map[string]string) *Error {
	body := ErrorBody{Code: code, Message: messages[code]}
	if len(fields) > 0 {
		body.Fields = maps.Clone(fields)
	}
	return &Error{Status: status, Body: body}
}

func invalidParameter(name, message string) *Error {
	return Invalid(map[string]string{name: message})
}
