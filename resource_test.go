package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

func TestResourceDeclarationErrors(t *testing.T) {
	type withoutID struct {
		Name string `json:"name"`
	}
	testCases := []struct {
		name   string
		change func(options *livewire.ResourceOptions[part])
		want   string
	}{
		{name: "no list", change: func(options *livewire.ResourceOptions[part]) { options.List = nil }, want: "List is required"},
		{name: "no read", change: func(options *livewire.ResourceOptions[part]) { options.Read = nil }, want: "Check is required"},
		{name: "no fields", change: func(options *livewire.ResourceOptions[part]) { options.Fields = nil }, want: "Fields is required"},
		{name: "invalid field", change: func(options *livewire.ResourceOptions[part]) { options.Fields[0].SQL = "" }, want: "no SQL expression"},
		{name: "negative limit", change: func(options *livewire.ResourceOptions[part]) { options.MaximumLimit = -1 }, want: "must not be negative"},
		{name: "default sort unknown", change: func(options *livewire.ResourceOptions[part]) { options.DefaultSort = []query.SortTerm{{Field: "nope"}} }, want: "not a sortable field"},
		{name: "default sort guarded", change: func(options *livewire.ResourceOptions[part]) {
			options.DefaultSort = []query.SortTerm{{Field: "secret"}}
		}, want: "guarded field"},
		{name: "default sort twice", change: func(options *livewire.ResourceOptions[part]) {
			options.DefaultSort = []query.SortTerm{{Field: "name"}, {Field: "name", Descending: true}}
		}, want: `DefaultSort names "name" twice`},
		{name: "export column unknown", change: func(options *livewire.ResourceOptions[part]) { options.ExportColumns = []string{"nope"} }, want: "not a field"},
		{name: "get without item", change: func(options *livewire.ResourceOptions[part]) { options.Item = nil }, want: "Get loader needs Item"},
		{name: "item without get", change: func(options *livewire.ResourceOptions[part]) { options.Get = nil }, want: "Get loader is required with Item"},
		{name: "item without parameter", change: func(options *livewire.ResourceOptions[part]) { options.Item.Parameter = "" }, want: "Item needs Parameter and Field"},
		{name: "item parameter spliced into the route", change: func(options *livewire.ResourceOptions[part]) { options.Item.Parameter = "id}/extra/{other" }, want: "Item.Parameter"},
		{name: "item field unknown", change: func(options *livewire.ResourceOptions[part]) { options.Item.Field = "nope" }, want: "not a filterable field"},
		{name: "item field of a float type", change: func(options *livewire.ResourceOptions[part]) { options.Item.Field = "price" }, want: "an item key is uuid, string, text, enum, int or time"},
		{name: "item field not in the item type", change: func(options *livewire.ResourceOptions[part]) {
			options.Fields = append(options.Fields, query.Field{Name: "sku", Type: query.TypeString, SQL: "parts.sku", Filter: true})
			options.Item.Field = "sku"
		}, want: `no JSON field "sku"`},
		{name: "guarded field not in the item type", change: func(options *livewire.ResourceOptions[part]) {
			options.Fields = append(options.Fields, query.Field{Name: "secretValue", Type: query.TypeString, SQL: "parts.secret", Read: authorization.IsAdmin()})
		}, want: `no JSON field "secretValue" (a guarded field is redacted by it)`},
		{name: "write-guarded field not in the item type", change: func(options *livewire.ResourceOptions[part]) {
			options.Fields = append(options.Fields, query.Field{Name: "cost", Type: query.TypeFloat, SQL: "parts.cost", Write: authorization.IsAdmin()})
		}, want: `no JSON field "cost"`},
		{name: "cursor without a uuid id", change: func(options *livewire.ResourceOptions[part]) {
			options.Cursor = true
			options.Fields[0] = query.Field{Name: "id", Type: query.TypeInteger, SQL: "parts.number", Filter: true, Sort: true}
		}, want: `Cursor needs a field "id" of type uuid`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := partOptions()
			testCase.change(options)
			err := addError(t, livewire.NewResource("parts", "/parts", options))
			require.ErrorContains(t, err, testCase.want)
			require.ErrorContains(t, err, `resource "parts"`)
		})
	}

	names := func(tables []string, versioned bool) *livewire.Resource {
		return livewire.NewResource("names", "/names", &livewire.ResourceOptions[withoutID]{
			Tables:    tables,
			Versioned: versioned,
			Read:      authorization.IsUser(),
			Fields:    query.Fields{{Name: "name", Type: query.TypeString, SQL: "parts.name"}},
			List:      func(context.Context, *query.ListQuery) ([]withoutID, int, error) { return nil, 0, nil },
		})
	}
	t.Run("live list without id", func(t *testing.T) {
		require.ErrorContains(t, addError(t, names([]string{"parts"}, false)), `no JSON field "id"`)
	})
	t.Run("versioned without version", func(t *testing.T) {
		require.ErrorContains(t, addError(t, names(nil, true)), `no JSON field "version"`)
	})
	t.Run("item type without id is fine when not live", func(t *testing.T) {
		require.NoError(t, addError(t, names(nil, false)))
	})
	t.Run("live list of pointer items without id", func(t *testing.T) {
		err := addError(t, livewire.NewResource("names", "/names", &livewire.ResourceOptions[*withoutID]{
			Tables: []string{"parts"},
			Read:   authorization.IsUser(),
			Fields: query.Fields{{Name: "name", Type: query.TypeString, SQL: "parts.name"}},
			List:   func(context.Context, *query.ListQuery) ([]*withoutID, int, error) { return nil, 0, nil },
		}))
		require.ErrorContains(t, err, `no JSON field "id"`)
	})
	t.Run("nil options", func(t *testing.T) {
		require.ErrorContains(t, addError(t, livewire.NewResource[part]("parts", "/parts", nil)), "Check is required")
	})
	t.Run("name is not an identifier", func(t *testing.T) {
		for _, name := range []string{"", "parts list", "1parts", "parts-list"} {
			require.ErrorContains(t, addError(t, livewire.NewResource(name, "/parts", partOptions())), "is not an identifier")
		}
	})
	t.Run("placeholder in the path", func(t *testing.T) {
		require.ErrorContains(t, addError(t, livewire.NewResource("parts", "/tenants/{tenant}/parts", partOptions())), "no field tagged")
	})
}

