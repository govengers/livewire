package livewire

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

type inner struct {
	ID    *string `json:"id"`
	Depth int     `json:"depth"`
	Plain string
}

type outerPointerWins struct {
	ID string `json:"id"` // depth 0 beats inner's pointer id at depth 1
	inner
}

type optionOnlyTag struct {
	inner `json:",omitempty"` // an option-only tag does not name the field: inner flattens
	Name  string              `json:"name"`
}

type namedEmbedding struct {
	inner `json:"inner"` // a named tag makes the struct one field
}

type ambiguousLeft struct {
	Value string
}

type ambiguousRight struct {
	Value string
}

type ambiguous struct {
	ambiguousLeft
	ambiguousRight
	Keep string `json:"keep"`
}

type taggedWinner struct {
	Value string `json:"Value"`
}

type untaggedLoser struct {
	Value string
}

type taggedBeatsUntagged struct {
	taggedWinner
	untaggedLoser
}

type recursive struct {
	*recursive        //nolint:unused // the recursive embedding itself is under test
	ID         string `json:"id"`
}

type mutualA struct {
	*mutualB
	A string `json:"a"`
}

type mutualB struct {
	*mutualA
	B string `json:"b"`
}

type skipped struct {
	Hidden   string `json:"-"`
	Exported string
	hidden   string //nolint:unused // encoding/json skips unexported fields; so must we
}

func TestJSONFieldsOfMatchesEncodingJSON(t *testing.T) {
	testCases := []struct {
		name  string
		value any
	}{
		{name: "outer pointer wins over the deeper field", value: outerPointerWins{ID: "x", inner: inner{ID: new("y")}}},
		{name: "option-only tag flattens", value: optionOnlyTag{inner: inner{ID: new("y")}}},
		{name: "named embedding is one field", value: namedEmbedding{}},
		{name: "same-depth conflict is dropped", value: ambiguous{Keep: "k"}},
		{name: "a tagged field beats an untagged one at the same depth", value: taggedBeatsUntagged{}},
		{name: "recursive embedding ends", value: recursive{ID: "r"}},
		{name: "mutually recursive embedding ends", value: mutualA{A: "a", mutualB: &mutualB{B: "b"}}},
		{name: "skipped and unexported fields", value: skipped{Exported: "e"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			marshaled, err := json.Marshal(testCase.value)
			require.NoError(t, err)
			var object map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(marshaled, &object))
			require.ElementsMatch(t, slices.Collect(keysOf(object)), slices.Collect(keysOf(jsonFieldsOf(reflect.TypeOf(testCase.value)))), "%s", marshaled)
		})
	}
}

func TestJSONFieldsOfReportsTheDominantField(t *testing.T) {
	fields := jsonFieldsOf(reflect.TypeFor[*outerPointerWins]())
	require.Equal(t, reflect.TypeFor[string](), fields["id"].Type, "the shallow value field, not the embedded pointer")
	require.Equal(t, reflect.TypeFor[int](), fields["depth"].Type)

	fields = jsonFieldsOf(reflect.TypeFor[taggedBeatsUntagged]())
	require.Equal(t, `json:"Value"`, string(fields["Value"].Tag))

	require.Empty(t, jsonFieldsOf(reflect.TypeFor[string]()))
}

func keysOf[Value any](values map[string]Value) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for key := range values {
			if !yield(key) {
				return
			}
		}
	}
}
