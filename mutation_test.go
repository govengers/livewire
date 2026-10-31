package livewire_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

type partLine struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity" validate:"min=1"`
}

// leaky fails to unmarshal with a message that must never reach a client.
type leaky struct{}

func (l *leaky) UnmarshalJSON([]byte) error { return errors.New("secret-token-123 rejected") }

type createPart struct {
	ID       *uuid.UUID `json:"id,omitempty"`
	Name     string     `json:"name"`
	Type     string     `json:"type" validate:"omitempty,oneof=bolt nut"`
	Secret   string     `json:"secret,omitempty"`
	Price    *float64   `json:"price,omitempty"`
	Internal string     `json:"-"`
	Nested   struct {
		Note string `json:"note" validate:"max=5"`
	} `json:"nested"`
	Lines      []partLine `json:"lines,omitempty" validate:"dive"`
	Leaky      *leaky     `json:"leaky,omitempty"`
	Attributes map[string]*struct {
		Note string `json:"note"`
	} `json:"attributes,omitempty"`
}

func TestMutationRefusesUnparseableValidateTags(t *testing.T) {
	type typo struct {
		Name string `json:"name" validate:"reqd"`
	}
	type badArgument struct {
		Count int `json:"count" validate:"min=abc"`
	}
	type misplacedDive struct {
		Name string `json:"name" validate:"dive"`
	}
	declare := func(declaration livewire.Declaration) error { return addError(t, declaration) }
	require.ErrorContains(t, declare(livewire.NewMutation("a", http.MethodPost, "/a", &livewire.MutationOptions[livewire.NoParameters, typo, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *typo) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})), "validate tags")
	require.ErrorContains(t, declare(livewire.NewMutation("b", http.MethodPost, "/b", &livewire.MutationOptions[livewire.NoParameters, badArgument, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *badArgument) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})), "validate tags")
	require.ErrorContains(t, declare(livewire.NewMutation("c", http.MethodPost, "/c", &livewire.MutationOptions[livewire.NoParameters, misplacedDive, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *misplacedDive) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})), "validate tags")
}

// Line is exported because encoding/json refuses to allocate an embedded pointer to an unexported struct.
type Line struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity" validate:"min=1"`
}

type pointerEmbedding struct {
	*Line
	Note string `json:"note" validate:"max=3"`
}

type generic[Value any] struct {
	Value  Value      `json:"value"`
	Nested *partLine  `json:"nested"`
	Many   []partLine `json:"many" validate:"dive"`
}

