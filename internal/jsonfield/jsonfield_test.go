package jsonfield

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
	Left string `json:"a"`
}

type mutualB struct {
	*mutualA
	Right string `json:"b"`
}

type diamondBase struct {
	Shared string `json:"shared"`
}

type diamondLeft struct {
	diamondBase
	Left string `json:"left"`
}

type diamondRight struct {
	diamondBase
	Right string `json:"right"`
}

// diamond reaches diamondBase twice on level 2: encoding/json cancels its fields.
type diamond struct {
	diamondLeft
	diamondRight
}

// shallowDiamond reaches diamondBase on level 1 and again on level 2: the shallow one wins.
type shallowDiamond struct {
	diamondLeft
	diamondBase
}

type skippedFields struct {
	Hidden   string `json:"-"`
	Exported string
	hidden   string //nolint:unused // encoding/json skips unexported fields; so must we
}

func TestOfMatchesEncodingJSON(t *testing.T) {
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
		{name: "mutually recursive embedding ends", value: mutualA{Left: "a", mutualB: &mutualB{Right: "b"}}},
		{name: "skipped and unexported fields", value: skippedFields{Exported: "e"}},
		{name: "diamond cancels the shared field", value: diamond{diamondLeft: diamondLeft{Left: "l"}, diamondRight: diamondRight{Right: "r"}}},
		{name: "shallow occurrence wins the diamond", value: shallowDiamond{diamondBase: diamondBase{Shared: "s"}}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			marshaled, err := json.Marshal(testCase.value)
			require.NoError(t, err)
			var object map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(marshaled, &object))
			require.ElementsMatch(t, slices.Collect(keysOf(object)), slices.Collect(keysOf(ByName(reflect.TypeOf(testCase.value)))), "%s", marshaled)
		})
	}
}

func TestByNameReportsTheDominantField(t *testing.T) {
	fields := ByName(reflect.TypeFor[outerPointerWins]())
	require.Equal(t, reflect.TypeFor[string](), fields["id"].Type, "the shallow value field, not the embedded pointer")
	require.Equal(t, reflect.TypeFor[int](), fields["depth"].Type)

	fields = ByName(reflect.TypeFor[taggedBeatsUntagged]())
	require.Equal(t, `json:"Value"`, string(fields["Value"].Tag))

	require.Empty(t, ByName(reflect.TypeFor[string]()))
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

type options struct {
	Plain    string  `json:"plain"`
	Empty    string  `json:"empty,omitempty"`
	Zero     int     `json:"zero,omitzero"`
	Count    int64   `json:"count,string"`
	Pointer  *bool   `json:"pointer,string"`
	Ignored  []int   `json:"ignored,string"` //nolint:staticcheck // SA5008: the string option does not apply to a slice, under test
	Both     *string `json:"both,omitempty,omitzero"`
	Untagged int
}

func TestOfReadsTheOptionsInEncodingJSONOrder(t *testing.T) {
	fields := Of(reflect.TypeFor[options]())
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	require.Equal(t, []string{"plain", "empty", "zero", "count", "pointer", "ignored", "both", "Untagged"}, names)
	require.Equal(t, Field{Name: "empty", StructField: fields[1].StructField, OmitEmpty: true}, fields[1])
	require.True(t, fields[2].OmitZero)
	require.True(t, fields[3].Quoted)
	require.True(t, fields[4].Quoted, "behind one pointer level")
	require.False(t, fields[5].Quoted)
	require.True(t, fields[6].OmitEmpty && fields[6].OmitZero)
	require.Nil(t, Of(reflect.TypeFor[*options]()), "a pointer is not a struct")
}
