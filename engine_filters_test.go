package livewire_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

func TestStoredFilterRoutes(t *testing.T) {
	bed := newTestbed(t, nil)
	ada := auth.WithIdentity(t.Context(), person("ada", roleReader))
	body := `{"resource":"gadgets","name":"Anchors","filter":{"where":{"op":"eq","field":"name","value":"Anchor"}}}`
	payload, err := bed.engine.SaveFilter(ada, []byte(body))
	require.NoError(t, err)
	var saved livewire.FilterItem
	require.NoError(t, json.Unmarshal(payload, &saved))
	require.NotEmpty(t, saved.ID)
	require.Equal(t, "Anchors", saved.Name)
	require.JSONEq(t, `{"where":{"field":"name","op":"eq","value":"Anchor"}}`, string(saved.Filter), "stored in canonical form")

	read, err := bed.engine.Read(ada, livewire.ReadRequest{Target: "gadgets", Source: livewire.MapSource{"filterId": saved.ID}})
	require.NoError(t, err, "a stored filter serves filterId")
	require.Equal(t, 0, listOf(t, read).Total)

	listed, err := bed.engine.ListFilters(ada, "gadgets")
	require.NoError(t, err)
	var list livewire.ListResponse[livewire.FilterItem]
	require.NoError(t, json.Unmarshal(listed, &list))
	require.Equal(t, []string{saved.ID}, []string{list.Items[0].ID})
	require.Equal(t, 1, list.Total)
	bob := auth.WithIdentity(t.Context(), person("bob", roleReader))
	listed, err = bed.engine.ListFilters(bob, "")
	require.NoError(t, err)
	require.JSONEq(t, `{"limit":0,"offset":0,"total":0,"items":[]}`, string(listed), "owner-only visibility")
	require.NoError(t, bed.engine.DeleteFilter(bob, saved.ID), "another owner's filter is absent, no error")
	require.NoError(t, bed.engine.DeleteFilter(ada, saved.ID))
	_, err = bed.engine.Read(ada, livewire.ReadRequest{Target: "gadgets", Source: livewire.MapSource{"filterId": saved.ID}})
	requireStatus(t, err, http.StatusNotFound)
	outcome := bed.outcomes.of(livewire.OutcomeRequest)[0]
	require.Equal(t, "filters", outcome.Target)

	refusals := []struct {
		name   string
		ctx    func() context.Context
		body   string
		status int
	}{
		{"not one object", func() context.Context { return ada }, `[]`, http.StatusBadRequest},
		{"an unknown member", func() context.Context { return ada }, `{"resource":"gadgets","filter":{},"extra":1}`, http.StatusBadRequest},
		{"no resource", func() context.Context { return ada }, `{"resource":"createGadget","filter":{}}`, http.StatusBadRequest},
		{"no filter", func() context.Context { return ada }, `{"resource":"gadgets"}`, http.StatusBadRequest},
		{"a bad filter", func() context.Context { return ada }, `{"resource":"gadgets","filter":{"where":{"field":"nothing","op":"eq","value":1}}}`, http.StatusBadRequest},
		{"the static check", func() context.Context { return t.Context() }, `{"resource":"gadgets","filter":{}}`, http.StatusUnauthorized},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			_, err := bed.engine.SaveFilter(refusal.ctx(), []byte(refusal.body))
			requireStatus(t, err, refusal.status)
		})
	}
	_, err = bed.engine.ListFilters(ada, "createGadget")
	requireStatus(t, err, http.StatusBadRequest)
	_, err = bed.engine.ListFilters(t.Context(), "")
	requireStatus(t, err, http.StatusUnauthorized)
}