func TestResourceCopiesOptions(t *testing.T) {
	options := partOptions()
	partition := func(context.Context, any) string { return "p1" }
	options.Partition = partition
	resource := newParts(t, options)

	options.List, options.Get, options.Distinct, options.Partition = nil, nil, nil, nil
	options.Fields[2].Enum[0] = "changed"
	options.Fields[0].Name = "changed"
	options.DefaultSort[0].Field = "changed"
	options.Item.Field = "changed"
	options.Tables[0] = "changed"

	items, _, err := resource.LoadList(userContext(), &query.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, []part{}, items)
	_, err = resource.LoadItem(userContext(), &query.ListQuery{})
	require.NoError(t, err)
	_, err = resource.LoadDistinct(userContext(), "type", &query.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, "p1", resource.Partition()(context.Background(), nil))
	require.Equal(t, "id", resource.Fields()[0].Name)
	require.Equal(t, []string{"bolt", "nut"}, resource.Fields()[2].Enum)
	require.Equal(t, "createdAt", resource.DefaultSort()[0].Field)
	require.Equal(t, "id", resource.Item().Field)
	require.Equal(t, []string{"parts"}, resource.Tables())

	fields := resource.Fields()
	fields[0].Name, fields[2].Enum[0] = "changed", "changed"
	require.Equal(t, "id", resource.Fields()[0].Name)
	require.Equal(t, "bolt", resource.Fields()[2].Enum[0], "Fields returns copies")
}

