package livewire

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/govengers/auth/authorization"
)

// Kind is the kind of a declaration.
type Kind string

// The declaration kinds.
const (
	KindResource Kind = "resource" // NewResource: a filterable list with item and distinct reads
	KindList     Kind = "list"     // NewList: a typed-parameters list
	KindObject   Kind = "object"   // NewObject: a typed-parameters object
	KindMutation Kind = "mutation" // NewMutation: a write with a JSON body
	KindUpload   Kind = "upload"   // NewUpload: a binary write, REST only
)

// Declaration is one entry of the registry: a *Resource, *List, *Object, *Mutation or
// *Upload. The interface is sealed to those five kinds; Registry.Add refuses anything else.
// Path is relative to the registry's BasePath and uses `{name}` placeholders.
type Declaration interface {
	Kind() Kind
	Name() string
	Method() string
	Path() string
	Tables() []string
	Check() authorization.Check
	Parameters() []Parameter
	DecodeParameters(source ParameterSource) (any, error)
	signature() *Signature
	routes() []route
}

// Signature is what every declaration kind shares: the name that is the wire target and
// generated identifier, the method and path of its main REST route, the tables that make it
// stale, its static check (D8) and its parameters. The kinds embed it.
type Signature struct {
	name       string
	method     string
	path       string
	tables     []string
	check      authorization.Check
	parameters *parameterSpec
	err        error
}

// Name is the wire target and the identifier of the generated clients.
func (s *Signature) Name() string { return s.name }

// Method is the HTTP method of the main REST route; reads are GET.
func (s *Signature) Method() string { return s.method }

// Path is the main REST route below the registry's BasePath, with `{name}` placeholders.
func (s *Signature) Path() string { return s.path }

// Tables are the tables whose changes re-run the declaration's subscriptions; none means the
// declaration is not live.
func (s *Signature) Tables() []string { return slices.Clone(s.tables) }

// Check is the static gate (D8): it runs before parameters are resolved against the
// database and before a body is read.
func (s *Signature) Check() authorization.Check { return s.check }

// Parameters describes the parameters struct for the generators; a resource has none of
// its own (its reads take the reserved parameters and the item key).
func (s *Signature) Parameters() []Parameter {
	if s.parameters == nil {
		return nil
	}
	return s.parameters.parameters()
}

// DecodeParameters fills a new parameters value (a *Params) from source. A missing path
// parameter or a value of the wrong form is an *Error answering 400 invalid_request that
// names the parameter.
func (s *Signature) DecodeParameters(source ParameterSource) (any, error) {
	return s.parameters.decode(source)
}

func (s *Signature) signature() *Signature { return s }

func (s *Signature) fail(format string, arguments ...any) {
	s.failWith(fmt.Errorf(format, arguments...))
}

func (s *Signature) failWith(err error) {
	if s.err == nil && err != nil {
		s.err = err
	}
}

func (s *Signature) compileParameters(parametersType reflect.Type) {
	if s.err != nil {
		return
	}
	spec, err := compileParameters(parametersType, s.path)
	if err != nil {
		s.err = err
		return
	}
	s.parameters = spec
}

func newSignature(name, method, path string, tables []string, check authorization.Check) Signature {
	signature := Signature{name: name, method: method, path: path, tables: slices.Clone(tables), check: check}
	switch {
	case name == "":
		signature.fail("a declaration needs a name")
	case check == nil:
		signature.fail("Check is required: every declaration has a static gate (D8)")
	}
	return signature
}

type route struct {
	method string
	path   string
}

func (r route) String() string { return r.method + " " + r.path }

// ListResponse is the body of every list read (D14): the page that was applied, the total
// under scope and filter (at most CountCap + 1 when the resource caps it), the items, and
// in cursor mode the cursor of the next page or nil at the end (D38). Items is never null.
type ListResponse[Item any] struct {
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
	Total  int     `json:"total"`
	Items  []Item  `json:"items"`
	Next   *string `json:"next,omitempty"`
}

// DistinctValue is one row of a distinct read (D15): a value of the field and how many rows
// carry it.
type DistinctValue struct {
	Value any `json:"value"`
	Count int `json:"count"`
}

// GuardedField names a field of a typed-parameters list or object whose Read check guards
// it (D39): a refused check strips the field from every item.
type GuardedField struct {
	Name string
	Read authorization.Check
}

// jsonFieldsOf maps the JSON names of a struct's marshaled fields to the fields, embedded
// structs flattened the way encoding/json does. A type that is not a struct has none.
func jsonFieldsOf(itemType reflect.Type) map[string]reflect.StructField {
	for itemType.Kind() == reflect.Pointer {
		itemType = itemType.Elem()
	}
	fields := map[string]reflect.StructField{}
	if itemType.Kind() != reflect.Struct {
		return fields
	}
	for position := range itemType.NumField() {
		field := itemType.Field(position)
		name, tagged := jsonName(field)
		if name == "-" {
			continue
		}
		if field.Anonymous && !tagged && jsonEmbeddable(field.Type) {
			for embeddedName, embedded := range jsonFieldsOf(field.Type) {
				fields[embeddedName] = embedded
			}
			continue
		}
		if field.IsExported() {
			fields[name] = field
		}
	}
	return fields
}

func jsonEmbeddable(fieldType reflect.Type) bool {
	if fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	return fieldType.Kind() == reflect.Struct
}

func jsonName(field reflect.StructField) (name string, tagged bool) {
	tag, tagged := field.Tag.Lookup("json")
	name, _, _ = strings.Cut(tag, ",")
	if name == "" {
		return field.Name, tagged && tag != ""
	}
	return name, true
}

func requireJSONField(itemType reflect.Type, name, why string) error {
	if itemType.Kind() != reflect.Struct {
		return nil
	}
	if _, ok := jsonFieldsOf(itemType)[name]; !ok {
		return fmt.Errorf("the item type %s has no JSON field %q, which %s", itemType, name, why)
	}
	return nil
}

const (
	liveListNeedsID = "a live list is diffed by it"
	keyFieldJSON    = "id"
)

var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
