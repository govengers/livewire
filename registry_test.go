package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

type dashboard struct {
	Open int `json:"open"`
}

func newDashboard(name, path string) *livewire.Object {
	return livewire.NewObject(name, path, &livewire.ObjectOptions[dashboard, livewire.NoParameters]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) (*dashboard, error) { return &dashboard{Open: 1}, nil },
	})
}

type otherPartID struct {
	PartID uuid.UUID `path:"partId"`
}

func newParameterizedList(name, path string) *livewire.List {
	return livewire.NewList(name, path, &livewire.ListOptions[part, partID]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *partID) ([]part, error) { return nil, nil },
	})
}

func newOtherParameterizedList(name, path string) *livewire.List {
	return livewire.NewList(name, path, &livewire.ListOptions[part, otherPartID]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *otherPartID) ([]part, error) { return nil, nil },
	})
}

// wrapped embeds a declaration kind, which forwards every method of the interface.
type wrapped struct {
	*livewire.Mutation
}

// nilWrapped embeds the interface itself, nil: every promoted call dispatches through nil.
type nilWrapped struct {
	livewire.Declaration
}

func TestNewRegistryValidatesBasePath(t *testing.T) {
	for _, basePath := range []string{"api", "/api/"} {
		_, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: basePath})
		require.ErrorContains(t, err, "BasePath")
	}
	options := &livewire.RegistryOptions{BasePath: "/api/v1"}
	registry, err := livewire.NewRegistry(options)
	require.NoError(t, err)
	require.Equal(t, "/api/v1", registry.BasePath())
	options.BasePath = "/changed"
	require.Equal(t, "/api/v1", registry.BasePath(), "the options are copied")

	registry, err = livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.Empty(t, registry.BasePath())
}

func TestRegistryAddRefusals(t *testing.T) {
	parts := func() *livewire.Resource { return livewire.NewResource("parts", "/parts", partOptions()) }
	createMutation := func(options func(*createOptionsType)) *livewire.Mutation {
		mutationOptions := createOptions()
		options(mutationOptions)
		return livewire.NewMutation("createPart", http.MethodPost, "/parts", mutationOptions)
	}
	var typedNil *livewire.Resource
	testCases := []struct {
		name         string
		declarations []livewire.Declaration
		want         string
	}{
		{name: "nil", declarations: []livewire.Declaration{nil}, want: "a nil declaration"},
		{name: "typed nil", declarations: []livewire.Declaration{typedNil}, want: "a nil declaration"},
		{name: "wrapped declaration", declarations: []livewire.Declaration{&wrapped{Mutation: createMutation(func(*createOptionsType) {})}}, want: "wraps a declaration"},
		{name: "wrapper around a nil declaration", declarations: []livewire.Declaration{&nilWrapped{}}, want: "wraps a declaration"},
		{name: "duplicate route with another placeholder name", declarations: []livewire.Declaration{parts(), newOtherParameterizedList("byPart", "/parts/{partId}")}, want: `the route GET /parts/{partId} is already registered by "parts"`},
		{name: "removes without an id path parameter", declarations: []livewire.Declaration{parts(), createMutation(func(options *createOptionsType) { options.Effect = livewire.Removes("parts") })}, want: `Removes needs the path parameter "id"`},
		{name: "upserts without content", declarations: []livewire.Declaration{parts(), newBodyless(http.MethodDelete, func(options *livewire.MutationOptions[partID, livewire.NoBody, livewire.NoContent]) {
			options.Effect = livewire.Upserts("parts")
		})}, want: "Upserts needs a response to upsert"},
		{name: "effect target not live", declarations: []livewire.Declaration{newParameterizedList("byPart", "/by/{id}"), createMutation(func(options *createOptionsType) { options.Effect = livewire.Upserts("byPart") })}, want: `Effect target "byPart" is not live`},
		{name: "upserts with a scalar response", declarations: []livewire.Declaration{parts(), livewire.NewMutation("rename", http.MethodPost, "/rename", &livewire.MutationOptions[livewire.NoParameters, createPart, string]{
			Check: authorization.IsUser(), Effect: livewire.Upserts("parts"),
			Handle: func(context.Context, *livewire.NoParameters, *createPart) (*string, error) { return new("x"), nil },
		})}, want: "is a scalar and can carry no JSON field"},
		{name: "upserts with a response without id", declarations: []livewire.Declaration{parts(), livewire.NewMutation("rename", http.MethodPost, "/rename", &livewire.MutationOptions[livewire.NoParameters, createPart, dashboard]{
			Check: authorization.IsUser(), Effect: livewire.Upserts("parts"),
			Handle: func(context.Context, *livewire.NoParameters, *createPart) (*dashboard, error) {
				return &dashboard{}, nil
			},
		})}, want: `no JSON field "id" (Upserts keys the optimistic item by it)`},
		{name: "construction error", declarations: []livewire.Declaration{livewire.NewResource[part]("parts", "/parts", nil)}, want: `resource "parts": Check is required`},
		{name: "duplicate name", declarations: []livewire.Declaration{parts(), newDashboard("parts", "/dashboard")}, want: `object "parts": the name is already registered`},
		{name: "duplicate route", declarations: []livewire.Declaration{parts(), newDashboard("dashboard", "/parts")}, want: `the route GET /parts is already registered by "parts"`},
		{name: "duplicate distinct route", declarations: []livewire.Declaration{parts(), newDashboard("dashboard", "/parts/distinct")}, want: `the route GET /parts/distinct is already registered`},
		{name: "duplicate item route via a list", declarations: []livewire.Declaration{parts(), newParameterizedList("byPart", "/parts/{id}")}, want: `the route GET /parts/{id} is already registered by "parts"`},
		{name: "mutation resource missing", declarations: []livewire.Declaration{createMutation(func(options *createOptionsType) { options.Resource = "nope" })}, want: `Resource "nope" is not a registered resource`},
		{name: "mutation resource is an object", declarations: []livewire.Declaration{newDashboard("dashboard", "/dashboard"), createMutation(func(options *createOptionsType) { options.Resource = "dashboard" })}, want: `Resource "dashboard" is not a registered resource`},
		{name: "effect target missing", declarations: []livewire.Declaration{createMutation(func(options *createOptionsType) { options.Effect = livewire.Upserts("nope") })}, want: `Effect target "nope" is not a registered list`},
		{name: "effect target is an object", declarations: []livewire.Declaration{newDashboard("dashboard", "/dashboard"), createMutation(func(options *createOptionsType) { options.Effect = livewire.Upserts("dashboard") })}, want: `Effect target "dashboard" is not a registered list`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registry, err := livewire.NewRegistry(nil)
			require.NoError(t, err)
			err = registry.Add(testCase.declarations...)
			require.ErrorContains(t, err, testCase.want)
			require.Empty(t, registry.Declarations(), "nothing is added when one declaration fails")
		})
	}
}