func TestMutationValidationPaths(t *testing.T) {
	type audited struct {
		createPart
		Reason string `json:"reason" validate:"required"`
	}
	auditedMutation := livewire.NewMutation("a", http.MethodPost, "/a", &livewire.MutationOptions[livewire.NoParameters, audited, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *audited) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	pointerMutation := livewire.NewMutation("b", http.MethodPost, "/b", &livewire.MutationOptions[livewire.NoParameters, pointerEmbedding, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *pointerEmbedding) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	genericMutation := livewire.NewMutation("c", http.MethodPost, "/c", &livewire.MutationOptions[livewire.NoParameters, generic[string], livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *generic[string]) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, auditedMutation, pointerMutation, genericMutation))

	_, err := auditedMutation.DecodeBody(userContext(), []byte(`{"nested":{"note":"too long"},"type":"screw"}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "reason")
	require.Equal(t, map[string]string{"reason": "required", "nested.note": "max", "type": "oneof"}, fields, "embedded value struct adds no segment")

	_, err = pointerMutation.DecodeBody(userContext(), []byte(`{"sku":"a","quantity":0,"note":"long"}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "note")
	require.Equal(t, map[string]string{"note": "max", "quantity": "min"}, fields, "embedded pointer struct adds no segment")

	_, err = genericMutation.DecodeBody(userContext(), []byte(`{"value":"v","nested":{"sku":"a","quantity":0},"many":[{"sku":"b","quantity":1},{"sku":"c","quantity":0}]}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.quantity")
	require.Equal(t, map[string]string{"nested.quantity": "min", "many[1].quantity": "min"}, fields, "generic root type is dropped")
}

func TestMutationUnknownFieldInsideMapValuesAndDuplicateMembers(t *testing.T) {
	mutation := newCreatePart(t, createOptions())
	_, err := mutation.DecodeBody(userContext(), []byte(`{"attributes":{"color":{"note":"n","nope":1}}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "attributes.color.nope")
	require.Equal(t, "unknown field", fields["attributes.color.nope"])

	body, err := mutation.DecodeBody(userContext(), []byte(`{"name":"first","name":"last"}`))
	require.NoError(t, err)
	require.Equal(t, "last", body.(*createPart).Name, "a repeated member is taken last-wins")
}

type partID struct {
	ID uuid.UUID `path:"id"`
}

type createOptionsType = livewire.MutationOptions[livewire.NoParameters, createPart, part]

func createOptions() *createOptionsType {
	return &createOptionsType{
		Check:  authorization.IsUser(),
		Status: http.StatusCreated,
		Handle: func(_ context.Context, _ *livewire.NoParameters, body *createPart) (*part, error) {
			return &part{Name: body.Name}, nil
		},
	}
}

func newCreatePart(t *testing.T, options *createOptionsType) *livewire.Mutation {
	t.Helper()
	mutation := livewire.NewMutation("createPart", http.MethodPost, "/parts", options)
	require.NoError(t, addError(t, livewire.NewResource("parts", "/parts", partOptions()), mutation))
	return mutation
}

func newBodyless(method string, options func(*livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent])) *livewire.Mutation {
	mutationOptions := &livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *partID, *livewire.NoBody) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	}
	if options != nil {
		options(mutationOptions)
	}
	return livewire.NewMutation("deletePart", method, "/parts/{id}", mutationOptions)
}

func TestMutationDeclarationErrors(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		change func(options *createOptionsType)
		want   string
	}{
		{name: "no handle", method: http.MethodPost, change: func(options *createOptionsType) { options.Handle = nil }, want: "Handle is required"},
		{name: "no check", method: http.MethodPost, change: func(options *createOptionsType) { options.Check = nil }, want: "Check is required"},
		{name: "read method", method: http.MethodGet, change: func(*createOptionsType) {}, want: "not a write method"},
		{name: "bad status", method: http.MethodPost, change: func(options *createOptionsType) { options.Status = http.StatusNotFound }, want: "not a success status"},
		{name: "204 with content", method: http.MethodPost, change: func(options *createOptionsType) { options.Status = http.StatusNoContent }, want: "204 cannot carry the response body"},
		{name: "bad client id version", method: http.MethodPost, change: func(options *createOptionsType) { options.ClientID = 9 }, want: "not a ClientIDVersion"},
		{name: "client id on an update", method: http.MethodPut, change: func(options *createOptionsType) { options.ClientID = livewire.UUIDv7 }, want: "ClientID needs a POST"},
		{name: "field check unknown", method: http.MethodPost, change: func(options *createOptionsType) {
			options.FieldChecks = map[string]authorization.Check{"nope": authorization.IsAdmin()}
		}, want: `no JSON field "nope"`},
		{name: "field check on a skipped field", method: http.MethodPost, change: func(options *createOptionsType) {
			options.FieldChecks = map[string]authorization.Check{"Internal": authorization.IsAdmin()}
		}, want: `no JSON field "Internal"`},
		{name: "field check nil", method: http.MethodPost, change: func(options *createOptionsType) {
			options.FieldChecks = map[string]authorization.Check{"name": nil}
		}, want: "is nil"},
		{name: "effect unknown kind", method: http.MethodPost, change: func(options *createOptionsType) {
			options.Effect = livewire.Effect{Kind: "replaces", Target: "parts"}
		}, want: "unknown"},
		{name: "effect without target", method: http.MethodPost, change: func(options *createOptionsType) {
			options.Effect = livewire.Effect{Kind: livewire.EffectUpserts}
		}, want: "Effect needs Kind and Target"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := createOptions()
			testCase.change(options)
			err := addError(t, livewire.NewMutation("createPart", testCase.method, "/parts", options))
			require.ErrorContains(t, err, testCase.want)
			require.ErrorContains(t, err, `mutation "createPart"`)
		})
	}

	t.Run("request not a struct", func(t *testing.T) {
		err := addError(t, livewire.NewMutation("rename", http.MethodPut, "/parts", &livewire.MutationOptions[livewire.NoParameters, string, livewire.NoContent]{
			Check: authorization.IsUser(),
			Handle: func(context.Context, *livewire.NoParameters, *string) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		}))
		require.ErrorContains(t, err, "not a struct")
	})
	t.Run("client id without id field", func(t *testing.T) {
		type unkeyed struct {
			Name string `json:"name"`
		}
		err := addError(t, livewire.NewMutation("create", http.MethodPost, "/parts", &livewire.MutationOptions[livewire.NoParameters, unkeyed, livewire.NoContent]{
			Check:    authorization.IsUser(),
			ClientID: livewire.UUIDv7,
			Handle: func(context.Context, *livewire.NoParameters, *unkeyed) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		}))
		require.ErrorContains(t, err, `no JSON field "id"`)
	})
	t.Run("client id without body", func(t *testing.T) {
		err := addError(t, newBodyless(http.MethodPost, func(options *livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent]) {
			options.ClientID = livewire.UUIDv4
		}))
		require.ErrorContains(t, err, "ClientID needs a POST with a body")
	})
	t.Run("nil options", func(t *testing.T) {
		require.ErrorContains(t, addError(t, livewire.NewMutation[livewire.NoParameters, createPart, part]("x", http.MethodPost, "/x", nil)), "Check is required")
	})
}

