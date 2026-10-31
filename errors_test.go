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

	testCases := []struct {
		name       string
		err        error
		status     int
		code       livewire.Code
		wantFields map[string]string
		noMessage  bool
	}{
		{name: "not found", err: livewire.NotFound(), status: http.StatusNotFound, code: livewire.CodeNotFound},
		{name: "conflict", err: livewire.Conflict(), status: http.StatusConflict, code: livewire.CodeConflict},
		{name: "in progress", err: livewire.InProgress(), status: http.StatusConflict, code: livewire.CodeInProgress},
		{name: "invalid", err: livewire.Invalid(map[string]string{"name": "required"}), status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, wantFields: map[string]string{"name": "required"}},
		{name: "forbidden field", err: livewire.ForbiddenField("secret"), status: http.StatusForbidden, code: livewire.CodeForbiddenField, wantFields: map[string]string{"secret": "forbidden"}},
		{name: "wrapped core error", err: fmt.Errorf("load part: %w", livewire.NotFound()), status: http.StatusNotFound, code: livewire.CodeNotFound},
		{name: "query error", err: queryError, status: http.StatusBadRequest, code: livewire.CodeInvalidSort, wantFields: map[string]string{"sort[0]": "unknown or unsortable field \"nope\""}},
		{name: "wrapped query error", err: fmt.Errorf("list: %w", queryError), status: http.StatusBadRequest, code: livewire.CodeInvalidSort, wantFields: map[string]string{"sort[0]": "unknown or unsortable field \"nope\""}},
		{name: "auth forbidden", err: &auth.Error{Kind: auth.ErrForbidden, Reason: "missing role admin"}, status: http.StatusForbidden, code: livewire.CodeForbidden},
		{name: "auth unauthenticated with code", err: &auth.Error{Kind: auth.ErrUnauthenticated, Code: "session_expired"}, status: http.StatusUnauthorized, code: "session_expired"},
		{name: "auth sentinel", err: auth.ErrConflict, status: http.StatusConflict, code: livewire.CodeConflict},
		{name: "auth invalid request", err: fmt.Errorf("bad: %w", auth.ErrInvalidRequest), status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "check refusal", err: authorization.IsAdmin()(userContext()), status: http.StatusForbidden, code: livewire.CodeForbidden},
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
			if testCase.err != nil {
				require.NotContains(t, body.Message, "10.0.0.1")
				require.NotContains(t, body.Message, "missing role")
			}
		})
	}
}

func TestErrorText(t *testing.T) {
	err := livewire.Invalid(map[string]string{"name": "required"})
	require.Equal(t, "invalid_request: the request is not valid (name: required)", err.Error())
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
