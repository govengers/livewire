package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
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

// boxed has its own unmarshaler, whose type errors carry offsets into its own value.
type boxed struct {
	Inner struct {
		K string `json:"k"`
		N int    `json:"n"`
	}
}

func (b *boxed) UnmarshalJSON(raw []byte) error { return json.Unmarshal(raw, &b.Inner) }

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
	type behindRequired struct {
		Count int `json:"count" validate:"required,min=abc"`
	}
	type afterAFailingRule struct {
		Email string `json:"email" validate:"email,min=abc"`
	}
	type behindAPassingAlternative struct {
		Count int `json:"count" validate:"max=3|min=abc"`
	}
	type crossFieldBehindOmitempty struct {
		Kind  string `json:"kind" validate:"omitempty,required_if=Count abc"`
		Count int    `json:"count"`
	}
	type diveTheZeroValueNeverReaches struct {
		Name string `json:"name" validate:"required,dive"`
	}
	type behindDiveOnElements struct {
		Counts []*int `json:"counts" validate:"omitempty,dive,required,min=abc"`
	}
	type behindDiveOnMapKeys struct {
		Counts map[string]int `json:"counts" validate:"omitempty,dive,keys,required,max=abc,endkeys,required"`
	}
	type behindDiveOnMapValues struct {
		Counts map[string]int `json:"counts" validate:"omitempty,dive,keys,required,endkeys,required,min=abc"`
	}
	type behindDiveOnKeysToTheEnd struct {
		Counts map[string]int `json:"counts" validate:"omitempty,dive,keys,max=abc"`
	}
	type behindAPointer struct {
		Count **int `json:"count" validate:"omitempty,min=abc"`
	}
	type crossFieldOnAPointer struct {
		Kind  string `json:"kind" validate:"required_if=Count abc"`
		Count *int   `json:"count"`
	}
	type uniqueOnAMissingField struct {
		Lines []partLine `json:"lines" validate:"omitempty,unique=Missing"`
	}
	type uniqueOverUnhashableMapValues struct {
		Tags map[string][]string `json:"tags" validate:"omitempty,unique"`
	}
	type crossFieldOnAPromotedField struct {
		counted
		Kind string `json:"kind" validate:"omitempty,required_if=Count abc"`
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
		"typo":                          func() error { return declareBody[typo](t) },
		"bad argument":                  func() error { return declareBody[badlyTagged](t) },
		"misplaced dive":                func() error { return declareBody[misplacedDive](t) },
		"behind a pointer":              func() error { return declareBody[behindPointer](t) },
		"behind a slice":                func() error { return declareBody[behindSlice](t) },
		"behind a map":                  func() error { return declareBody[behindMap](t) },
		"own unmarshaler by pointer":    func() error { return declareBody[moneyPointer](t) },
		"own unmarshaler in a slice":    func() error { return declareBody[moneySlice](t) },
		"own unmarshaler in a map":      func() error { return declareBody[moneyMap](t) },
		"own unmarshaler at the root":   func() error { return declareBody[money](t) },
		"behind omitempty":              func() error { return declareBody[behindOmitempty](t) },
		"behind required":               func() error { return declareBody[behindRequired](t) },
		"after a failing rule":          func() error { return declareBody[afterAFailingRule](t) },
		"behind a passing alternative":  func() error { return declareBody[behindAPassingAlternative](t) },
		"cross-field behind omitempty":  func() error { return declareBody[crossFieldBehindOmitempty](t) },
		"dive the zero never reaches":   func() error { return declareBody[diveTheZeroValueNeverReaches](t) },
		"behind a dive on elements":     func() error { return declareBody[behindDiveOnElements](t) },
		"behind a dive on map keys":     func() error { return declareBody[behindDiveOnMapKeys](t) },
		"behind a dive on map values":   func() error { return declareBody[behindDiveOnMapValues](t) },
		"keys to the end of the tag":    func() error { return declareBody[behindDiveOnKeysToTheEnd](t) },
		"behind a pointer to a pointer": func() error { return declareBody[behindAPointer](t) },
		"cross-field on a pointer":      func() error { return declareBody[crossFieldOnAPointer](t) },
		"unique on a missing field":     func() error { return declareBody[uniqueOnAMissingField](t) },
		"unique over unhashable values": func() error { return declareBody[uniqueOverUnhashableMapValues](t) },
		"cross-field on a promoted one": func() error { return declareBody[crossFieldOnAPromotedField](t) },
	} {
		require.ErrorContains(t, declare(), "validate tags", name)
	}
}

// counted is embedded to promote Count.
type counted struct {
	Count int `json:"count"`
}