func TestResourceDerivesNullable(t *testing.T) {
	options := partOptions()
	resource := newParts(t, options)
	byName := map[string]query.Field{}
	for _, field := range resource.Fields() {
		byName[field.Name] = field
	}
	require.True(t, byName["price"].Nullable, "pointer field")
	require.True(t, byName["deletedAt"].Nullable, "pointer field")
	require.False(t, byName["createdAt"].Nullable, "value field")
	require.False(t, byName["name"].Nullable)
	require.False(t, options.Fields[3].Nullable, "the caller's fields are untouched")
}

func TestResourceAccessors(t *testing.T) {
	options := partOptions()
	options.Versioned = true
	options.Cursor = true
	options.ExportColumns = []string{"name", "type"}
	resource := newParts(t, options)
	require.Equal(t, livewire.KindResource, resource.Kind())
	require.Equal(t, "parts", resource.Name())
	require.Equal(t, http.MethodGet, resource.Method())
	require.Equal(t, "/parts", resource.Path())
	require.Equal(t, "/parts/{id}", resource.ItemPath())
	require.Equal(t, "/parts/distinct", resource.DistinctPath())
	require.True(t, resource.Versioned())
	require.True(t, resource.Cursor())
	require.Equal(t, []string{"name", "type"}, resource.ExportColumns())
	require.Equal(t, reflect.TypeFor[part](), resource.ItemType())
	require.Empty(t, resource.Parameters())
	require.NotNil(t, resource.Check())
	require.Nil(t, resource.Partition())
	decoded, err := resource.DecodeParameters(livewire.MapSource{})
	require.NoError(t, err)
	require.Equal(t, &livewire.NoParameters{}, decoded)

	plain := partOptions()
	plain.Item, plain.Get, plain.Distinct = nil, nil, nil
	bare := newParts(t, plain)
	require.Empty(t, bare.ItemPath())
	require.Empty(t, bare.DistinctPath())
	require.Nil(t, bare.Item())
	_, err = bare.ItemQuery(userContext(), livewire.MapSource{"id": "x"})
	requireError(t, err, http.StatusNotFound, livewire.CodeNotFound, "")
	_, err = bare.LoadItem(userContext(), &query.ListQuery{})
	requireError(t, err, http.StatusNotFound, livewire.CodeNotFound, "")
	_, err = bare.LoadDistinct(userContext(), "type", &query.ListQuery{})
	requireError(t, err, http.StatusNotFound, livewire.CodeNotFound, "")
}

