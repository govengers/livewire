package generate_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/generate/internal/fixture/billing"
	"github.com/govengers/livewire/query"
)

// pointerText marshals itself as text through a pointer receiver: only where it is
// addressable (a struct field, a slice element), not as a map value.
type pointerText struct {
	X int `json:"x"`
}

func (*pointerText) MarshalText() ([]byte, error) { return []byte("text"), nil }

// objectMarshaler writes an object; the string option does not quote it.
type objectMarshaler struct{}

func (objectMarshaler) MarshalJSON() ([]byte, error) { return []byte(`{"a":1}`), nil }

// keptNil calls a nil value non-zero, so omitzero writes it as null.
type keptNil []string

func (keptNil) IsZero() bool { return false }

type (
	enumStrings []string
	enumArray   [2]string
	enumMap     map[string]string
)

// Wrap is instantiated with two element types of the same Go name.
type Wrap[Item any] struct {
	Value Item `json:"value"`
}

// edgeItem carries the mapping cases of the second review round.
type edgeItem struct {
	Texts  map[string]pointerText `json:"texts"`
	Text   pointerText            `json:"text"`
	Quoted objectMarshaler        `json:"quoted,string"` //nolint:staticcheck // SA5008: the string option on a marshaler, a mapping case under test
	Kept   keptNil                `json:"kept,omitzero"`
	Values enumStrings            `json:"values" enum:"a,b"`
	Pair   enumArray              `json:"pair" enum:"a,b"`
	Labels enumMap                `json:"labels" enum:"a,b"`
	Amount json.Number            `json:"amount"`
	Exact  json.Number            `json:"exact,string"`
	First  Wrap[fixture.Part]     `json:"first"`
	Second Wrap[billing.Part]     `json:"second"`
}

type historyItem struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type roomParameters struct {
	Room string `path:"room"`
	Key  string `query:"key"`
}

type oddParameters struct {
	Mode string `path:"mode" enum:"?:,b"`
}

func historyResource(name string, versioned bool) *livewire.Resource {
	if versioned {
		return parts()
	}
	return livewire.NewResource(name, "/"+name, &livewire.ResourceOptions[historyItem]{
		Read:   authorization.IsUser(),
		Fields: query.Fields{{Name: "name", Type: query.TypeText, SQL: "history.name", Filter: true, Sort: true}},
		List: func(context.Context, *query.ListQuery) (*livewire.Page[historyItem], error) {
			return &livewire.Page[historyItem]{}, nil
		},
	})
}

// newEdgeRegistry declares what a registry may hold that once broke the generated client: a
// resource named history, an object with its own `key` parameter under presence, a required
// path parameter whose enum spells TypeScript's optional marker, a guarded library DTO and
// the mapping cases of edgeItem.
func newEdgeRegistry(t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		historyResource("history", false),
		livewire.NewObject("room", "/rooms/{room}", &livewire.ObjectOptions[edgeItem, roomParameters]{
			Read: authorization.IsUser(), Load: func(context.Context, *roomParameters) (*edgeItem, error) { return &edgeItem{}, nil },
		}),
		livewire.NewList("odd", "/odd/{mode}", &livewire.ListOptions[livewire.FilterItem, oddParameters]{
			Read: authorization.IsUser(), Guarded: []livewire.GuardedField{{Name: "name", Read: authorization.IsAdmin()}},
			Load: func(context.Context, *oddParameters) ([]livewire.FilterItem, error) { return nil, nil },
		}),
	))
	return registry
}

func edgeOptions(directory string) *generate.Options {
	return &generate.Options{
		TypeScriptDirectory: directory, OpenAPIFile: filepath.Join(directory, "openapi.yaml"), Presence: true,
		OpenAPI: generate.OpenAPIOptions{Title: "Edges", Version: "1"},
	}
}