func TestMutationAcceptsUsableValidateTags(t *testing.T) {
	type usable struct {
		Kind    string            `json:"kind" validate:"omitempty,oneof=a b"`
		Count   *int              `json:"count" validate:"required_if=Kind a,omitempty,min=1|eq=0"`
		Other   int               `json:"other" validate:"omitempty,eqfield=Kind,gtfield=Count"`
		ByName  map[string]*int   `json:"byName" validate:"omitempty,dive,keys,min=1,max=8,endkeys,required,min=2"`
		Lines   []partLine        `json:"lines" validate:"omitempty,unique=SKU,dive"`
		Grid    [][]int           `json:"grid" validate:"dive,dive,min=0"`
		Created time.Time         `json:"created" validate:"omitempty,lt"`
		Labels  map[string]string `json:"labels" validate:"dive,keys,alphanum,endkeys"`
	}
	require.NoError(t, declareBody[usable](t))
}

func TestMutationDiveOnAnInterfaceRunsAtRunTime(t *testing.T) {
	type listed struct {
		Items any `json:"items" validate:"omitempty,dive,min=1"`
	}
	mutation := newBody[listed](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"items":[""]}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "items[0]")
	require.Equal(t, map[string]string{"items[0]": "min"}, fields, "the elements of an interface value are named by the decoded value")
	_, err = mutation.DecodeBody(userContext(), []byte(`{"items":["a"]}`))
	require.NoError(t, err)
}

func TestMutationAnswersARunTimeValidatorFaultAsInternal(t *testing.T) {
	type behindAnInterface struct {
		Value any `json:"value" validate:"omitempty,min=abc"`
	}
	mutation := newBody[behindAnInterface](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"value":3}`))
	require.ErrorContains(t, err, "failed at run time", "a rule on an interface value shows its fault only at run time: an error, not a panic")
	requireError(t, err, http.StatusInternalServerError, livewire.CodeInternal, "")
}

type tree map[string]tree

type chain []chain

// withinDeadline runs work and fails the test when it has not returned after five seconds,
// so an endless loop fails the test instead of hanging the suite. work must not call require.
func withinDeadline(t *testing.T, work func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not return within five seconds")
	}
}

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

func TestMutationValidationPathsEndOnRecursiveCollections(t *testing.T) {
	type grove struct {
		Tree  tree  `json:"tree" validate:"required,dive,min=1"`
		Chain chain `json:"chain" validate:"required,dive,min=1"`
	}
	mutation := newBody[grove](t)
	var omitted, empty error
	withinDeadline(t, func() {
		_, omitted = mutation.DecodeBody(userContext(), []byte(`{}`))
		_, empty = mutation.DecodeBody(userContext(), []byte(`{"tree":{"a":{}},"chain":[[[]],[]]}`))
	})
	fields := requireError(t, omitted, http.StatusBadRequest, livewire.CodeInvalidRequest, "tree")
	require.Equal(t, map[string]string{"tree": "required", "chain": "required"}, fields, "an omitted recursive collection names its field")
	fields = requireError(t, empty, http.StatusBadRequest, livewire.CodeInvalidRequest, "tree[a]")
	require.Equal(t, map[string]string{"tree[a]": "min", "chain[1]": "min"}, fields, "each key consumes one level of the recursive type")
}

type pointerLoop *pointerLoop

func TestDeclarationsRefusePointerLoops(t *testing.T) {
	type looped struct {
		Loop pointerLoop `json:"loop"`
	}
	type loopKeyed struct {
		Names map[pointerLoop]string `json:"names" validate:"dive,keys,required,endkeys"`
	}
	declarations := map[string]livewire.Declaration{}
	withinDeadline(t, func() {
		declarations["mutation map key"] = livewire.NewMutation("loopKeys", http.MethodPost, "/loop-keys", &livewire.MutationOptions[livewire.NoParameters, loopKeyed, livewire.NoContent]{
			Check: authorization.IsUser(),
			Handle: func(context.Context, *livewire.NoParameters, *loopKeyed) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		})
		declarations["resource item"] = livewire.NewResource("loops", "/loops", &livewire.ResourceOptions[pointerLoop]{
			Read:   authorization.IsUser(),
			Fields: query.Fields{{Name: "id", Type: query.TypeUUID, SQL: "loops.id", Filter: true}},
			List:   func(context.Context, *query.ListQuery) ([]pointerLoop, int, error) { return nil, 0, nil },
		})
		declarations["list item field"] = livewire.NewList("loopList", "/loop-list", &livewire.ListOptions[looped, livewire.NoParameters]{
			Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) ([]looped, error) { return nil, nil },
		})
		declarations["object item field"] = livewire.NewObject("loopObject", "/loop-object", &livewire.ObjectOptions[looped, livewire.NoParameters]{
			Read: authorization.IsUser(),
			Load: func(context.Context, *livewire.NoParameters) (*looped, error) { return &looped{}, nil },
		})
		declarations["mutation field"] = livewire.NewMutation("loopMutation", http.MethodPost, "/loop-mutation", &livewire.MutationOptions[livewire.NoParameters, looped, livewire.NoContent]{
			Check: authorization.IsUser(),
			Handle: func(context.Context, *livewire.NoParameters, *looped) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		})
	})
	for name, declaration := range declarations {
		require.ErrorContains(t, addError(t, declaration), "points to itself", name)
	}
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

	for _, key := range []string{"a]b.c", "a[", "a]", "a].b[", "a]..x["} {
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

	type unmarshaled struct {
		Name     string `json:"name"`
		Internal string `json:"-" validate:"required"`
	}
	unmarshaledMutation := newBody[unmarshaled](t)
	_, err = unmarshaledMutation.DecodeBody(userContext(), []byte(`{"name":"a"}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
	require.Equal(t, map[string]string{"body": "required"}, fields, "a field the body cannot carry names the body, never its Go name")
}