func TestResourceListQuery(t *testing.T) {
	options := partOptions()
	options.MaximumLimit = 50
	options.CountCap = 1000
	options.Scope = func(context.Context) (query.Predicate, error) {
		return func(builder *query.Builder) { builder.WriteString("parts.tenant = ").Argument("t1") }, nil
	}
	resource := newParts(t, options)
	specification := &query.ResourceSQL{From: "parts", IDColumn: "parts.id"}

	t.Run("defaults", func(t *testing.T) {
		listQuery, err := resource.ListQuery(userContext(), &livewire.ReadParameters{})
		require.NoError(t, err)
		require.Equal(t, 1000, listQuery.CountCap)
		text, arguments := listQuery.PageSQL(specification)
		require.Equal(t, "SELECT parts.id, (parts.created_at) FROM parts WHERE parts.tenant = $1 ORDER BY (parts.created_at) DESC, parts.id ASC LIMIT $2 OFFSET $3", text)
		require.Equal(t, []any{"t1", 50, 0}, arguments)
	})
	t.Run("filter sort and page", func(t *testing.T) {
		listQuery, err := resource.ListQuery(userContext(), &livewire.ReadParameters{
			Filter: json.RawMessage(`{"where":{"field":"type","op":"eq","value":"bolt"},"search":"m8"}`),
			Sort:   "name", Limit: "10", Offset: "20",
		})
		require.NoError(t, err)
		text, arguments := listQuery.PageSQL(specification)
		require.Contains(t, text, "(parts.type) = $2")
		require.Contains(t, text, "(parts.name) ILIKE $3")
		require.NotContains(t, text, "parts.notes", "search skips the guarded searchable field")
		require.Contains(t, text, "ORDER BY (parts.name) ASC, parts.id ASC")
		require.Equal(t, []any{"t1", "bolt", "%m8%", 10, 20}, arguments)
	})
	t.Run("admin searches the guarded field too", func(t *testing.T) {
		listQuery, err := resource.ListQuery(adminContext(), &livewire.ReadParameters{Filter: json.RawMessage(`{"search":"m8"}`)})
		require.NoError(t, err)
		text, _ := listQuery.PageSQL(specification)
		require.Contains(t, text, "(parts.notes) ILIKE")
	})
	t.Run("limit is clamped", func(t *testing.T) {
		listQuery, err := resource.ListQuery(userContext(), &livewire.ReadParameters{Limit: "500"})
		require.NoError(t, err)
		require.Equal(t, 50, listQuery.Limit)
	})
	t.Run("admin filters and sorts the guarded field", func(t *testing.T) {
		_, err := resource.ListQuery(adminContext(), &livewire.ReadParameters{Filter: json.RawMessage(`{"where":{"field":"secret","op":"eq","value":"x"}}`), Sort: "secret"})
		require.NoError(t, err)
	})

	refusals := []struct {
		name       string
		parameters *livewire.ReadParameters
		status     int
		code       livewire.Code
		field      string
	}{
		{name: "unknown filter field is 400", parameters: &livewire.ReadParameters{Filter: json.RawMessage(`{"where":{"field":"nope","op":"eq","value":"x"}}`)}, status: http.StatusBadRequest, code: livewire.CodeInvalidFilter, field: "where.field"},
		{name: "guarded filter field is 403", parameters: &livewire.ReadParameters{Filter: json.RawMessage(`{"where":{"field":"secret","op":"eq","value":"x"}}`)}, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "guarded field nested in a visible condition is 403", parameters: &livewire.ReadParameters{Filter: json.RawMessage(`{"where":{"and":[{"field":"name","op":"eq","value":"x"},{"field":"secret","op":"isNull","value":true}]}}`)}, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "unknown and guarded in the filter: unknown wins", parameters: &livewire.ReadParameters{Filter: json.RawMessage(`{"where":{"and":[{"field":"secret","op":"eq","value":"x"},{"field":"nope","op":"eq","value":"x"}]}}`)}, status: http.StatusBadRequest, code: livewire.CodeInvalidFilter, field: "where.and[1].field"},
		{name: "guarded filter and unknown sort: unknown wins", parameters: &livewire.ReadParameters{Filter: json.RawMessage(`{"where":{"field":"secret","op":"eq","value":"x"}}`), Sort: "nope"}, status: http.StatusBadRequest, code: livewire.CodeInvalidSort, field: "sort[0]"},
		{name: "guarded sort and bad page: bad page wins", parameters: &livewire.ReadParameters{Sort: "secret", Limit: "-1"}, status: http.StatusBadRequest, code: livewire.CodeInvalidPage, field: "limit"},
		{name: "guarded sort and bad cursor: bad cursor wins", parameters: &livewire.ReadParameters{Sort: "secret", After: "x"}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "after"},
		{name: "malformed filter", parameters: &livewire.ReadParameters{Filter: json.RawMessage(`{`)}, status: http.StatusBadRequest, code: livewire.CodeInvalidFilter},
		{name: "unknown sort is 400", parameters: &livewire.ReadParameters{Sort: "nope"}, status: http.StatusBadRequest, code: livewire.CodeInvalidSort, field: "sort[0]"},
		{name: "guarded sort is 403", parameters: &livewire.ReadParameters{Sort: "-secret"}, status: http.StatusForbidden, code: livewire.CodeForbiddenField, field: "secret"},
		{name: "bad page", parameters: &livewire.ReadParameters{Limit: "-1"}, status: http.StatusBadRequest, code: livewire.CodeInvalidPage, field: "limit"},
		{name: "cursor without cursor mode", parameters: &livewire.ReadParameters{After: "abc"}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest, field: "after"},
	}
	for _, testCase := range refusals {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := resource.ListQuery(userContext(), testCase.parameters)
			requireError(t, err, testCase.status, testCase.code, testCase.field)
		})
	}
}

