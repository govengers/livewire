package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/govengers/livewire"
)

// Error is a refusal or failure the server answered (section 3): the HTTP status and the
// error body. Code is from the closed set: a body that is not livewire's (a proxy's error
// page) gets the code of its status, and an error push, which carries no status, gets the
// status of its code. Match it with errors.As.
type Error struct {
	Status  int
	Code    livewire.Code
	Message string
	Fields  map[string]string
}

// Error names the status, the code and the message.
func (e *Error) Error() string {
	return fmt.Sprintf("livewire: %d %s: %s", e.Status, e.Code, e.Message)
}

// EndedError ends the live side of a Client for good: the server closed its socket with
// 4401 or refused the socket's handshake with 401, so the API key's identity is gone (D7,
// D66). Every subscription ends with it and Subscribe answers it after; make a new Client
// once a valid key is at hand.
type EndedError struct {
	// CloseCode is 4401 for a close, 401 for a refused handshake.
	CloseCode int
}

// Error says that the live side ended.
func (e *EndedError) Error() string {
	return fmt.Sprintf("livewire: the live side of the client ended (%d): the identity is gone", e.CloseCode)
}

var errorCodes = []livewire.Code{
	livewire.CodeInvalidRequest, livewire.CodeInvalidFilter, livewire.CodeInvalidSort, livewire.CodeInvalidPage,
	livewire.CodeInvalidCursor, livewire.CodeForbiddenField, livewire.CodeUnauthenticated, livewire.CodeForbidden,
	livewire.CodeNotFound, livewire.CodeConflict, livewire.CodeInProgress, livewire.CodeInternal,
}

// errorOf builds the Error of a status and a body; status 0 (an error push) takes the
// status of the code.
func errorOf(status int, body []byte) *Error {
	var decoded livewire.ErrorBody
	if json.Unmarshal(body, &decoded) != nil || !slices.Contains(errorCodes, decoded.Code) {
		decoded = livewire.ErrorBody{Code: codeOfStatus(status)}
	}
	if decoded.Message == "" {
		decoded.Message = string(decoded.Code)
	}
	if status == 0 {
		status = statusOfCode(decoded.Code)
	}
	return &Error{Status: status, Code: decoded.Code, Message: decoded.Message, Fields: decoded.Fields}
}

func codeOfStatus(status int) livewire.Code {
	switch status {
	case http.StatusBadRequest:
		return livewire.CodeInvalidRequest
	case http.StatusUnauthorized:
		return livewire.CodeUnauthenticated
	case http.StatusForbidden:
		return livewire.CodeForbidden
	case http.StatusNotFound:
		return livewire.CodeNotFound
	case http.StatusConflict:
		return livewire.CodeConflict
	default:
		return livewire.CodeInternal
	}
}

func statusOfCode(code livewire.Code) int {
	switch code {
	case livewire.CodeInvalidRequest, livewire.CodeInvalidFilter, livewire.CodeInvalidSort, livewire.CodeInvalidPage, livewire.CodeInvalidCursor:
		return http.StatusBadRequest
	case livewire.CodeForbiddenField, livewire.CodeForbidden:
		return http.StatusForbidden
	case livewire.CodeUnauthenticated:
		return http.StatusUnauthorized
	case livewire.CodeNotFound:
		return http.StatusNotFound
	case livewire.CodeConflict, livewire.CodeInProgress:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