func TestStoredFilterRefusesAGuardedField(t *testing.T) {
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	fields := gadgetFields()
	fields[4].Filter = true // the admin's secret
	require.NoError(t, registry.Add(livewire.NewResource("secrets", "/secrets", &livewire.ResourceOptions[gadget]{
		Read: authorization.IsUser(), Fields: fields,
		List: func(context.Context, *query.ListQuery) (*livewire.Page[gadget], error) {
			return &livewire.Page[gadget]{}, nil
		},
	})))
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, IdentitySource: newFakeIdentities(), Transactor: &fakeTransactor{}, FilterStore: newFakeFilters(), SameHostOnly: true,
	})
	require.NoError(t, err)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	_, err = engine.SaveFilter(ctx, []byte(`{"resource":"secrets","filter":{"where":{"field":"secret","op":"eq","value":"x"}}}`))
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "secret")

	withoutStore, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: newFakeIdentities(), Transactor: &fakeTransactor{}, SameHostOnly: true})
	require.NoError(t, err)
	require.Same(t, registry, withoutStore.Registry())
	_, err = withoutStore.SaveFilter(ctx, []byte(`{"resource":"secrets","filter":{}}`))
	requireStatus(t, err, http.StatusNotFound)
	_, err = withoutStore.ListFilters(ctx, "")
	requireStatus(t, err, http.StatusNotFound)
	requireStatus(t, withoutStore.DeleteFilter(ctx, "x"), http.StatusNotFound)
}

func TestLibraryRoutesAreReserved(t *testing.T) {
	for _, path := range []string{livewire.BatchPath, livewire.FiltersPath} {
		registry, err := livewire.NewRegistry(nil)
		require.NoError(t, err)
		mutation := livewire.NewMutation("taken", http.MethodPost, path, &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(),
			Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		})
		require.ErrorContains(t, registry.Add(mutation), "the library's own", path)
	}
	bed := newTestbed(t, nil)
	gadgets, _ := bed.engine.Registry().Lookup("gadgets")
	require.Equal(t, "/gadgets/presence", gadgets.(*livewire.Resource).PresencePath())
	team, _ := bed.engine.Registry().Lookup("team")
	require.Equal(t, "/teams/{team}/presence", team.(*livewire.Object).PresencePath())
	open, _ := bed.engine.Registry().Lookup("openGadgets")
	require.Empty(t, open.(*livewire.Resource).PresencePath(), "no item read, no presence")
}

func TestSubscriptionEndsAndQueryWaitsAreReported(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a"})
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets.item", map[string]string{"id": "a"}, nil))
	second := subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.True(t, socket.do(livewire.OperationUnsubscribe, second, nil, nil).OK)
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeEnd)) == 1 }, frameWait, time.Millisecond)
	ended := bed.outcomes.of(livewire.OutcomeEnd)[0]
	require.Equal(t, "gadgets", ended.Target)
	require.Equal(t, second, ended.Subscription)
	require.NoError(t, socket.connection.Close())
	require.Eventually(t, func() bool { return len(bed.outcomes.of(livewire.OutcomeEnd)) == 2 }, frameWait, time.Millisecond, "a closed socket ends its subscriptions")
	require.Equal(t, "gadgets.item", bed.outcomes.of(livewire.OutcomeEnd)[1].Target)

	require.True(t, outcomeWith(bed, livewire.OutcomeSubscribe, func(outcome livewire.Outcome) bool { return outcome.QueryWait > 0 }),
		"a socket op reports the time it took to get its query slots")

	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	_, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets"})
	require.NoError(t, err)
	rest := bed.outcomes.of(livewire.OutcomeRequest)
	require.Zero(t, rest[len(rest)-1].QueryWait, "REST takes no query slot (D52)")
}

func TestOutcomeTargetsAreAClosedSet(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	for _, target := range []string{"nothing", "gadgets.bogus", "gadgets.item"} {
		socket.do(livewire.OperationQuery, target, map[string]string{"id": "x"}, nil)
	}
	subscription := subscribed(t, socket.do(livewire.OperationSubscribe, "gadgets", nil, nil))
	require.True(t, socket.do(livewire.OperationUnsubscribe, subscription, nil, nil).OK)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	_, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets", Kind: livewire.ReadDistinct, Source: livewire.MapSource{"field": "group"}})
	require.NoError(t, err)

	var targets []string
	for _, outcome := range bed.outcomes.of(livewire.OutcomeRequest) {
		targets = append(targets, outcome.Target)
		if outcome.Operation == livewire.OperationUnsubscribe {
			require.Equal(t, subscription, outcome.Subscription)
		}
	}
	require.Equal(t, []string{"", "", "gadgets.item", "", "gadgets.distinct"}, targets, "unknown names and subscription ids report no target")

	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.ErrorContains(t, registry.Add(livewire.NewList("filters", "/filter-list", &livewire.ListOptions[gadget, livewire.NoParameters]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *livewire.NoParameters) ([]gadget, error) { return nil, nil },
	})), "reserved")
}

