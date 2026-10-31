package livewire_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

func TestResponseFor(t *testing.T) {
	_, queryError := query.ParseSort(partFields(), "nope")
	require.Error(t, queryError)
	sortField := map[string]string{"sort[0]": "unknown or unsortable field \"nope\""}

	testCases := []struct {
		name       string
		err        error
		status     int
		code       livewire.Code
		wantFields map[string]string
	}{
		{name: "not found", err: livewire.NotFound(), status: http.StatusNotFound, code: livewire.CodeNotFound},
		{name: "conflict", err: livewire.Conflict(), status: http.StatusConflict, code: livewire.CodeConflict},
		{name: "in progress", err: livewire.InProgress(), status: http.StatusConflict, code: livewire.CodeInProgress},
		{name: "invalid", err: livewire.Invalid(map[string]string{"name": "required"}), status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, wantFields: map[string]string{"name": "required"}},
		{name: "forbidden field", err: livewire.ForbiddenField("secret"), status: http.StatusForbidden, code: livewire.CodeForbiddenField, wantFields: map[string]string{"secret": "forbidden"}},
		{name: "wrapped core error", err: fmt.Errorf("load part: %w", livewire.NotFound()), status: http.StatusNotFound, code: livewire.CodeNotFound},
		{name: "query error", err: queryError, status: http.StatusBadRequest, code: livewire.CodeInvalidSort, wantFields: sortField},
		{name: "wrapped query error", err: fmt.Errorf("list: %w", queryError), status: http.StatusBadRequest, code: livewire.CodeInvalidSort, wantFields: sortField},
		{name: "auth forbidden", err: &auth.Error{Kind: auth.ErrForbidden, Reason: "missing role admin"}, status: http.StatusForbidden, code: livewire.CodeForbidden},
		{name: "auth custom code maps to the kind", err: &auth.Error{Kind: auth.ErrUnauthenticated, Code: "session_expired"}, status: http.StatusUnauthorized, code: livewire.CodeUnauthenticated},
		{name: "auth invalid request with code", err: &auth.Error{Kind: auth.ErrInvalidRequest, Code: "invalid_api_key"}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "auth conflict with code", err: &auth.Error{Kind: auth.ErrConflict, Code: "username_taken"}, status: http.StatusConflict, code: livewire.CodeConflict},
		{name: "auth forbidden with code", err: &auth.Error{Kind: auth.ErrForbidden, Code: "inactive"}, status: http.StatusForbidden, code: livewire.CodeForbidden},
		{name: "auth sentinel", err: auth.ErrConflict, status: http.StatusConflict, code: livewire.CodeConflict},
		{name: "wrapped auth sentinel", err: fmt.Errorf("bad: %w", auth.ErrInvalidRequest), status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "check refusal", err: authorization.IsAdmin()(userContext()), status: http.StatusForbidden, code: livewire.CodeForbidden},
		{name: "auth error of an unknown kind", err: &auth.Error{Kind: errors.New("odd")}, status: http.StatusInternalServerError, code: livewire.CodeInternal},
		{name: "refusal wrapping a core cause answers the refusal", err: &auth.Error{Kind: auth.ErrForbidden, Err: livewire.NotFound()}, status: http.StatusForbidden, code: livewire.CodeForbidden},
		{name: "refusal wrapping a query cause answers the refusal", err: &auth.Error{Kind: auth.ErrForbidden, Err: queryError}, status: http.StatusForbidden, code: livewire.CodeForbidden},
		{name: "sentinel wrapping a core cause answers the sentinel", err: fmt.Errorf("%w: %w", auth.ErrUnauthenticated, livewire.Conflict()), status: http.StatusUnauthorized, code: livewire.CodeUnauthenticated},
		{name: "core error wrapping a refusal answers the core error", err: fmt.Errorf("%w: %w", livewire.NotFound(), auth.ErrForbidden), status: http.StatusNotFound, code: livewire.CodeNotFound},
		{name: "query error wrapping a refusal answers the query error", err: fmt.Errorf("%w (%w)", queryError, auth.ErrForbidden), status: http.StatusBadRequest, code: livewire.CodeInvalidSort, wantFields: sortField},
		{name: "unknown error", err: errors.New("pq: connection refused to 10.0.0.1"), status: http.StatusInternalServerError, code: livewire.CodeInternal},
		{name: "nil", err: nil, status: http.StatusInternalServerError, code: livewire.CodeInternal},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := livewire.ResponseFor(testCase.err)
			require.Equal(t, testCase.status, status)
			require.Equal(t, testCase.code, body.Code)
			require.NotEmpty(t, body.Message)
			require.Equal(t, testCase.wantFields, body.Fields)
			require.NotContains(t, body.Message, "10.0.0.1")
			require.NotContains(t, body.Message, "missing role")
		})
	}
}

func TestErrorText(t *testing.T) {
	err := livewire.Invalid(map[string]string{"name": "required", "age": "must be an integer"})
	require.Equal(t, "invalid_request: the request is not valid (age: must be an integer) (name: required)", err.Error())
	require.Equal(t, "not_found: not found", livewire.NotFound().Error())
}

func TestInvalidCopiesFields(t *testing.T) {
	fields := map[string]string{"name": "required"}
	err := livewire.Invalid(fields)
	fields["name"] = "changed"
	require.Equal(t, "required", err.Body.Fields["name"])
}

func TestErrorBodyJSON(t *testing.T) {
	_, body := livewire.ResponseFor(livewire.NotFound())
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"code":"not_found","message":"not found"}`, string(raw))

	_, body = livewire.ResponseFor(livewire.ForbiddenField("secret"))
	raw, err = json.Marshal(body)
	require.NoError(t, err)
	require.JSONEq(t, `{"code":"forbidden_field","message":"a field of the request is not accessible","fields":{"secret":"forbidden"}}`, string(raw))
}