func TestEdgeRegistryMapping(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, generate.Write(newEdgeRegistry(t), edgeOptions(directory)))
	types := readGenerated(t, directory, "types.ts")
	_, item, found := strings.Cut(types, "export interface EdgeItem {")
	require.True(t, found, types)
	item, _, _ = strings.Cut(item, "\n}\n")
	item += "\n"
	for _, line := range []string{
		"texts: Record<string, PointerText> | null", // a map value is not addressable: no pointer-receiver MarshalText
		"text: string",                             // a field is: its pointer-receiver MarshalText writes it
		"quoted: unknown",                          // the string option does not quote a marshaler
		"kept?: KeptNil | null",                    // omitzero with an IsZero that keeps nil
		"values: ('a' | 'b')[] | null",             // the enum tag through a named slice
		"pair: ('a' | 'b')[]",                      // through a named array
		"labels: Record<string, 'a' | 'b'> | null", // through a named map
		"amount: number",                           // json.Number is a number
		"exact: string",                            // which the string option quotes
		"first: WrapFixturePart",                   // type arguments of one name qualified by their packages
		"second: WrapBillingPart",
	} {
		require.Contains(t, item, "\n  "+line+"\n", line)
	}
	require.Contains(t, types, "export interface PointerText {\n  x: number\n}")
	require.Contains(t, types, "export interface LivewireFilterItem {", "a guarded library type is a DTO of its own")
	_, filterItem, _ := strings.Cut(types, "export interface LivewireFilterItem {")
	require.Contains(t, filterItem, "\n  name?: string\n")

	api := readGenerated(t, directory, "api.ts")
	require.Contains(t, api, "export interface RoomPresenceParameters {\n  room: string\n  key: string\n}", "the object's own key is the presence key, once and required")
	require.Contains(t, api, "export interface OddParameters {\n  mode: '?:' | 'b'\n}")
	require.Contains(t, api, "export function getOdd(client: runtime.Client, parameters: OddParameters, options?", "a required parameter has no default")
	require.Contains(t, readGenerated(t, directory, "filters.ts"), "export type HistoryDistinctField = 'name'", "a resource named history has its distinct fields")
}

type keyPathParameters struct {
	Key string `path:"key"`
}

type pointerKey struct{ A int }

func (*pointerKey) MarshalText() ([]byte, error) { return []byte("k"), nil }

type pointerKeyItem struct {
	Values map[pointerKey]string `json:"values"`
}

type reviewlower struct {
	A int `json:"a"`
}

type Reviewlower struct {
	B int `json:"b"`
}

// addressed is a named struct whose field marshals itself through a pointer receiver only.
type addressed struct {
	Text pointerText `json:"text"`
}

type addressedItem struct {
	Values map[string]addressed `json:"values"`
}

// scalarRegistry holds only scalar DTOs: api.ts imports nothing from types.ts.
func scalarRegistry(t *testing.T) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		livewire.NewList("words", "/words", &livewire.ListOptions[string, livewire.NoParameters]{
			Read: authorization.IsUser(), Load: func(context.Context, *livewire.NoParameters) ([]string, error) { return nil, nil },
		}),
		livewire.NewMutation("touch", http.MethodPost, "/touch", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
				return &livewire.NoContent{}, nil
			},
		}),
	))
	return registry
}

func TestScalarRegistryImportsNoTypes(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, generate.Write(scalarRegistry(t), &generate.Options{TypeScriptDirectory: directory}))
	require.NotContains(t, readGenerated(t, directory, "api.ts"), "from './types'")
	require.Contains(t, readGenerated(t, generateFixture(t), "typescript/api.ts"), "import type * as types from './types'")
}

type caseItem struct {
	Lower reviewlower `json:"lower"`
	Upper Reviewlower `json:"upper"`
}

func objectOf[Item any](name string) livewire.Declaration {
	return livewire.NewObject(name, "/"+name, &livewire.ObjectOptions[Item, livewire.NoParameters]{
		Read: authorization.IsUser(), Load: func(context.Context, *livewire.NoParameters) (*Item, error) { return new(Item), nil },
	})
}

func TestEdgeRegistryRefusals(t *testing.T) {
	testCases := []struct {
		name         string
		declarations []livewire.Declaration
		message      string
	}{
		{"a resource named history next to a history read", []livewire.Declaration{historyResource("history", false), historyResource("parts", true)}, `are both named "HistoryCondition"`},
		{"a presence key that is a path parameter", []livewire.Declaration{livewire.NewObject("room", "/rooms/{key}", &livewire.ObjectOptions[historyItem, keyPathParameters]{
			Read: authorization.IsUser(), Load: func(context.Context, *keyPathParameters) (*historyItem, error) { return &historyItem{}, nil },
		})}, "must be a string query parameter"},
		{"a map key with a pointer-receiver MarshalText", []livewire.Declaration{objectOf[pointerKeyItem]("keys")}, "with a value receiver"},
		{"a named struct as a map value whose field marshals itself through a pointer only", []livewire.Declaration{objectOf[addressedItem]("addressed")}, "is a map value"},
		{"two Go names of one package that differ in case", []livewire.Declaration{objectOf[caseItem]("cases")}, "generate_test.Reviewlower are both named"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registry, err := livewire.NewRegistry(nil)
			require.NoError(t, err)
			require.NoError(t, registry.Add(testCase.declarations...))
			directory := t.TempDir()
			options := &generate.Options{OpenAPIFile: filepath.Join(directory, "openapi.yaml"), OpenAPI: generate.OpenAPIOptions{Title: "x", Version: "1"}, Presence: true}
			require.ErrorContains(t, generate.Write(registry, options), testCase.message)
		})
	}
	_ = http.MethodGet
}
