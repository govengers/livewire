package livewire

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type resolvedLine struct {
	Note string `json:"note"`
}

// ResolvedEmbedded is embedded by pointer, so its promoted fields sit behind a nil pointer.
type ResolvedEmbedded struct {
	Deep string `json:"deep"`
}

type resolvedOrder struct {
	*ResolvedEmbedded
	Lines  []resolvedLine            `json:"lines"`
	Detail *resolvedLine             `json:"detail"`
	ByName map[string]resolvedLine   `json:"byName"`
	Extra  any                       `json:"extra"`
	Nested map[string][]resolvedLine `json:"nested"`
}

// The validator writes only namespaces of the body it validated; these are the ones it never
// writes, which still must never be reported as they are.
func TestNamespaceResolverRefusesWhatTheBodyCannotExplain(t *testing.T) {
	body := &resolvedOrder{
		Lines:  []resolvedLine{{Note: "a"}},
		ByName: map[string]resolvedLine{"a": {}},
		Extra:  []any{"x"},
		Nested: map[string][]resolvedLine{"a": {{}}, "a][0": {}}, // "a][0" reads the start, not the rest
	}
	resolver := newNamespaceResolver("resolvedOrder", indexFields(reflect.TypeFor[resolvedOrder]()), body)
	testCases := map[string]string{
		"resolvedOrder.Lines[0].Note":     "lines[0].note",
		"resolvedOrder.Extra[0]":          "extra[0]",
		"otherOrder.Lines[0].Note":        "body",
		"resolvedOrder.Lines[1].Note":     "body",
		"resolvedOrder.Lines[-1].Note":    "body",
		"resolvedOrder.Lines[x].Note":     "body",
		"resolvedOrder.Lines[0":           "body",
		"resolvedOrder.Lines[0]Note":      "body",
		"resolvedOrder.Lines[0]XNote":     "body",
		"resolvedOrder.ByName[aZ.Note":    "body",
		"resolvedOrder.Nested[a][0].Note": "nested[a][0].note",
		"resolvedOrder.Detail.Note":       "body",
		"resolvedOrder.Lines.Note":        "body",
		"resolvedOrder.Lines[0][0]":       "body",
		"resolvedOrder.ByName[b].Note":    "body",
		"resolvedOrder.Extra[0][0]":       "body",
		"resolvedOrder.Nope":              "body",
		"resolvedOrder.Deep":              "body",
		"resolvedOrder.Detail[0]":         "body",
	}
	for namespace, want := range testCases {
		require.Equal(t, want, resolver.pathOf(namespace), namespace)
	}
}

type bracketedValues struct {
	Values map[string][]string `json:"values" validate:"dive,dive,required"`
}

// keyMatchingSteps resolves the namespace of every element under one key of "a" and pairs
// of brackets, beside keys that are its prefixes, and returns the steps the key matching
// took and the namespaces' total length.
func keyMatchingSteps(t *testing.T, pairs, elements int) (steps, length int) {
	t.Helper()
	key := "a" + strings.Repeat("[]", pairs)
	body := &bracketedValues{Values: map[string][]string{key: make([]string, elements), "a": {}, "a[]": {}}}
	resolver := newNamespaceResolver("bracketedValues", indexFields(reflect.TypeFor[bracketedValues]()), body)
	for position := range elements {
		index := strconv.Itoa(position)
		namespace := "bracketedValues.Values[" + key + "][" + index + "]"
		require.Equal(t, "values["+key+"]["+index+"]", resolver.pathOf(namespace))
		length += len(namespace)
	}
	return resolver.steps, length
}

func TestNamespaceResolverMatchesKeysInLinearSteps(t *testing.T) {
	steps, length := keyMatchingSteps(t, 2000, 50)
	require.Positive(t, steps)
	require.LessOrEqual(t, steps, length, "a key byte is compared once per namespace")
	doubled, _ := keyMatchingSteps(t, 4000, 50)
	require.Less(t, float64(doubled), 2.5*float64(steps), "twice the key length costs twice the steps (%d and %d)", steps, doubled)
}
