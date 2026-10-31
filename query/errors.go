package query

import "fmt"

// Code names what a client got wrong. Every code answers 400.
type Code string

// The codes an Error carries.
const (
	CodeInvalidFilter Code = "invalid_filter"
	CodeInvalidSort   Code = "invalid_sort"
	CodeInvalidPage   Code = "invalid_page"
	CodeInvalidCursor Code = "invalid_cursor"
)

// Message is the fixed, client-safe text of the code; "" for a code this package does not
// define.
func (c Code) Message() string {
	return messages[c]
}

// Error is a client mistake in a filter document, a sort, a page or a cursor. Fields maps
// the path of the offending part (`where.and[1].field`, `sort[0]`, `limit`, `after`) to
// what is wrong with it; one error names one fault. Match it with errors.As; the root
// package answers it with 400 and the Code.
type Error struct {
	Code    Code
	Message string
	Fields  map[string]string
}

func (e *Error) Error() string {
	text := string(e.Code) + ": " + e.Message
	for path, message := range e.Fields {
		text += " (" + path + ": " + message + ")"
	}
	return text
}

var messages = map[Code]string{
	CodeInvalidFilter: "the filter is not valid for this resource",
	CodeInvalidSort:   "the sort is not valid for this resource",
	CodeInvalidPage:   "the page is not valid",
	CodeInvalidCursor: "the cursor is not valid for this sort",
}

func newError(code Code, path, format string, arguments ...any) *Error {
	return &Error{
		Code:    code,
		Message: messages[code],
		Fields:  map[string]string{path: fmt.Sprintf(format, arguments...)},
	}
}

func filterError(path, format string, arguments ...any) *Error {
	return newError(CodeInvalidFilter, path, format, arguments...)
}