func TestRegistryAddAndLookup(t *testing.T) {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api"})
	require.NoError(t, err)
	parts := livewire.NewResource("parts", "/parts", partOptions())
	createOptions := createOptions()
	createOptions.Resource = "parts"
	createOptions.Effect = livewire.Upserts("stats")
	create := livewire.NewMutation("createPart", http.MethodPost, "/parts", createOptions)
	stats := livewire.NewList("stats", "/stats", &livewire.ListOptions[part, livewire.NoParameters]{
		Tables: []string{"parts"},
		Read:   authorization.IsUser(),
		Load:   func(context.Context, *livewire.NoParameters) ([]part, error) { return nil, nil },
	})
	require.NoError(t, registry.Add(create, parts, stats), "bindings resolve against the whole call")
	require.NoError(t, registry.Add(newDashboard("dashboard", "/dashboard")))
	require.NoError(t, registry.Add(newParameterizedList("byPart", "/parts/{id}/stats")), "a sibling route below the item route is fine")

	declarations := registry.Declarations()
	require.Len(t, declarations, 5)
	require.Equal(t, []string{"createPart", "parts", "stats", "dashboard"}, []string{declarations[0].Name(), declarations[1].Name(), declarations[2].Name(), declarations[3].Name()})
	declarations[0] = nil
	require.Equal(t, "createPart", registry.Declarations()[0].Name(), "Declarations returns a copy")

	found, ok := registry.Lookup("parts")
	require.True(t, ok)
	require.Same(t, parts, found)
	_, ok = registry.Lookup("nope")
	require.False(t, ok)

	require.ErrorContains(t, registry.Add(newDashboard("parts", "/x")), "already registered")
	require.Len(t, registry.Declarations(), 5)

	other, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.ErrorContains(t, other.Add(parts), `resource "parts" is registered with another registry`)
}