func TestListedFiltersAreAuthorizedAgain(t *testing.T) {
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	fields := gadgetFields()
	fields[4].Filter = true // the admin's secret
	require.NoError(t, registry.Add(livewire.NewResource("secrets", "/secrets", &livewire.ResourceOptions[gadget]{
		Read: authorization.HasRole(roleReader), Fields: fields,
		List: func(context.Context, *query.ListQuery) (*livewire.Page[gadget], error) {
			return &livewire.Page[gadget]{}, nil
		},
	})))
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, IdentitySource: newFakeIdentities(), Transactor: &fakeTransactor{}, FilterStore: newFakeFilters(), SameHostOnly: true,
	})
	require.NoError(t, err)
	save := func(filter string) {
		_, err := engine.SaveFilter(auth.WithIdentity(t.Context(), admin("ada")), []byte(`{"resource":"secrets","name":"`+filter+`","filter":{"where":{"field":"`+filter+`","op":"eq","value":"sensitive-value"}}}`))
		require.NoError(t, err)
	}
	save("secret")
	save("name")
	listed := func(identity *auth.Identity, resource string) []string {
		payload, err := engine.ListFilters(auth.WithIdentity(t.Context(), identity), resource)
		require.NoError(t, err)
		var list livewire.ListResponse[livewire.FilterItem]
		require.NoError(t, json.Unmarshal(payload, &list))
		names := []string{}
		for _, item := range list.Items {
			names = append(names, item.Name)
		}
		slices.Sort(names)
		return names
	}
	for _, resource := range []string{"", "secrets"} {
		require.Equal(t, []string{"name", "secret"}, listed(admin("ada"), resource))
		require.Equal(t, []string{"name"}, listed(person("ada", roleReader), resource), "the secret's filter left with the admin role (%q)", resource)
		require.Empty(t, listed(person("ada"), resource), "no filter of a resource the owner may no longer read (%q)", resource)
	}
}

func TestUnknownOperationsReportOther(t *testing.T) {
	bed := newTestbed(t, nil)
	socket := bed.dial("ada")
	for _, operation := range []livewire.Operation{"bogus0", "", livewire.OperationPing} {
		socket.response(socket.sendFrame(livewire.RequestFrame{Operation: operation}))
	}
	var operations []livewire.Operation
	for _, outcome := range bed.outcomes.of(livewire.OutcomeRequest) {
		operations = append(operations, outcome.Operation)
	}
	require.Equal(t, []livewire.Operation{"other", "other", livewire.OperationPing}, operations)
}

func TestStoredFiltersKeepTheirCanonicalTarget(t *testing.T) {
	bed := newTestbed(t, nil)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	payload, err := bed.engine.SaveFilter(ctx, []byte(`{"resource":"gadgets.","name":"Dotted","filter":{"where":{"field":"name","op":"eq","value":"x"}}}`))
	require.NoError(t, err)
	var saved livewire.FilterItem
	require.NoError(t, json.Unmarshal(payload, &saved))
	require.Equal(t, "gadgets", saved.Resource, "stored under the name its reads look it up by")
	_, err = bed.engine.Read(ctx, livewire.ReadRequest{Target: "gadgets", Source: livewire.MapSource{"filterId": saved.ID}})
	require.NoError(t, err, "the saved filter serves its filterId")
	listed, err := bed.engine.ListFilters(ctx, "gadgets.")
	require.NoError(t, err)
	require.Contains(t, string(listed), saved.ID)
}
