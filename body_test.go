package livewire_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func declareBody[Request any](t *testing.T) error {
	t.Helper()
	return addError(t, livewire.NewMutation("m", http.MethodPost, "/m", &livewire.MutationOptions[livewire.NoParameters, Request, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *Request) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	}))
}

func newBody[Request any](t *testing.T) *livewire.Mutation {
	t.Helper()
	mutation := livewire.NewMutation("m", http.MethodPost, "/m", &livewire.MutationOptions[livewire.NoParameters, Request, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *Request) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, mutation))
	return mutation
}

type badlyTagged struct {
	Count int `json:"count" validate:"min=abc"`
}

func TestMutationRefusesUnparseableValidateTags(t *testing.T) {
	type typo struct {
		Name string `json:"name" validate:"reqd"`
	}
	type misplacedDive struct {
		Name string `json:"name" validate:"dive"`
	}
	type behindPointer struct {
		Detail *badlyTagged `json:"detail"`
	}
	type behindSlice struct {
		Details []badlyTagged `json:"details" validate:"dive"`
	}
	type behindMap struct {
		Details map[string][]*badlyTagged `json:"details"`
	}
	type behindOmitempty struct {
		Count int `json:"count" validate:"omitempty,min=abc"`
	}
	require.ErrorContains(t, declareBody[typo](t), "validate tags")
	require.ErrorContains(t, declareBody[badlyTagged](t), "validate tags")
	require.ErrorContains(t, declareBody[misplacedDive](t), "validate tags")
	require.ErrorContains(t, declareBody[behindPointer](t), "validate tags")
	require.ErrorContains(t, declareBody[behindSlice](t), "validate tags")
	require.ErrorContains(t, declareBody[behindMap](t), "validate tags")

	mutation := newBody[behindOmitempty](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"count":3}`))
	require.ErrorContains(t, err, "failed at run time", "a fault the preflight cannot see is an error, not a panic")
	requireError(t, err, http.StatusInternalServerError, livewire.CodeInternal, "")
}

func TestMutationValidationPathsWithPunctuationAndGenerics(t *testing.T) {
	type keyed struct {
		Values map[string]string `json:"values" validate:"dive,required"`
	}
	type namedEmbedding struct {
		Line `json:"nested"`
	}
	keyedMutation := newBody[keyed](t)
	_, err := keyedMutation.DecodeBody(userContext(), []byte(`{"values":{"a.b":"","c[d]":""}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "values[a.b]")
	require.Equal(t, map[string]string{"values[a.b]": "required", "values[c[d]]": "required"}, fields)

	genericMutation := newBody[generic[part]](t)
	_, err = genericMutation.DecodeBody(userContext(), []byte(`{"value":{"id":"3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"},"nested":{"sku":"a","quantity":0}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.quantity")
	require.Equal(t, map[string]string{"nested.quantity": "min"}, fields, "a package-qualified generic root is dropped")

	namedMutation := newBody[namedEmbedding](t)
	_, err = namedMutation.DecodeBody(userContext(), []byte(`{"nested":{"sku":"a","quantity":0}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.quantity")
	require.Equal(t, map[string]string{"nested.quantity": "min"}, fields, "a tagged embedding is a segment")
}

func TestMutationRefusesDuplicateMembersAndNamesMapValues(t *testing.T) {
	type nested struct {
		Note string `json:"note"`
	}
	type withMaps struct {
		Name   string          `json:"name"`
		Nested nested          `json:"nested"`
		ByID   map[int]*nested `json:"byId"`
	}
	mutation := newBody[withMaps](t)
	testCases := []struct {
		name  string
		raw   string
		field string
		text  string
	}{
		{name: "top-level duplicate", raw: `{"name":"a","name":"b"}`, field: "name", text: "duplicate member"},
		{name: "nested duplicate", raw: `{"nested":{"note":"a"},"nested":{"note":"b"}}`, field: "nested", text: "duplicate member"},
		{name: "duplicate inside a nested object", raw: `{"nested":{"note":"a","note":"b"}}`, field: "nested.note", text: "duplicate member"},
		{name: "unknown field in the first of two occurrences", raw: `{"nested":{"nope":1},"nested":{"note":"b"}}`, field: "nested.nope", text: "unknown field"},
		{name: "unknown field inside an int-keyed map value", raw: `{"byId":{"7":{"note":"n","nope":1}}}`, field: "byId.7.nope", text: "unknown field"},
		{name: "duplicate map key", raw: `{"byId":{"7":{},"7":{}}}`, field: "byId.7", text: "duplicate member"},
		{name: "trailing garbage", raw: `{"name":"a"} x`, field: "body", text: "must be valid JSON"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mutation.DecodeBody(userContext(), []byte(testCase.raw))
			fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, testCase.field)
			require.Equal(t, testCase.text, fields[testCase.field])
		})
	}
	body, err := mutation.DecodeBody(userContext(), []byte(`{"byId":{"7":{"note":"n"}},"nested":{"note":"x"}}`))
	require.NoError(t, err)
	require.Equal(t, "n", body.(*withMaps).ByID[7].Note)
}

type deep struct {
	Child *deep `json:"child,omitempty"`
	Leaf  int   `json:"leaf,omitempty"`
}

func TestMutationDeepBodyIsLinear(t *testing.T) {
	mutation := newBody[deep](t)
	const depth = 5000
	body := strings.Repeat(`{"child":`, depth) + `{"leaf":1}` + strings.Repeat(`}`, depth)
	started := time.Now()
	_, err := mutation.DecodeBody(userContext(), []byte(body))
	require.NoError(t, err)
	require.Less(t, time.Since(started), 2*time.Second)

	unknown := strings.Repeat(`{"child":`, depth) + `{"nope":1}` + strings.Repeat(`}`, depth)
	_, err = mutation.DecodeBody(userContext(), []byte(unknown))
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, strings.Repeat("child.", depth)+"nope")
}