func TestResourceListQueryCursor(t *testing.T) {
	options := partOptions()
	options.Cursor = true
	resource := newParts(t, options)
	first, err := resource.ListQuery(userContext(), &livewire.ReadParameters{Sort: "name"})
	require.NoError(t, err)
	after, err := first.NextCursor([]any{"bolt"}, "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f").Encode()
	require.NoError(t, err)

	listQuery, err := resource.ListQuery(userContext(), &livewire.ReadParameters{Sort: "name", After: after})
	require.NoError(t, err)
	require.NotNil(t, listQuery.Cursor)

	_, err = resource.ListQuery(userContext(), &livewire.ReadParameters{Sort: "-name", After: after})
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidCursor, "after")
	_, err = resource.ListQuery(userContext(), &livewire.ReadParameters{Sort: "name", After: "not-a-cursor"})
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidCursor, "after")

	t.Run("default sort round trip", func(t *testing.T) {
		firstPage, err := resource.ListQuery(userContext(), &livewire.ReadParameters{})
		require.NoError(t, err)
		next, err := firstPage.NextCursor([]any{"2026-01-01T00:00:00Z"}, "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f").Encode()
		require.NoError(t, err)
		secondPage, err := resource.ListQuery(userContext(), &livewire.ReadParameters{After: next})
		require.NoError(t, err)
		require.NotNil(t, secondPage.Cursor)
	})
}

func TestResourceCursorMismatchIsRefusedBeforeGuardsAndScope(t *testing.T) {
	options := partOptions()
	options.Cursor = true
	scopeCalls := 0
	options.Scope = func(context.Context) (query.Predicate, error) {
		scopeCalls++
		return func(builder *query.Builder) { builder.WriteString("TRUE") }, nil
	}
	resource := newParts(t, options)
	underID, err := resource.ListQuery(adminContext(), &livewire.ReadParameters{Sort: "id"})
	require.NoError(t, err)
	require.Equal(t, 1, scopeCalls)
	after, err := underID.NextCursor([]any{"3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"}, "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f").Encode()
	require.NoError(t, err)

	_, err = resource.ListQuery(userContext(), &livewire.ReadParameters{Sort: "secret", After: after})
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidCursor, "after")
	require.Equal(t, 1, scopeCalls, "an invalid request never runs the scope")
}

func TestResourceListQueryScopeError(t *testing.T) {
	failing := partOptions()
	failing.Scope = func(context.Context) (query.Predicate, error) { return nil, errors.New("tenant unknown") }
	_, err := newParts(t, failing).ListQuery(userContext(), &livewire.ReadParameters{})
	require.ErrorContains(t, err, "tenant unknown")
}

func TestResourceDistinctQuery(t *testing.T) {
	resource := newParts(t, partOptions())
	listQuery, field, err := resource.DistinctQuery(userContext(), &livewire.ReadParameters{Field: "type"})
	require.NoError(t, err)
	require.Equal(t, "type", field.Name)
	require.Equal(t, livewire.DefaultDistinctMaximum, listQuery.Limit)

	listQuery, _, err = resource.DistinctQuery(userContext(), &livewire.ReadParameters{Field: "type", Limit: "5"})
	require.NoError(t, err)
	require.Equal(t, 5, listQuery.Limit)

	_, _, err = resource.DistinctQuery(adminContext(), &livewire.ReadParameters{Field: "secret"})
	require.NoError(t, err)

	refusals := []struct {
		name       string
		parameters *livewire.ReadParameters
		status     int
		code       livewire.Code
	}{
		{name: "missing", parameters: &livewire.ReadParameters{}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "unknown", parameters: &livewire.ReadParameters{Field: "nope"}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "array field", parameters: &livewire.ReadParameters{Field: "tags"}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "guarded", parameters: &livewire.ReadParameters{Field: "secret"}, status: http.StatusForbidden, code: livewire.CodeForbiddenField},
		{name: "guarded field and unknown filter: unknown wins", parameters: &livewire.ReadParameters{Field: "secret", Filter: json.RawMessage(`{"where":{"field":"nope","op":"eq","value":1}}`)}, status: http.StatusBadRequest, code: livewire.CodeInvalidFilter},
		{name: "guarded filter and unknown distinct field: unknown wins", parameters: &livewire.ReadParameters{Field: "nope", Filter: json.RawMessage(`{"where":{"field":"secret","op":"eq","value":"x"}}`)}, status: http.StatusBadRequest, code: livewire.CodeInvalidRequest},
		{name: "unknown sort", parameters: &livewire.ReadParameters{Field: "type", Sort: "nope"}, status: http.StatusBadRequest, code: livewire.CodeInvalidSort},
	}
	for _, testCase := range refusals {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := resource.DistinctQuery(userContext(), testCase.parameters)
			requireError(t, err, testCase.status, testCase.code, "")
		})
	}
}

