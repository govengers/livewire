package livewire_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

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
	createMutation := func(options func(*livewire.MutationOptions[livewire.NoParameters, createPart, part])) *livewire.Mutation {
		mutationOptions := createOptions()
		options(mutationOptions)
		return livewire.NewMutation("createPart", http.MethodPost, "/parts", mutationOptions)
	}
	testCases := []struct {
		name         string
		declarations []livewire.Declaration
		want         string
	}{
		{name: "construction error", declarations: []livewire.Declaration{livewire.NewResource[part]("parts", "/parts", nil)}, want: `resource "parts": Check is required`},
		{name: "duplicate name", declarations: []livewire.Declaration{parts(), newDashboard("parts", "/dashboard")}, want: `object "parts": the name is already registered`},
		{name: "duplicate route", declarations: []livewire.Declaration{parts(), newDashboard("dashboard", "/parts")}, want: `the route GET /parts is already registered by "parts"`},
		{name: "duplicate item route", declarations: []livewire.Declaration{parts(), newDashboard("dashboard", "/parts/distinct")}, want: `the route GET /parts/distinct is already registered`},
		{name: "mutation resource missing", declarations: []livewire.Declaration{createMutation(func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.Resource = "nope"
		})}, want: `Resource "nope" is not a registered resource`},
		{name: "mutation resource is a list", declarations: []livewire.Declaration{newDashboard("dashboard", "/dashboard"), createMutation(func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.Resource = "dashboard"
		})}, want: `Resource "dashboard" is not a registered resource`},
		{name: "effect target missing", declarations: []livewire.Declaration{createMutation(func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.Effect = livewire.Upserts("nope")
		})}, want: `Effect target "nope" is not a registered list`},
		{name: "effect target is an object", declarations: []livewire.Declaration{newDashboard("dashboard", "/dashboard"), createMutation(func(options *livewire.MutationOptions[livewire.NoParameters, createPart, part]) {
			options.Effect = livewire.Upserts("dashboard")
		})}, want: `Effect target "dashboard" is not a registered list`},
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

	declarations := registry.Declarations()
	require.Len(t, declarations, 4)
	require.Equal(t, []string{"createPart", "parts", "stats", "dashboard"}, []string{declarations[0].Name(), declarations[1].Name(), declarations[2].Name(), declarations[3].Name()})
	declarations[0] = nil
	require.Equal(t, "createPart", registry.Declarations()[0].Name(), "Declarations returns a copy")

	found, ok := registry.Lookup("parts")
	require.True(t, ok)
	require.Same(t, parts, found)
	_, ok = registry.Lookup("nope")
	require.False(t, ok)

	require.ErrorContains(t, registry.Add(newDashboard("parts", "/x")), "already registered")
	require.Len(t, registry.Declarations(), 4)
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
	stats := livewire.NewList("stats", "/stats", &livewire.ListOptions[part, statParameters]{
		Tables:  []string{"parts"},
		Read:    authorization.IsUser(),
		Guarded: []livewire.GuardedField{{Name: "secret", Read: authorization.IsAdmin()}},
		Load: func(_ context.Context, parameters *statParameters) ([]part, error) {
			if parameters.Since == "fail" {
				return nil, livewire.NotFound()
			}
			return nil, nil
		},
	})
	dashboardObject := livewire.NewObject("dashboard", "/dashboard", &livewire.ObjectOptions[dashboard, livewire.NoParameters]{
		Read:    authorization.IsUser(),
		Guarded: []livewire.GuardedField{{Name: "open", Read: authorization.IsAdmin()}},
		Load:    func(context.Context, *livewire.NoParameters) (*dashboard, error) { return nil, nil }, //nolint:nilnil // provokes the no-item-no-error failure
	})
	require.NoError(t, addError(t, stats, dashboardObject))

	require.Equal(t, livewire.KindList, stats.Kind())
	require.Equal(t, http.MethodGet, stats.Method())
	require.Equal(t, []string{"parts"}, stats.Tables())
	require.Len(t, stats.Parameters(), 1)
	require.Len(t, stats.Fields(), 1)
	require.Equal(t, "secret", stats.Fields()[0].Name)
	require.NotNil(t, stats.Fields()[0].Read)
	items, err := stats.Load(userContext(), &statParameters{})
	require.NoError(t, err)
	require.Equal(t, []part{}, items)
	_, err = stats.Load(userContext(), &statParameters{Since: "fail"})
	requireError(t, err, http.StatusNotFound, livewire.CodeNotFound, "")

	require.Equal(t, livewire.KindObject, dashboardObject.Kind())
	require.Equal(t, "open", dashboardObject.Fields()[0].Name)
	_, err = dashboardObject.Load(userContext(), &livewire.NoParameters{})
	require.ErrorContains(t, err, "no item and no error")
	item, err := newDashboard("d", "/d").Load(userContext(), &livewire.NoParameters{})
	require.NoError(t, err)
	require.Equal(t, &dashboard{Open: 1}, item)
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
		{name: "list without name", declaration: livewire.NewList("", "/s", &livewire.ListOptions[part, livewire.NoParameters]{Read: authorization.IsUser()}), want: "needs a name"},
		{name: "live list without id", declaration: livewire.NewList("s", "/s", &livewire.ListOptions[withoutID, livewire.NoParameters]{
			Tables: []string{"t"}, Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]withoutID, error) { return nil, nil },
		}), want: `no JSON field "id"`},
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

func TestContextCarriers(t *testing.T) {
	ctx := context.Background()
	require.Nil(t, livewire.TransactionFrom(ctx))
	require.Empty(t, livewire.IdempotencyKeyFrom(ctx))
	transaction := &fakeTransaction{}
	ctx = livewire.WithIdempotencyKey(livewire.WithTransaction(ctx, transaction), "key")
	require.Same(t, transaction, livewire.TransactionFrom(ctx))
	require.Equal(t, "key", livewire.IdempotencyKeyFrom(ctx))
}

type fakeTransaction struct{}

func (f *fakeTransaction) Commit(context.Context) error   { return nil }
func (f *fakeTransaction) Rollback(context.Context) error { return nil }