type branch struct {
	Note string            `json:"note" validate:"required"`
	Sub  map[string]branch `json:"sub" validate:"dive"`
}

func TestMutationValidationPathsThroughARecursiveMap(t *testing.T) {
	mutation := newBody[branch](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"note":"n","sub":{"k":{"note":"m","sub":{"j":{}}}}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "sub[k].sub[j].note")
	require.Equal(t, map[string]string{"sub[k].sub[j].note": "required"}, fields)

	for _, key := range []string{"x]y.Sub[z", "k].Note[x", "k].Nope[x", "k]Sub[j", "k].Sub[j", "k].Sub[j].Sub[i"} {
		_, err = mutation.DecodeBody(userContext(), []byte(`{"note":"n","sub":{"`+key+`":{}}}`))
		fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
		require.Equal(t, map[string]string{"body": "required"}, fields, "a key %q that forges a path names the body", key)
	}

	forged := `{"note":"n","sub":{"k":{"note":"m","sub":{"j":{"note":"o","sub":{"i":{"note":"p"}}}}},"k].Sub[j":{},"k].Sub[j].Sub[i":{}}}`
	_, err = mutation.DecodeBody(userContext(), []byte(forged))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
	require.Equal(t, map[string]string{"body": "required"}, fields, "keys that forge the path of existing entries name the body")
}

// caseless keys format in lower case, so two keys can share one text.
type caseless string

func (c caseless) String() string { return strings.ToLower(string(c)) }