func TestRegistryAddLeavesDeclarationsUntouchedOnFailure(t *testing.T) {
	strict := partOptions()
	strict.Fields[1].Write = authorization.IsAdmin() // name is admin-only on this registry's resource
	boundOptions := createOptions()
	boundOptions.Resource = "parts"
	create := livewire.NewMutation("createPart", http.MethodPost, "/parts", boundOptions)

	failing, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	unbound := createOptions()
	unbound.Effect = livewire.Upserts("nope")
	err = failing.Add(livewire.NewResource("parts", "/parts", strict), create, livewire.NewMutation("other", http.MethodPost, "/other", unbound))
	require.ErrorContains(t, err, `Effect target "nope"`, "the later declaration fails in bind, after create was bound in staging")
	require.Empty(t, failing.Declarations())
	_, err = create.DecodeBody(userContext(), []byte(`{"name":"bolt"}`))
	require.NoError(t, err, "the failed Add published no binding: name is not admin-only on create")

	open, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, open.Add(livewire.NewResource("parts", "/parts", partOptions()), create), "the failed Add left the mutation unbound")
	_, err = create.DecodeBody(userContext(), []byte(`{"name":"bolt"}`))
	require.NoError(t, err, "the binding is the open registry's, not the failed one's")

	again, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	err = again.Add(livewire.NewResource("parts", "/parts", strict), create)
	require.ErrorContains(t, err, "registered with another registry")
	_, err = create.DecodeBody(userContext(), []byte(`{"name":"bolt"}`))
	require.NoError(t, err, "the refused Add changed nothing")
}

func TestRegistryTenantAndObserve(t *testing.T) {
	single, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	tenant, err := single.Tenant(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, tenant)
	single.Observe(context.Background(), livewire.Outcome{Kind: livewire.OutcomeRequest})

	var observed []livewire.Outcome
	multi, err := livewire.NewRegistry(&livewire.RegistryOptions{
		Tenant: func(_ context.Context, parameters any) (string, error) {
			switch parameters := parameters.(type) {
			case *partID:
				return parameters.ID.String(), nil
			case nil:
				return "", nil
			default:
				return "", errors.New("no tenant")
			}
		},
		Observer: func(_ context.Context, outcome livewire.Outcome) { observed = append(observed, outcome) },
	})
	require.NoError(t, err)
	tenant, err = multi.Tenant(context.Background(), &partID{})
	require.NoError(t, err)
	require.Equal(t, "00000000-0000-0000-0000-000000000000", tenant)
	_, err = multi.Tenant(context.Background(), nil)
	require.ErrorContains(t, err, "no tenant and no error")
	_, err = multi.Tenant(context.Background(), "x")
	require.ErrorContains(t, err, "no tenant")
	multi.Observe(context.Background(), livewire.Outcome{Kind: livewire.OutcomeCall, Target: "createPart", Status: http.StatusCreated})
	require.Len(t, observed, 1)
	require.Equal(t, "createPart", observed[0].Target)
}

func TestTypedDeclarations(t *testing.T) {
	type statParameters struct {
		Since string `query:"since"`
	}
	partition := func(_ context.Context, parameters any) string { return "since:" + parameters.(*statParameters).Since }
	options := &livewire.ListOptions[part, statParameters]{
		Tables:    []string{"parts"},
		Read:      authorization.IsUser(),
		Partition: partition,
		Guarded:   []livewire.GuardedField{{Name: "secret", Read: authorization.IsAdmin()}},
		Load: func(_ context.Context, parameters *statParameters) ([]part, error) {
			if parameters.Since == "fail" {
				return nil, livewire.NotFound()
			}
			return nil, nil
		},
	}
	stats := livewire.NewList("stats", "/stats", options)
	options.Load, options.Partition, options.Guarded[0].Name = nil, nil, "changed"
	dashboardObject := livewire.NewObject("dashboard", "/dashboard", &livewire.ObjectOptions[dashboard, livewire.NoParameters]{
		Read:      authorization.IsUser(),
		Partition: func(context.Context, any) string { return "all" },
		Guarded:   []livewire.GuardedField{{Name: "open", Read: authorization.IsAdmin()}},
		Load:      func(context.Context, *livewire.NoParameters) (*dashboard, error) { return nil, nil }, //nolint:nilnil // provokes the no-item-no-error failure
	})
	require.NoError(t, addError(t, stats, dashboardObject))

	require.Equal(t, livewire.KindList, stats.Kind())
	require.Equal(t, http.MethodGet, stats.Method())
	require.Equal(t, []string{"parts"}, stats.Tables())
	require.Len(t, stats.Parameters(), 1)
	require.Len(t, stats.Fields(), 1)
	require.Equal(t, "secret", stats.Fields()[0].Name, "the options were copied")
	require.NotNil(t, stats.Fields()[0].Read)
	require.Equal(t, "since:x", stats.Partition()(context.Background(), &statParameters{Since: "x"}))
	items, err := stats.Load(userContext(), &statParameters{})
	require.NoError(t, err)
	require.Equal(t, []part{}, items)
	_, err = stats.Load(userContext(), &statParameters{Since: "fail"})
	requireError(t, err, http.StatusNotFound, livewire.CodeNotFound, "")

	require.Equal(t, livewire.KindObject, dashboardObject.Kind())
	require.Equal(t, "open", dashboardObject.Fields()[0].Name)
	require.Equal(t, "all", dashboardObject.Partition()(context.Background(), nil))
	_, err = dashboardObject.Load(userContext(), &livewire.NoParameters{})
	require.ErrorContains(t, err, "no item and no error")
	item, err := newDashboard("d", "/d").Load(userContext(), &livewire.NoParameters{})
	require.NoError(t, err)
	require.Equal(t, &dashboard{Open: 1}, item)
	require.Nil(t, newDashboard("d", "/d").Partition())
}

