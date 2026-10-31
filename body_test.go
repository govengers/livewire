package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"

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

// money has its own unmarshaler and a tag the validator cannot parse.
type money struct {
	Cents int64 `json:"cents" validate:"min=abc"`
}

func (m *money) UnmarshalJSON(raw []byte) error {
	var cents int64
	if err := json.Unmarshal(raw, &cents); err != nil {
		return errors.New("money: not a number")
	}
	m.Cents = cents
	return nil
}

// lenient has its own unmarshaler and accepts any object.
type lenient struct {
	Seen int
}

func (l *lenient) UnmarshalJSON([]byte) error { l.Seen++; return nil }

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
	type moneyPointer struct {
		Amount *money `json:"amount"`
	}
	type moneySlice struct {
		Amounts []money `json:"amounts" validate:"dive"`
	}
	type moneyMap struct {
		Amounts map[string]money `json:"amounts" validate:"dive"`
	}
	for name, declare := range map[string]func() error{
		"typo":                        func() error { return declareBody[typo](t) },
		"bad argument":                func() error { return declareBody[badlyTagged](t) },
		"misplaced dive":              func() error { return declareBody[misplacedDive](t) },
		"behind a pointer":            func() error { return declareBody[behindPointer](t) },
		"behind a slice":              func() error { return declareBody[behindSlice](t) },
		"behind a map":                func() error { return declareBody[behindMap](t) },
		"own unmarshaler by pointer":  func() error { return declareBody[moneyPointer](t) },
		"own unmarshaler in a slice":  func() error { return declareBody[moneySlice](t) },
		"own unmarshaler in a map":    func() error { return declareBody[moneyMap](t) },
		"own unmarshaler at the root": func() error { return declareBody[money](t) },
	} {
		require.ErrorContains(t, declare(), "validate tags", name)
	}

	mutation := newBody[behindOmitempty](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"count":3}`))
	require.ErrorContains(t, err, "failed at run time", "a fault the preflight cannot see is an error, not a panic")
	requireError(t, err, http.StatusInternalServerError, livewire.CodeInternal, "")
}

type tree map[string]tree

type chain []chain

func TestMutationConstructionEndsOnRecursiveCollections(t *testing.T) {
	type forest struct {
		Tree  tree  `json:"tree"`
		Chain chain `json:"chain"`
		Self  *forest
	}
	mutation := newBody[forest](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"tree":{"a":{"b":{}}},"chain":[[],[[]]]}`))
	require.NoError(t, err)
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

	for _, key := range []string{"a]b.c", "a[", "a]"} {
		_, err = keyedMutation.DecodeBody(userContext(), []byte(`{"values":{"`+key+`":""}}`))
		fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
		require.Equal(t, map[string]string{"body": "required"}, fields, "an unbalanced key %q names the body, never a Go name", key)
	}

	genericMutation := newBody[generic[part]](t)
	_, err = genericMutation.DecodeBody(userContext(), []byte(`{"value":{"id":"3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"},"nested":{"sku":"a","quantity":0}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.quantity")
	require.Equal(t, map[string]string{"nested.quantity": "min"}, fields, "a package-qualified generic root is dropped")

	namedMutation := newBody[namedEmbedding](t)
	_, err = namedMutation.DecodeBody(userContext(), []byte(`{"nested":{"sku":"a","quantity":0}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.quantity")
	require.Equal(t, map[string]string{"nested.quantity": "min"}, fields, "a tagged embedding is a segment")
}

type withMaps struct {
	Name   string          `json:"name"`
	Nested nested          `json:"nested"`
	ByID   map[int]*nested `json:"byId" validate:"dive"`
	Any    any             `json:"any"`
	Raw    json.RawMessage `json:"raw"`
	Loose  *lenient        `json:"loose"`
	Number json.Number     `json:"number"`
}

type nested struct {
	Note string `json:"note" validate:"max=3"`
}

func TestMutationBodyWalkRefusalsByPath(t *testing.T) {
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
		{name: "unknown field inside an int-keyed map value", raw: `{"byId":{"7":{"note":"n","nope":1}}}`, field: "byId[7].nope", text: "unknown field"},
		{name: "validation inside the same map value shares the prefix", raw: `{"byId":{"7":{"note":"long"}}}`, field: "byId[7].note", text: "max"},
		{name: "duplicate map key", raw: `{"byId":{"7":{},"7":{}}}`, field: "byId[7]", text: "duplicate member"},
		{name: "duplicate inside an any value", raw: `{"any":{"secret":1,"secret":2}}`, field: "any.secret", text: "duplicate member"},
		{name: "duplicate inside a raw value", raw: `{"raw":[{"x":1,"x":2}]}`, field: "raw[0].x", text: "duplicate member"},
		{name: "duplicate inside a custom-unmarshaled value", raw: `{"loose":{"k":{"a":1,"a":2}}}`, field: "loose.k.a", text: "duplicate member"},
		{name: "trailing garbage", raw: `{"name":"a"} x`, field: "body", text: "must be valid JSON"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mutation.DecodeBody(userContext(), []byte(testCase.raw))
			fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, testCase.field)
			require.Equal(t, testCase.text, fields[testCase.field])
		})
	}
}

func TestMutationBodyWalkAcceptsWhatTheDecoderAccepts(t *testing.T) {
	mutation := newBody[withMaps](t)
	body, err := mutation.DecodeBody(userContext(), []byte(`{"byId":{"7":{"note":"n"}},"nested":{"note":"x"},"loose":{"extra":1,"more":[1,2]},"any":{"k":[{"v":1}]},"raw":{"q":true},"number":1e400}`))
	require.NoError(t, err)
	decoded := body.(*withMaps)
	require.Equal(t, "n", decoded.ByID[7].Note)
	require.Equal(t, 1, decoded.Loose.Seen, "a type with its own unmarshaler takes any members")
	require.Equal(t, json.Number("1e400"), decoded.Number, "a number beyond float64 reaches json.Number")
	require.JSONEq(t, `{"q":true}`, string(decoded.Raw))
}

type deep struct {
	Child *deep `json:"child,omitempty"`
	Leaf  int   `json:"leaf,omitempty"`
}

func TestMutationDeepBodyAllocatesInProportion(t *testing.T) {
	mutation := newBody[deep](t)
	const depth = 5000
	body := []byte(strings.Repeat(`{"child":`, depth) + `{"leaf":1}` + strings.Repeat(`}`, depth))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := mutation.DecodeBody(userContext(), body)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	allocated := after.TotalAlloc - before.TotalAlloc
	require.Less(t, allocated, uint64(len(body))*40, "the walk and the decode together allocate a small multiple of the body (%d bytes for %d)", allocated, len(body))

	unknown := strings.Repeat(`{"child":`, depth) + `{"nope":1}` + strings.Repeat(`}`, depth)
	_, err = mutation.DecodeBody(userContext(), []byte(unknown))
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, strings.Repeat("child.", depth)+"nope")
}
