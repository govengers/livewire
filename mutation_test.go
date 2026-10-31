package livewire_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

type createPart struct {
	ID     *uuid.UUID `json:"id,omitempty"`
	Name   string     `json:"name"`
	Type   string     `json:"type"`
	Secret string     `json:"secret,omitempty"`
	Price  *float64   `json:"price,omitempty"`
	Nested struct {
		Note string `json:"note"`
	} `json:"nested"`
}

type partID struct {
	ID uuid.UUID `path:"id"`
}

func createOptions() *livewire.MutationOptions[livewire.NoParameters, createPart, part] {
	return &livewire.MutationOptions[livewire.NoParameters, createPart, part]{
		Check:  authorization.IsUser(),
		Status: http.StatusCreated,
		Handle: func(_ context.Context, _ *livewire.NoParameters, body *createPart) (*part, error) {
			return &part{Name: body.Name}, nil
		},
	}
}

func newCreatePart(t *testing.T, options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) *livewire.Mutation {
	t.Helper()
	mutation := livewire.NewMutation("createPart", http.MethodPost, "/parts", options)
	require.NoError(t, addError(t, livewire.NewResource("parts", "/parts", partOptions()), mutation))
	return mutation
}

func TestMutationDeclarationErrors(t *testing.T) {
	testCases := []struct {
		name   string
		method string
		change func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part])
		want   string
	}{
		{name: "no handle", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) { options.Handle = nil }, want: "Handle is required"},
		{name: "no check", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) { options.Check = nil }, want: "Check is required"},
		{name: "read method", method: http.MethodGet, change: func(*livewire.MutationOptions[livewire.NoParameters, createPart, part]) {}, want: "not a write method"},
		{name: "bad status", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.Status = http.StatusNotFound
		}, want: "not a success status"},
		{name: "bad client id version", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) { options.ClientID = 9 }, want: "not a ClientIDVersion"},
		{name: "field check unknown", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.FieldChecks = map[string]authorization.Check{"nope": authorization.IsAdmin()}
		}, want: `no JSON field "nope"`},
		{name: "field check nil", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.FieldChecks = map[string]authorization.Check{"name": nil}
		}, want: "is nil"},
		{name: "effect unknown kind", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.Effect = livewire.Effect{Kind: "replaces", Target: "parts"}
		}, want: "unknown"},
		{name: "effect without target", method: http.MethodPost, change: func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
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
		err := addError(t, livewire.NewMutation("rename", http.MethodPut, "/parts", &livewire.MutationOptions[livewire.NoParameters, unkeyed, livewire.NoContent]{
			Check:    authorization.IsUser(),
			ClientID: livewire.UUIDv7,
			Handle: func(context.Context, *livewire.NoParameters, *unkeyed) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		}))
		require.ErrorContains(t, err, `no JSON field "id"`)
	})
	t.Run("client id without body", func(t *testing.T) {
		err := addError(t, livewire.NewMutation("deletePart", http.MethodDelete, "/parts/{id}", &livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent]{
			Check:    authorization.IsUser(),
			ClientID: livewire.UUIDv4,
			Handle: func(context.Context, *partID, *livewire.NoBody) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		}))
		require.ErrorContains(t, err, "ClientID needs a body")
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

	deletePart := livewire.NewMutation("deletePart", http.MethodDelete, "/parts/{id}", &livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent]{
		Check:  authorization.IsUser(),
		Effect: livewire.Removes("parts"),
		Handle: func(context.Context, *partID, *livewire.NoBody) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, livewire.NewResource("parts", "/parts", partOptions()), deletePart))
	require.Equal(t, http.StatusNoContent, deletePart.Status())
	require.False(t, deletePart.HasBody())
	require.False(t, deletePart.HasContent())
	require.Equal(t, livewire.Effect{Kind: livewire.EffectRemoves, Target: "parts"}, deletePart.Effect())
	require.Equal(t, livewire.Upserts("parts"), livewire.Effect{Kind: livewire.EffectUpserts, Target: "parts"})
	body, err := deletePart.DecodeBody(userContext(), []byte(`garbage`))
	require.NoError(t, err)
	require.Equal(t, &livewire.NoBody{}, body)

	renamed := livewire.NewMutation("renamePart", http.MethodPatch, "/parts/{id}", &livewire.MutationOptions[partID, createPart, livewire.NoContent]{
		Check: authorization.IsUser(),
		Handle: func(context.Context, *partID, *createPart) (*livewire.NoContent, error) {
			return &livewire.NoContent{}, nil
		},
	})
	require.NoError(t, addError(t, renamed))
	require.Equal(t, http.StatusNoContent, renamed.Status())
}