func TestTypedDeclarationErrors(t *testing.T) {
	type withoutID struct {
		Name string `json:"name"`
	}
	testCases := []struct {
		name        string
		declaration livewire.Declaration
		want        string
	}{
		{name: "list without load", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[part, livewire.NoParameters]{Read: authorization.IsUser()}), want: "Load is required"},
		{name: "list nil options", declaration: livewire.NewList[part, livewire.NoParameters]("s", "/s", nil), want: "Check is required"},
		{name: "list without name", declaration: livewire.NewList("", "/s", &livewire.ListOptions[part, livewire.NoParameters]{Read: authorization.IsUser()}), want: "is not an identifier"},
		{name: "live list without id", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[withoutID, livewire.NoParameters]{
			Tables: []string{"t"}, Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]withoutID, error) { return nil, nil },
		}), want: `no JSON field "id"`},
		{name: "live list of pointers without id", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[*withoutID, livewire.NoParameters]{
			Tables: []string{"t"}, Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]*withoutID, error) { return nil, nil },
		}), want: `no JSON field "id"`},
		{name: "live list of scalars", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[int, livewire.NoParameters]{
			Tables: []string{"t"}, Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]int, error) { return nil, nil },
		}), want: "is a scalar and can carry no JSON field"},
		{name: "live list of string slices", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[[]string, livewire.NoParameters]{
			Tables: []string{"t"}, Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([][]string, error) { return nil, nil },
		}), want: "is a scalar and can carry no JSON field"},
		{name: "live list with a guarded id", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[part, livewire.NoParameters]{
			Tables: []string{"t"}, Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "id", Read: authorization.IsAdmin()}},
			Load: func(context.Context, *livewire.NoParameters) ([]part, error) { return nil, nil },
		}), want: `the field "id" of a live list cannot be guarded`},
		{name: "guard unknown field", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[part, livewire.NoParameters]{
			Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "nope", Read: authorization.IsAdmin()}},
			Load: func(context.Context, *livewire.NoParameters) ([]part, error) { return nil, nil },
		}), want: `no JSON field "nope"`},
		{name: "guard without read", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[part, livewire.NoParameters]{
			Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "secret"}},
			Load: func(context.Context, *livewire.NoParameters) ([]part, error) { return nil, nil },
		}), want: "needs Name and Read"},
		{name: "guard twice", declaration: livewire.NewObject("o", "/o", &livewire.ObjectOptions[part, livewire.NoParameters]{
			Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "secret", Read: authorization.IsAdmin()}, {Name: "secret", Read: authorization.IsAdmin()}},
			Load: func(context.Context, *livewire.NoParameters) (*part, error) { return &part{}, nil },
		}), want: "guarded twice"},
		{name: "object without load", declaration: livewire.NewObject("o", "/o", &livewire.ObjectOptions[part, livewire.NoParameters]{Read: authorization.IsUser()}), want: "Load is required"},
		{name: "object nil options", declaration: livewire.NewObject[part, livewire.NoParameters]("o", "/o", nil), want: "Check is required"},
		{name: "object bad parameters", declaration: livewire.NewObject("o", "/o/{id}", &livewire.ObjectOptions[part, livewire.NoParameters]{
			Read: authorization.IsUser(), Load: func(context.Context, *livewire.NoParameters) (*part, error) { return &part{}, nil },
		}), want: "no field tagged"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			require.ErrorContains(t, addError(t, testCase.declaration), testCase.want)
		})
	}
}

