package livewire_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

type status string

type paging struct {
	Limit  int   `query:"limit"`
	Offset *int8 `query:"offset"`
}

type everyParameter struct {
	paging
	ID        uuid.UUID  `path:"id"`
	Name      string     `path:"name"`
	Status    status     `query:"status" enum:"open,closed"`
	Statuses  []status   `query:"statuses" enum:"open,closed"`
	Tags      []string   `query:"tags"`
	Active    bool       `query:"active"`
	Owner     *uuid.UUID `query:"owner"`
	Since     time.Time  `query:"since"`
	Until     *time.Time `query:"until"`
	Note      *string    `query:"note"`
	Count     int64      `query:"count"`
	Ignored   string
	unexposed string //nolint:unused // the decoder must skip an untagged unexported field
}

func stats[Params any](t *testing.T, path string) *livewire.List {
	t.Helper()
	list := livewire.NewList("stats", path, &livewire.ListOptions[part, Params]{
		Read: authorization.IsUser(),
		Load: func(context.Context, *Params) ([]part, error) { return nil, nil },
	})
	return list
}

func TestDecodeParametersEveryKind(t *testing.T) {
	list := stats[everyParameter](t, "/stats/{id}/{name}")
	require.NoError(t, addError(t, list))
	id := uuid.MustParse("0192f1a0-1b2c-7d3e-8f40-516273849506")
	owner := uuid.MustParse("3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f")
	since := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

	source := querySource{
		path: map[string]string{"id": id.String(), "name": "bolt"},
		query: map[string][]string{
			"limit": {"20"}, "offset": {"5"}, "status": {"open"}, "statuses": {"open,closed", "open"}, "tags": {"a,b", "", "c"},
			"active": {"true"}, "owner": {owner.String()}, "since": {"2026-09-23T10:00:00Z"}, "until": {"2026-09-23T12:00:00+02:00"},
			"note": {"hi"}, "count": {"9000000000"}, "ignored": {"x"},
		},
	}
	decoded, err := list.DecodeParameters(source)
	require.NoError(t, err)
	parameters, ok := decoded.(*everyParameter)
	require.True(t, ok)
	require.Equal(t, &everyParameter{
		paging:   paging{Limit: 20, Offset: new(int8(5))},
		ID:       id,
		Name:     "bolt",
		Status:   "open",
		Statuses: []status{"open", "closed", "open"},
		Tags:     []string{"a", "b", "c"},
		Active:   true,
		Owner:    &owner,
		Since:    since,
		Until:    new(since),
		Note:     new("hi"),
		Count:    9000000000,
	}, parameters)
	require.True(t, parameters.Until.Equal(since))
}

func TestDecodeParametersAbsentAndEmpty(t *testing.T) {
	list := stats[everyParameter](t, "/stats/{id}/{name}")
	require.NoError(t, addError(t, list))
	decoded, err := list.DecodeParameters(livewire.MapSource{"id": uuid.Nil.String(), "name": "x", "note": "", "tags": ""})
	require.NoError(t, err)
	parameters, ok := decoded.(*everyParameter)
	require.True(t, ok)
	require.Nil(t, parameters.Note)
	require.Nil(t, parameters.Tags)
	require.Nil(t, parameters.Owner)
	require.Zero(t, parameters.Limit)
}

func TestDecodeParametersRefusals(t *testing.T) {
	list := stats[everyParameter](t, "/stats/{id}/{name}")
	require.NoError(t, addError(t, list))
	valid := map[string]string{"id": uuid.Nil.String(), "name": "x"}

	testCases := []struct {
		name      string
		values    map[string]string
		wantField string
		wantText  string
	}{
		{name: "missing path parameter", values: map[string]string{"id": uuid.Nil.String()}, wantField: "name", wantText: "required"},
		{name: "empty path parameter", values: map[string]string{"id": uuid.Nil.String(), "name": ""}, wantField: "name", wantText: "required"},
		{name: "bad uuid", values: map[string]string{"id": "nope", "name": "x"}, wantField: "id", wantText: "must be a uuid"},
		{name: "bad pointer uuid", values: with(valid, "owner", "nope"), wantField: "owner", wantText: "must be a uuid"},
		{name: "bad time", values: with(valid, "since", "yesterday"), wantField: "since", wantText: "must be an RFC 3339 time"},
		{name: "bad bool", values: with(valid, "active", "yes"), wantField: "active", wantText: "must be a boolean"},
		{name: "bad int", values: with(valid, "limit", "ten"), wantField: "limit", wantText: "must be an integer"},
		{name: "int overflow", values: with(valid, "offset", "300"), wantField: "offset", wantText: "must be an integer"},
		{name: "enum", values: with(valid, "status", "late"), wantField: "status", wantText: "must be one of open, closed"},
		{name: "enum in list", values: with(valid, "statuses", "open,late"), wantField: "statuses", wantText: "must be one of open, closed"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := list.DecodeParameters(livewire.MapSource(testCase.values))
			fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, testCase.wantField)
			require.Equal(t, testCase.wantText, fields[testCase.wantField])
		})
	}
}

