package livewire

import (
	"errors"
	"maps"
	"net/http"
	"reflect"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/query"
)

// Code is the stable client-facing code of an error body. The set is closed: generated
// clients switch on it, so a custom code of an *auth.Error is mapped to its kind's code.
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
// (nested ones as a dotted path) with a fixed text per fault.
type ErrorBody struct {
	Code    Code              `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Error is a refusal the core or a handler answers with its status and body. Only the
// constructors below build one, so the code is from the closed set and the message is the
// code's fixed text. Match it with errors.As.
type Error struct {
	status int
	body   ErrorBody
}

// Status is the HTTP status of the refusal.
func (e *Error) Status() int { return e.status }

// Body is the error body; the result is the caller's copy.
func (e *Error) Body() ErrorBody {
	body := e.body
	body.Fields = maps.Clone(e.body.Fields)
	return body
}

func (e *Error) Error() string {
	text := string(e.body.Code) + ": " + e.body.Message
	for _, name := range sortedKeys(e.body.Fields) {
		text += " (" + name + ": " + e.body.Fields[name] + ")"
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

// ResponseFor maps any error to the status and body a client gets. The outermost error of a
// known kind in the chain decides, so a refusal that wraps a cause answers as the refusal:
// a *Error answers as it is; a *query.Error answers 400 with its code and fields; an
// *auth.Error or a bare auth sentinel answers its kind's status with the kind's code from
// the closed set (a custom auth code is dropped); anything else, a nil *Error included, is a
// 500 with the code internal and no text of the cause.
func ResponseFor(err error) (int, ErrorBody) {
	for current := range errorChain(err) {
		if status, body, ok := responseForOne(current); ok {
			return status, body
		}
	}
	status, response := auth.ResponseFor(nil)
	return status, ErrorBody{Code: CodeInternal, Message: response.Message}
}

func responseForOne(err error) (int, ErrorBody, bool) {
	if isNilPointer(err) {
		return 0, ErrorBody{}, false // a typed nil answers nothing; its methods may dereference it
	}
	switch typed := err.(type) { //nolint:errorlint // one node of the chain, not the chain: errors.As would look past it
	case *Error:
		if typed.status == 0 {
			return 0, ErrorBody{}, false // a zero value was never built by a constructor
		}
		return typed.status, typed.Body(), true
	case *query.Error:
		return queryResponse(typed)
	case *auth.Error:
		return authResponse(typed.Kind, true)
	}
	return authResponse(err, isLeaf(err))
}

// queryResponse answers a *query.Error with its code's fixed message (query.Code.Message); a
// hand-built one keeps only a code the query package defines and its fields, copied.
func queryResponse(queryError *query.Error) (int, ErrorBody, bool) {
	message := queryError.Code.Message()
	if message == "" {
		return 0, ErrorBody{}, false
	}
	return http.StatusBadRequest, ErrorBody{Code: Code(queryError.Code), Message: message, Fields: maps.Clone(queryError.Fields)}, true
}

var authCodes = []struct {
	kind error
	code Code
}{
	{auth.ErrUnauthenticated, CodeUnauthenticated},
	{auth.ErrForbidden, CodeForbidden},
	{auth.ErrInvalidRequest, CodeInvalidRequest},
	{auth.ErrConflict, CodeConflict},
}

// authResponse answers the auth kind err is. A leaf is matched with errors.Is (identity or
// its own Is method; never a hash or comparison of a non-comparable value); a node that
// wraps causes is asked its own Is method only, so a cause the walk visits later cannot
// override an outer known error.
func authResponse(err error, leaf bool) (int, ErrorBody, bool) {
	for _, known := range authCodes {
		if matchesKind(err, known.kind, leaf) {
			status, response := auth.ResponseFor(known.kind)
			return status, ErrorBody{Code: known.code, Message: response.Message}, true
		}
	}
	return 0, ErrorBody{}, false
}

func matchesKind(err, kind error, leaf bool) bool {
	if leaf {
		return errors.Is(err, kind)
	}
	matcher, ok := err.(interface{ Is(error) bool })
	return ok && matcher.Is(kind)
}

// errorChain walks the error tree outermost first, depth first, like errors.As does.
func errorChain(err error) func(yield func(error) bool) {
	return func(yield func(error) bool) {
		walkErrors(err, yield)
	}
}

func walkErrors(err error, yield func(error) bool) bool {
	if err == nil {
		return true
	}
	if !yield(err) {
		return false
	}
	if isNilPointer(err) {
		return true // a typed nil has nothing to unwrap and its Unwrap may dereference itself
	}
	switch unwrapper := err.(type) { //nolint:errorlint // this is the unwrapping itself
	case interface{ Unwrap() error }:
		return walkErrors(unwrapper.Unwrap(), yield)
	case interface{ Unwrap() []error }:
		for _, wrapped := range unwrapper.Unwrap() {
			if !walkErrors(wrapped, yield) {
				return false
			}
		}
	}
	return true
}

// isLeaf reports whether err wraps nothing, so matching it is matching the node itself and
// not a cause the walk visits later.
func isLeaf(err error) bool {
	switch err.(type) { //nolint:errorlint // asks the node, not the chain
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		return false
	default:
		return true
	}
}

func isNilPointer(err error) bool {
	value := reflect.ValueOf(err)
	return value.Kind() == reflect.Pointer && value.IsNil()
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
	return &Error{status: status, body: body}
}

func invalidParameter(name, message string) *Error {
	return Invalid(map[string]string{name: message})
}