func TestMutationDefaultsAndAccessors(t *testing.T) {
	mutation := newCreatePart(t, createOptions())
	require.Equal(t, livewire.KindMutation, mutation.Kind())
	require.Equal(t, http.StatusCreated, mutation.Status())
	require.True(t, mutation.HasBody())
	require.True(t, mutation.HasContent())
	require.Equal(t, reflect.TypeFor[createPart](), mutation.RequestType())
	require.Equal(t, reflect.TypeFor[part](), mutation.ResponseType())
	require.Equal(t, reflect.TypeFor[livewire.NoParameters](), mutation.ParametersType())
	require.Equal(t, livewire.ClientIDNone, mutation.ClientID())
	require.Empty(t, mutation.Resource())
	require.Equal(t, livewire.Effect{}, mutation.Effect())
	require.Nil(t, mutation.Tables())

	deletePart := newBodyless(http.MethodDelete, func(options *livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent]) {
		options.Effect = livewire.Removes("parts")
	})
	require.NoError(t, addError(t, livewire.NewResource("parts", "/parts", partOptions()), deletePart))
	require.Equal(t, http.StatusNoContent, deletePart.Status())
	require.False(t, deletePart.HasBody())
	require.False(t, deletePart.HasContent())
	require.Equal(t, livewire.Effect{Kind: livewire.EffectRemoves, Target: "parts"}, deletePart.Effect())
	require.Equal(t, livewire.Upserts("parts"), livewire.Effect{Kind: livewire.EffectUpserts, Target: "parts"})

	body, err := deletePart.DecodeBody(userContext(), nil)
	require.NoError(t, err)
	require.Equal(t, &livewire.NoBody{}, body)
	body, err = deletePart.DecodeBody(userContext(), []byte(" \n"))
	require.NoError(t, err)
	require.Equal(t, &livewire.NoBody{}, body)
	_, err = deletePart.DecodeBody(userContext(), []byte(`{}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
	require.Equal(t, "not accepted", fields["body"])

	renamed := livewire.NewMutation("renamePart", http.MethodPatch, "/parts/{id}", &livewire.MutationOptions[partID, createPart, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *partID, *createPart) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, renamed))
	require.Equal(t, http.StatusNoContent, renamed.Status())
	_, err = renamed.DecodeBody(userContext(), []byte(`{"id":"not-a-uuid-at-all","name":"x"}`))
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
	_, err = renamed.DecodeBody(userContext(), []byte(`{"id":"3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f","name":"x"}`))
	require.NoError(t, err, "an update may carry any id; the client-id rule is a create's")
}

func TestMutationCopiesOptions(t *testing.T) {
	options := createOptions()
	options.FieldChecks = map[string]authorization.Check{"price": authorization.IsAdmin()}
	mutation := newCreatePart(t, options)
	options.Handle = nil
	options.FieldChecks["price"] = nil
	delete(options.FieldChecks, "price")

	response, err := mutation.Handle(userContext(), &livewire.NoParameters{}, &createPart{Name: "bolt"})
	require.NoError(t, err)
	require.Equal(t, &part{Name: "bolt"}, response)
	_, err = mutation.DecodeBody(userContext(), []byte(`{"price":1}`))
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "price")
}

func TestMutationDecodeBody(t *testing.T) {
	options := createOptions()
	options.ClientID = livewire.UUIDv7
	options.Resource = "parts"
	options.FieldChecks = map[string]authorization.Check{"price": authorization.HasRole("pricing")}
	mutation := newCreatePart(t, options)
	versionSeven := "0192f1a0-1b2c-7d3e-8f40-516273849506"
	versionFour := "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"
	wrongVariant := "0192f1a0-1b2c-7d3e-0f40-516273849506"

	t.Run("accepted", func(t *testing.T) {
		body, err := mutation.DecodeBody(userContext(), []byte(`{"id":"`+versionSeven+`","name":"bolt","type":"bolt","nested":{"note":"n"},"lines":[{"sku":"a","quantity":2}]}`))
		require.NoError(t, err)
		decoded, ok := body.(*createPart)
		require.True(t, ok)
		require.Equal(t, "bolt", decoded.Name)
		require.Equal(t, uuid.MustParse(versionSeven), *decoded.ID)
		require.Equal(t, "n", decoded.Nested.Note)
	})
	t.Run("admin with pricing writes every guarded field", func(t *testing.T) {
		_, err := mutation.DecodeBody(roleContext("pricing", "admin"), []byte(`{"name":"bolt","secret":"s","price":1}`))
		require.NoError(t, err)
	})

	testCases := []struct {
		name     string
		identity func() context.Context
		raw      string
		status   int
		code     livewire.Code
		field    string
		text     string
	}{
		{name: "empty body", identity: userContext, raw: ``, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body", text: "required"},
		{name: "not an object", identity: userContext, raw: `[1]`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body", text: "must be an object"},
		{name: "null", identity: userContext, raw: `null`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body", text: "must be an object"},
		{name: "malformed", identity: userContext, raw: `{"name":`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body", text: "must be valid JSON"},
		{name: "unknown field names it", identity: userContext, raw: `{"name":"bolt","colour":"red"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "colour", text: "unknown field"},
		{name: "skipped field is unknown", identity: userContext, raw: `{"Internal":"x"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "Internal", text: "unknown field"},
		{name: "unknown nested field names its path", identity: userContext, raw: `{"name":"bolt","nested":{"nope":1}}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "nested.nope", text: "unknown field"},
		{name: "unknown field in an array element", identity: userContext, raw: `{"lines":[{"sku":"a","quantity":1},{"sku":"b","quantity":1,"colour":"red"}]}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "lines[1].colour", text: "unknown field"},
		{name: "wrong type names the field", identity: userContext, raw: `{"name":7}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "name", text: "has the wrong type"},
		{name: "wrong nested type names the path", identity: userContext, raw: `{"nested":{"note":1}}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "nested.note", text: "has the wrong type"},
		{name: "custom unmarshaler text stays on the server", identity: userContext, raw: `{"leaky":{}}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body", text: "could not be decoded as the request type"},
		{name: "unknown before forbidden", identity: userContext, raw: `{"secret":"s","colour":"red"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "colour"},
		{name: "client id of the wrong version", identity: userContext, raw: `{"id":"` + versionFour + `","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id", text: "must be a canonical UUID version 7"},
		{name: "client id of the wrong variant", identity: userContext, raw: `{"id":"` + wrongVariant + `","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "client id as urn", identity: userContext, raw: `{"id":"urn:uuid:` + versionSeven + `","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "client id in braces", identity: userContext, raw: `{"id":"{` + versionSeven + `}","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "client id without hyphens", identity: userContext, raw: `{"id":"0192f1a01b2c7d3e8f40516273849506","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "client id not a uuid", identity: userContext, raw: `{"id":"nope","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "client id not a string", identity: userContext, raw: `{"id":7,"name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "resource write check inherited from read", identity: userContext, raw: `{"name":"bolt","secret":"s"}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "field check", identity: userContext, raw: `{"name":"bolt","price":1}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "price"},
		{name: "field check passes, resource check refuses", identity: func() context.Context { return roleContext("pricing") }, raw: `{"name":"bolt","price":1,"secret":"s"}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "forbidden before invalid", identity: userContext, raw: `{"secret":"s","type":"screw"}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "validation names the field with its tag", identity: userContext, raw: `{"name":"bolt","type":"screw"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "type", text: "oneof"},
		{name: "validation names a nested path", identity: userContext, raw: `{"name":"bolt","nested":{"note":"too long"}}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "nested.note", text: "max"},
		{name: "validation names an array element", identity: userContext, raw: `{"lines":[{"sku":"a","quantity":0}]}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "lines[0].quantity", text: "min"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mutation.DecodeBody(testCase.identity(), []byte(testCase.raw))
			fields := requireError(t, err, testCase.status, testCase.code, testCase.field)
			if testCase.text != "" {
				require.Equal(t, testCase.text, fields[testCase.field])
			}
			for _, value := range fields {
				require.NotContains(t, value, "secret-token")
				require.NotContains(t, value, "json:")
			}
		})
	}
}

func TestMutationRequiredValidation(t *testing.T) {
	type address struct {
		City string `json:"city" validate:"required"`
	}
	type signup struct {
		Email   string  `json:"email" validate:"required,email"`
		Address address `json:"address"`
	}
	mutation := livewire.NewMutation("signup", http.MethodPost, "/signups", &livewire.MutationOptions[livewire.NoParameters, signup, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *livewire.NoParameters, *signup) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, mutation))

	_, err := mutation.DecodeBody(userContext(), []byte(`{"address":{}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "email")
	require.Equal(t, map[string]string{"email": "required", "address.city": "required"}, fields)

	_, err = mutation.DecodeBody(userContext(), []byte(`{"email":"a@b.de","address":{"city":"Stuttgart"}}`))
	require.NoError(t, err)
}

func TestMutationIndependentResourceAndFieldPermissions(t *testing.T) {
	resourceOptions := partOptions()
	resourceOptions.Fields[1].Write = authorization.IsAdmin()                                             // name: writable by admins only, readable by all
	resourceOptions.Fields[3].Read = authorization.HasRole("pricing")                                     // price: guarded for reads
	resourceOptions.Fields[3].Write = func(context.Context) error { return nil }                          // price: anyone may write
	resourceOptions.Fields[2].Read, resourceOptions.Fields[2].Write = authorization.HasRole("typer"), nil // type: resource check
	mutationOptions := createOptions()
	mutationOptions.Resource = "parts"
	mutationOptions.FieldChecks = map[string]authorization.Check{"type": authorization.HasRole("typist")}
	mutation := livewire.NewMutation("createPart", http.MethodPost, "/parts", mutationOptions)
	require.NoError(t, addError(t, livewire.NewResource("parts", "/parts", resourceOptions), mutation))
	require.Equal(t, "parts", mutation.Resource())

	_, err := mutation.DecodeBody(userContext(), []byte(`{"name":"bolt"}`))
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "name")
	_, err = mutation.DecodeBody(adminContext(), []byte(`{"name":"bolt"}`))
	require.NoError(t, err)
	_, err = mutation.DecodeBody(userContext(), []byte(`{"price":1}`))
	require.NoError(t, err, "an explicit Write check wins over Read")

	testCases := []struct {
		name  string
		roles []string
		ok    bool
	}{
		{name: "neither", roles: nil},
		{name: "resource check only", roles: []string{"typer"}},
		{name: "field check only", roles: []string{"typist"}},
		{name: "both", roles: []string{"typer", "typist"}, ok: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mutation.DecodeBody(roleContext(testCase.roles...), []byte(`{"type":"bolt"}`))
			if testCase.ok {
				require.NoError(t, err)
				return
			}
			requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "type")
		})
	}
}

func TestMutationEmbeddedRequestStruct(t *testing.T) {
	type audited struct {
		createPart
		Reason string `json:"reason"`
	}
	mutation := livewire.NewMutation("createPart", http.MethodPost, "/parts", &livewire.MutationOptions[livewire.NoParameters, audited, livewire.NoContent]{
		Check:       authorization.IsUser(),
		Resource:    "parts",
		FieldChecks: map[string]authorization.Check{"secret": authorization.HasRole("keeper")},
		Handle: func(context.Context, *livewire.NoParameters, *audited) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, livewire.NewResource("parts", "/parts", partOptions()), mutation))

	body, err := mutation.DecodeBody(userContext(), []byte(`{"name":"bolt","reason":"restock"}`))
	require.NoError(t, err)
	require.Equal(t, "bolt", body.(*audited).Name)
	_, err = mutation.DecodeBody(roleContext("keeper"), []byte(`{"secret":"s"}`))
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "secret")
	_, err = mutation.DecodeBody(roleContext("keeper", "admin"), []byte(`{"secret":"s"}`))
	require.NoError(t, err, "the embedded field is found by the resource and the field check")
	_, err = mutation.DecodeBody(userContext(), []byte(`{"createPart":{}}`))
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "createPart")
}

func TestMutationHandle(t *testing.T) {
	mutation := newCreatePart(t, createOptions())
	parameters, err := mutation.DecodeParameters(livewire.MapSource{})
	require.NoError(t, err)
	body, err := mutation.DecodeBody(userContext(), []byte(`{"name":"bolt"}`))
	require.NoError(t, err)
	response, err := mutation.Handle(userContext(), parameters, body)
	require.NoError(t, err)
	require.Equal(t, &part{Name: "bolt"}, response)
}