func TestMutationDecodeBody(t *testing.T) {
	options := createOptions()
	options.ClientID = livewire.UUIDv7
	options.Resource = "parts"
	options.FieldChecks = map[string]authorization.Check{"price": authorization.HasRole("pricing")}
	mutation := newCreatePart(t, options)
	versionSeven := "0192f1a0-1b2c-7d3e-8f40-516273849506"
	versionFour := "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"
	pricingContext := func() context.Context {
		return auth.WithIdentity(context.Background(), &auth.Identity{Provider: "password", Subject: "p", Active: true, Roles: []string{"pricing"}})
	}
	pricingAdmin := auth.WithIdentity(context.Background(), &auth.Identity{Provider: "password", Subject: "p", Active: true, Roles: []string{"pricing", auth.RoleAdmin}})

	t.Run("accepted", func(t *testing.T) {
		body, err := mutation.DecodeBody(userContext(), []byte(`{"id":"`+versionSeven+`","name":"bolt","type":"bolt","nested":{"note":"n"}}`))
		require.NoError(t, err)
		decoded, ok := body.(*createPart)
		require.True(t, ok)
		require.Equal(t, "bolt", decoded.Name)
		require.Equal(t, uuid.MustParse(versionSeven), *decoded.ID)
		require.Equal(t, "n", decoded.Nested.Note)
	})
	t.Run("admin with pricing writes every guarded field", func(t *testing.T) {
		_, err := mutation.DecodeBody(pricingAdmin, []byte(`{"name":"bolt","secret":"s","price":1}`))
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
		{name: "malformed", identity: userContext, raw: `{"name":`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body"},
		{name: "unknown field names it", identity: userContext, raw: `{"name":"bolt","colour":"red"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "colour", text: "unknown field"},
		{name: "unknown nested field", identity: userContext, raw: `{"name":"bolt","nested":{"nope":1}}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "body"},
		{name: "wrong type names the field", identity: userContext, raw: `{"name":7}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "name", text: "must be a string"},
		{name: "unknown before forbidden", identity: userContext, raw: `{"secret":"s","colour":"red"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "colour"},
		{name: "client id of the wrong version", identity: userContext, raw: `{"id":"` + versionFour + `","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id", text: "must be a UUID version 7"},
		{name: "client id not a uuid", identity: userContext, raw: `{"id":"nope","name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "client id not a string", identity: userContext, raw: `{"id":7,"name":"bolt"}`, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "id"},
		{name: "resource write check inherited from read", identity: userContext, raw: `{"name":"bolt","secret":"s"}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "field check", identity: userContext, raw: `{"name":"bolt","price":1}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "price"},
		{name: "field check passes, resource check refuses", identity: pricingContext, raw: `{"name":"bolt","price":1,"secret":"s"}`, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := mutation.DecodeBody(testCase.identity(), []byte(testCase.raw))
			fields := requireError(t, err, testCase.status, testCase.code, testCase.field)
			if testCase.text != "" {
				require.Equal(t, testCase.text, fields[testCase.field])
			}
		})
	}
}

func TestMutationExplicitWriteCheckAndBothChecks(t *testing.T) {
	resourceOptions := partOptions()
	resourceOptions.Fields[1].Write = authorization.IsAdmin()                                     // name: writable by admins only, readable by all
	resourceOptions.Fields[3].Read = authorization.HasRole("pricing")                             // price: guarded for reads
	resourceOptions.Fields[3].Write = func(context.Context) error { return nil }                  // price: anyone may write
	resourceOptions.Fields[2].Read, resourceOptions.Fields[2].Write = authorization.IsUser(), nil // type: plain user check
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
	_, err = mutation.DecodeBody(userContext(), []byte(`{"type":"bolt"}`))
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "type")
	typist := auth.WithIdentity(context.Background(), &auth.Identity{Provider: "password", Subject: "t", Active: true, Roles: []string{"typist"}})
	_, err = mutation.DecodeBody(typist, []byte(`{"type":"bolt"}`))
	require.NoError(t, err, "a field with a resource check and a field check needs both")
	_, err = mutation.DecodeBody(context.Background(), []byte(`{"type":"bolt"}`))
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "type")
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
