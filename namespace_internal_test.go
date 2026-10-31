package livewire

import (
	"reflect"
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
		Nested: map[string][]resolvedLine{"a": {{}}},
	}
	resolver := newNamespaceResolver("resolvedOrder", indexFields(reflect.TypeFor[resolvedOrder]()), body)
	testCases := map[string]string{
		"resolvedOrder.Lines[0].Note":     "lines[0].note",
		"resolvedOrder.Nested[a][0].Note": "nested[a][0].note",
		"resolvedOrder.Extra[0]":          "extra[0]",
		"otherOrder.Lines[0].Note":        "body",
		"resolvedOrder.Lines[1].Note":     "body",
		"resolvedOrder.Lines[-1].Note":    "body",
		"resolvedOrder.Lines[x].Note":     "body",
		"resolvedOrder.Lines[0":           "body",
		"resolvedOrder.Lines[0]Note":      "body",
		"resolvedOrder.Lines[0]XNote":     "body",
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