func TestResourceItemQuery(t *testing.T) {
	resource := newParts(t, partOptions())
	listQuery, err := resource.ItemQuery(userContext(), livewire.MapSource{"id": "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"})
	require.NoError(t, err)
	text, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	require.Contains(t, text, "(parts.id) = $1")
	require.Equal(t, []any{"3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f", 1, 0}, arguments)

	_, err = resource.ItemQuery(userContext(), livewire.MapSource{})
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "id")
	_, err = resource.ItemQuery(userContext(), livewire.MapSource{"id": "not-a-uuid"})
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "id")
	require.Equal(t, "must be a uuid", fields["id"])

	type numbered struct {
		Number int    `json:"number"`
		Name   string `json:"name"`
	}
	byNumber := livewire.NewResource("numbered", "/numbered", &livewire.ResourceOptions[numbered]{
		Read:   authorization.IsUser(),
		Fields: query.Fields{{Name: "number", Type: query.TypeInteger, SQL: "n.number", Filter: true, Read: authorization.IsAdmin()}},
		Item:   &livewire.ItemKey{Parameter: "number", Field: "number"},
		List:   func(context.Context, *query.ListQuery) ([]numbered, int, error) { return nil, 0, nil },
		Get:    func(context.Context, *query.ListQuery) (*numbered, error) { return &numbered{}, nil },
	})
	require.NoError(t, addError(t, byNumber))
	listQuery, err = byNumber.ItemQuery(adminContext(), livewire.MapSource{"number": "42"})
	require.NoError(t, err)
	_, arguments = listQuery.PageSQL(&query.ResourceSQL{From: "n", IDColumn: "n.number"})
	require.Equal(t, int64(42), arguments[0])
	_, err = byNumber.ItemQuery(adminContext(), livewire.MapSource{"number": "x"})
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "number")
	_, err = byNumber.ItemQuery(userContext(), livewire.MapSource{"number": "42"})
	requireError(t, err, http.StatusForbidden, livewire.CodeForbiddenField, "number")
}

func TestResourceLoaders(t *testing.T) {
	resource := newParts(t, partOptions())
	items, total, err := resource.LoadList(userContext(), &query.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, 0, total)
	require.Equal(t, []part{}, items, "a nil list becomes an empty one")

	item, err := resource.LoadItem(userContext(), &query.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, &part{Name: "bolt"}, item)

	values, err := resource.LoadDistinct(userContext(), "type", &query.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, []livewire.DistinctValue{}, values)

	failing := partOptions()
	failing.Get = func(context.Context, *query.ListQuery) (*part, error) { return nil, nil } //nolint:nilnil // provokes the no-item-no-error failure
	failing.Distinct = func(context.Context, string, *query.ListQuery) ([]livewire.DistinctValue, error) {
		return nil, livewire.NotFound()
	}
	broken := newParts(t, failing)
	_, err = broken.LoadItem(userContext(), &query.ListQuery{})
	require.ErrorContains(t, err, "no item and no error")
	_, err = broken.LoadDistinct(userContext(), "type", &query.ListQuery{})
	requireError(t, err, http.StatusNotFound, livewire.CodeNotFound, "")
}