func TestParametersDescribe(t *testing.T) {
	list := stats[everyParameter](t, "/stats/{id}/{name}")
	require.NoError(t, addError(t, list))
	parameters := list.Parameters()
	byName := map[string]livewire.Parameter{}
	for _, parameter := range parameters {
		byName[parameter.Name] = parameter
	}
	require.Len(t, parameters, 13)
	require.Equal(t, livewire.Parameter{Name: "id", In: livewire.InPath, Type: reflect.TypeFor[uuid.UUID]()}, byName["id"])
	require.True(t, byName["id"].Required())
	require.Equal(t, livewire.Parameter{Name: "statuses", In: livewire.InQuery, Type: reflect.TypeFor[[]status](), Enum: []string{"open", "closed"}, List: true}, byName["statuses"])
	require.False(t, byName["statuses"].Required())
	require.Equal(t, "limit", parameters[0].Name, "embedded fields come first, in declaration order")

	byName["statuses"].Enum[0] = "changed"
	require.Equal(t, "open", list.Parameters()[5].Enum[0], "Parameters returns copies")
}

func TestParameterDeclarationErrors(t *testing.T) {
	type unexported struct {
		//nolint:unused // the declaration must refuse a tagged unexported field
		secret string `query:"secret"`
	}
	type bothTags struct {
		ID string `path:"id" query:"id"`
	}
	type unsupported struct {
		Weight float64 `query:"weight"`
	}
	type intList struct {
		IDs []int `query:"ids"`
	}
	type pathList struct {
		Names []string `path:"names"`
	}
	type enumOnInt struct {
		Limit int `query:"limit" enum:"1,2"`
	}
	type duplicate struct {
		First  string `query:"name"`
		Second string `path:"name"`
	}
	type withID struct {
		ID string `path:"id"`
	}

	testCases := []struct {
		name string
		list *livewire.List
		want string
	}{
		{name: "not a struct", list: stats[string](t, "/stats"), want: "is not a struct"},
		{name: "unexported", list: stats[unexported](t, "/stats"), want: "unexported field"},
		{name: "path and query", list: stats[bothTags](t, "/stats/{id}"), want: "tagged path and query"},
		{name: "unsupported type", list: stats[unsupported](t, "/stats"), want: "unsupported type float64"},
		{name: "list of ints", list: stats[intList](t, "/stats"), want: "list parameter"},
		{name: "list in the path", list: stats[pathList](t, "/stats/{names}"), want: "list parameter"},
		{name: "enum on int", list: stats[enumOnInt](t, "/stats"), want: "enum values"},
		{name: "declared twice", list: stats[duplicate](t, "/stats/{name}"), want: "declared twice"},
		{name: "placeholder without field", list: stats[livewire.NoParameters](t, "/stats/{id}"), want: "no field tagged path:\"id\""},
		{name: "field without placeholder", list: stats[withID](t, "/stats"), want: "is not in the path"},
		{name: "no leading slash", list: stats[livewire.NoParameters](t, "stats"), want: "does not start with a slash"},
		{name: "empty segment", list: stats[livewire.NoParameters](t, "/stats//all"), want: "empty segment"},
		{name: "trailing slash", list: stats[livewire.NoParameters](t, "/stats/"), want: "empty segment"},
		{name: "malformed placeholder", list: stats[withID](t, "/stats/{id"), want: "malformed placeholder"},
		{name: "placeholder in a segment", list: stats[withID](t, "/stats/x{id}"), want: "malformed placeholder"},
		{name: "empty placeholder", list: stats[withID](t, "/stats/{}"), want: "malformed placeholder"},
		{name: "repeated placeholder", list: stats[withID](t, "/stats/{id}/{id}"), want: "repeated placeholder"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := addError(t, testCase.list)
			require.ErrorContains(t, err, testCase.want)
			require.ErrorContains(t, err, `list "stats"`)
		})
	}
}

func TestReadParametersFrom(t *testing.T) {
	parameters := livewire.ReadParametersFrom(livewire.MapSource{
		"filter": `{"search":"bolt"}`, "filterId": "f1", "sort": "-name", "limit": "10", "offset": "20", "after": "cursor", "field": "type",
	})
	require.Equal(t, &livewire.ReadParameters{
		Filter: []byte(`{"search":"bolt"}`), FilterID: "f1", Sort: "-name", Limit: "10", Offset: "20", After: "cursor", Field: "type",
	}, parameters)
	require.Equal(t, &livewire.ReadParameters{}, livewire.ReadParametersFrom(livewire.MapSource{}))
	require.Nil(t, livewire.ReadParametersFrom(livewire.MapSource{"filter": ""}).Filter)
}

// querySource is a ParameterSource with repeated query keys, as a REST adapter has them.
type querySource struct {
	path  map[string]string
	query map[string][]string
}

func (s querySource) Path(name string) (string, bool) {
	value, ok := s.path[name]
	return value, ok
}

func (s querySource) Query(name string) ([]string, bool) {
	values, ok := s.query[name]
	return values, ok
}

func with(values map[string]string, name, value string) map[string]string {
	extended := make(map[string]string, len(values)+1)
	for key, existing := range values {
		extended[key] = existing
	}
	extended[name] = value
	return extended
}