func TestMutationValidationPathsOfKeysWithBrackets(t *testing.T) {
	type keyed struct {
		Values map[string]string            `json:"values" validate:"dive,required"`
		Nested map[string]map[string]string `json:"nested" validate:"dive,dive,min=1"`
	}
	mutation := newBody[keyed](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"values":{"a[":"set","a[]x":""}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "values[a[]x]")
	require.Equal(t, map[string]string{"values[a[]x]": "required"}, fields, "a key text that does not end at a segment is no candidate")

	_, err = mutation.DecodeBody(userContext(), []byte(`{"values":{"a[":"ok","a[][b]":""}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "values[a[][b]]")
	require.Equal(t, map[string]string{"values[a[][b]]": "required"}, fields, "a key whose rest cannot resolve is no reading")

	_, err = mutation.DecodeBody(userContext(), []byte(`{"nested":{"a[":{"b]":""},"a[][b]":{}}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
	require.Equal(t, map[string]string{"body": "min"}, fields, "a namespace two entries explain names neither")
}

func TestMutationValidationPathsOfKeysSharingOneText(t *testing.T) {
	type labels struct {
		Labels map[caseless]string `json:"labels" validate:"dive,min=2"`
	}
	mutation := newBody[labels](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"labels":{"A":"x","a":"y"}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "body")
	require.Equal(t, map[string]string{"body": "min"}, fields, "a text two keys share names neither")
	_, err = mutation.DecodeBody(userContext(), []byte(`{"labels":{"A[":"xx","a[":"xx","a[]":"x"}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "labels[a[]]")
	require.Equal(t, map[string]string{"labels[a[]]": "min"}, fields, "keys sharing a text are no reading when their rest does not resolve")
	_, err = mutation.DecodeBody(userContext(), []byte(`{"labels":{"B":"x"}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "labels[B]")
	require.Equal(t, map[string]string{"labels[B]": "min"}, fields, "the key as the body carries it")
}

type labelled struct {
	Name string `json:"label" validate:"max=1"`
}

// relabelled promotes labelled.Name under "label" beside its own Name under "name".
type relabelled struct {
	labelled
	Name string `json:"name" validate:"required"`
}

func TestMutationValidationPathsTellFieldsWithOneGoNameApart(t *testing.T) {
	mutation := newBody[relabelled](t)
	for range 20 {
		_, err := mutation.DecodeBody(userContext(), []byte(`{"label":"long"}`))
		fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "name")
		require.Equal(t, map[string]string{"name": "required", "label": "max"}, fields)
	}
}

func TestMutationPathsOfABracketedFieldName(t *testing.T) {
	type bracketed struct {
		Nested struct {
			Name string `json:"[name]" validate:"max=1"`
		} `json:"nested"`
	}
	mutation := newBody[bracketed](t)
	_, err := mutation.DecodeBody(userContext(), []byte(`{"nested":{"[name]":"long"}}`))
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.[name]")
	require.Equal(t, "max", fields["nested.[name]"])
	_, err = mutation.DecodeBody(userContext(), []byte(`{"nested":{"[name]":"a","[name]":"b"}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.[name]")
	require.Equal(t, "duplicate member", fields["nested.[name]"], "a field segment is written as a field, whatever its name looks like")
	_, err = mutation.DecodeBody(userContext(), []byte(`{"nested":{"[name]":1}}`))
	fields = requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "nested.[name]")
	require.Equal(t, "has the wrong type", fields["nested.[name]"])
}

type withMaps struct {
	Name   string            `json:"name"`
	Nested nested            `json:"nested"`
	ByID   map[int]*nested   `json:"byId" validate:"dive"`
	ByName map[string]nested `json:"byName"`
	Lines  []nested          `json:"lines"`
	Any    any               `json:"any"`
	Raw    json.RawMessage   `json:"raw"`
	Loose  *lenient          `json:"loose"`
	Cents  *boxed            `json:"cents"`
	Number json.Number       `json:"number"`
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
		{name: "duplicate inside the second raw element", raw: `{"raw":[{"x":1},{"x":1,"x":2}]}`, field: "raw[1].x", text: "duplicate member"},
		{name: "wrong type of a map member", raw: `{"byName":{"a.b":{"note":1}}}`, field: "byName[a.b].note", text: "has the wrong type"},
		{name: "unknown field of the same map member", raw: `{"byName":{"a.b":{"nope":1}}}`, field: "byName[a.b].nope", text: "unknown field"},
		{name: "wrong type of an array element", raw: `{"lines":[{},{"note":1}]}`, field: "lines[1].note", text: "has the wrong type"},
		{name: "unknown field of the same array element", raw: `{"lines":[{},{"nope":1}]}`, field: "lines[1].nope", text: "unknown field"},
		{name: "wrong type of a whole value", raw: `{"name":"a","nested":[1]}`, field: "nested", text: "has the wrong type"},
		{name: "map key of the wrong type names the entry", raw: `{"byId":{"x":{}}}`, field: "byId[x]", text: "has the wrong type"},
		{name: "wrong type of the first of two elements", raw: `{"lines":[{"note":1},{}]}`, field: "lines[0].note", text: "has the wrong type"},
		{name: "wrong type inside an own unmarshaler names the body", raw: `{"name":"abcdefghijklmnopqrst","cents":{"k":"v","n":"x"}}`, field: "body", text: "has the wrong type"},
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

// deepBodyAllocation is what decoding a body of depth nested objects allocates.
func deepBodyAllocation(t *testing.T, mutation *livewire.Mutation, depth int) uint64 {
	t.Helper()
	body := []byte(strings.Repeat(`{"child":`, depth) + `{"leaf":1}` + strings.Repeat(`}`, depth))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := mutation.DecodeBody(userContext(), body)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	return after.TotalAlloc - before.TotalAlloc
}

func TestMutationDeepBodyAllocatesInProportion(t *testing.T) {
	mutation := newBody[deep](t)
	const depth = 5000
	half, full := deepBodyAllocation(t, mutation, depth/2), deepBodyAllocation(t, mutation, depth)
	// The absolute multiple depends on the toolchain's encoding/json; the growth does not: in
	// proportion to the depth it about doubles, a path written eagerly on every push grows
	// with the depth squared and quadruples.
	require.Less(t, float64(full), 2.5*float64(half), "twice the depth allocates about twice as much (%d bytes, then %d)", half, full)

	unknown := strings.Repeat(`{"child":`, depth) + `{"nope":1}` + strings.Repeat(`}`, depth)
	_, err := mutation.DecodeBody(userContext(), []byte(unknown))
	requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, strings.Repeat("child.", depth)+"nope")
}