func TestObjectCopiesOptions(t *testing.T) {
	options := &livewire.ObjectOptions[dashboard, livewire.NoParameters]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) (*dashboard, error) { return &dashboard{Open: 2}, nil },
	}
	object := livewire.NewObject("dashboard", "/dashboard", options)
	require.NoError(t, addError(t, object))
	options.Load = nil
	item, err := object.Load(userContext(), &livewire.NoParameters{})
	require.NoError(t, err)
	require.Equal(t, &dashboard{Open: 2}, item)
}

func TestLiveRawMessageItems(t *testing.T) {
	list := livewire.NewList("raw", "/raw", &livewire.ListOptions[json.RawMessage, livewire.NoParameters]{
		Tables: []string{"t"}, Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]json.RawMessage, error) { return nil, nil },
	})
	require.NoError(t, addError(t, list), "a marshaler type may carry an id; it cannot be checked at Add")
}

type badlyNamed struct {
	ID     string `json:"id"`
	Hidden string `json:"bad'name"` //nolint:staticcheck // SA5008: the malformed tag is the subject of the test
	Quoted string `json:"'quoted'"` // a tag name encoding/json drops for the Go name
}

// TestOddJSONTagNamesFollowEncodingJSON pins that the field model names a field exactly as
// the toolchain's encoding/json marshals it, whatever it makes of an odd tag.
func TestOddJSONTagNamesFollowEncodingJSON(t *testing.T) {
	marshaled, err := json.Marshal(badlyNamed{ID: "a", Hidden: "h", Quoted: "q"})
	require.NoError(t, err)
	var keys map[string]string
	require.NoError(t, json.Unmarshal(marshaled, &keys))
	require.Len(t, keys, 3)
	require.NotContains(t, keys, "bad'name")
	require.Contains(t, keys, "Quoted", "an unusable name falls back to the Go name")
	var secretKey string
	for key, value := range keys {
		if value == "h" {
			secretKey = key
		}
	}
	require.NotEmpty(t, secretKey)

	load := func(context.Context, *livewire.NoParameters) ([]badlyNamed, error) { return nil, nil }
	err = addError(t, livewire.NewList("s", "/s", &livewire.ListOptions[badlyNamed, livewire.NoParameters]{
		Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "bad'name", Read: authorization.IsAdmin()}}, Load: load,
	}))
	require.ErrorContains(t, err, `no JSON field "bad'name"`, "a guard under the tag as written protects nothing")

	list := livewire.NewList("s", "/s", &livewire.ListOptions[badlyNamed, livewire.NoParameters]{
		Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: secretKey, Read: authorization.IsAdmin()}, {Name: "Quoted", Read: authorization.IsAdmin()}}, Load: load,
	})
	require.NoError(t, addError(t, list))
	redacted, err := livewire.RedactItems(userContext(), list.Fields(), json.RawMessage(`[`+string(marshaled)+`]`))
	require.NoError(t, err)
	require.JSONEq(t, `[{"id":"a"}]`, string(redacted), "the guard under the marshaled name strips the value")
}

func TestLivePointerItemsWithID(t *testing.T) {
	type keyed struct {
		ID string `json:"id"`
	}
	list := livewire.NewList("s", "/s", &livewire.ListOptions[*keyed, livewire.NoParameters]{
		Tables: []string{"t"}, Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]*keyed, error) { return nil, nil },
	})
	require.NoError(t, addError(t, list))
}

type fakeHandle struct{ name string }

func TestContextCarriers(t *testing.T) {
	ctx := context.Background()
	require.Nil(t, livewire.TransactionFrom(ctx))
	require.Empty(t, livewire.IdempotencyKeyFrom(ctx))
	require.Empty(t, livewire.TenantFrom(ctx))
	_, ok := livewire.OwnerOf(ctx)
	require.False(t, ok, "no identity, no owner")

	handle := &fakeHandle{name: "tx"}
	ctx = livewire.WithTenant(livewire.WithIdempotencyKey(livewire.WithTransaction(ctx, handle), "key"), "t1")
	typed, ok := livewire.TransactionFrom(ctx).(*fakeHandle)
	require.True(t, ok, "the stored value is the adapter's handle, asserted back to its type")
	require.Same(t, handle, typed)
	require.Equal(t, "key", livewire.IdempotencyKeyFrom(ctx))
	require.Equal(t, "t1", livewire.TenantFrom(ctx))

	ctx = auth.WithIdentity(ctx, &auth.Identity{Provider: "password", Subject: "u1"})
	owner, ok := livewire.OwnerOf(ctx)
	require.True(t, ok)
	require.Equal(t, livewire.Owner{Key: "password:u1", Tenant: "t1"}, owner)
}
