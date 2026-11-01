package livewire

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire/internal/jsonfield"
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
// *Upload. The interface is sealed to those five kinds; a type that embeds one of them is
// refused by Registry.Add. Path is relative to the registry's BasePath and uses `{name}`
// placeholders. Name is the wire target and the identifier of the generated clients; Method
// is the HTTP method of the main REST route (GET for reads); Tables are the tables whose
// changes re-run the declaration's subscriptions, none means not live; Check is the static
// gate (D8) that runs before parameters are resolved against the database and before a body
// is read; Parameters describes the parameters struct for the generators; DecodeParameters
// fills a new parameters value (a *Parameters) from source and answers a missing path
// parameter, a repeated scalar parameter or a value of the wrong form with an *Error of 400
// invalid_request naming the parameter.
type Declaration interface {
	Kind() Kind
	Name() string
	Method() string
	Path() string
	Tables() []string
	Check() authorization.Check
	Parameters() []Parameter
	DecodeParameters(source ParameterSource) (any, error)
	// declaration returns the receiver; Registry.Add refuses a value that forwards it.
	declaration() Declaration
	shared() *signature
	routes() []route
}

// PartitionResolver names the partition of a subscription from the decoded parameters (a
// *Parameters, nil for a resource read) and the context (D25); the engine indexes
// subscriptions by table and partition and partitioned triggers carry the row's column.
// Without one the tenant of D42 is the partition.
type PartitionResolver func(ctx context.Context, parameters any) string

// signature is what every declaration kind shares. The kinds embed it; its methods are the
// Declaration interface.
type signature struct {
	name       string
	method     string
	path       string
	tables     []string
	check      authorization.Check
	parameters *parameterSpecification
	registry   *Registry
	err        error
}

// liveness is the part the three live kinds (Resource, List, Object) share.
type liveness struct {
	partition PartitionResolver
}

// Partition is the partition resolver of the declaration (D25), nil without one.
func (l *liveness) Partition() PartitionResolver { return l.partition }

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func newSignature(name, method, path string, tables []string, check authorization.Check) signature {
	declared := signature{name: name, method: method, path: path, tables: slices.Clone(tables), check: check}
	switch {
	case !identifier.MatchString(name):
		declared.fail("the name %q is not an identifier (a letter, then letters, digits and underscores)", name)
	case check == nil:
		declared.fail("Check is required: every declaration has a static gate (D8)")
	}
	return declared
}

func (s *signature) Name() string { return s.name }

func (s *signature) Method() string { return s.method }

func (s *signature) Path() string { return s.path }

func (s *signature) Tables() []string { return slices.Clone(s.tables) }

func (s *signature) Check() authorization.Check { return s.check }

func (s *signature) Parameters() []Parameter {
	if s.parameters == nil {
		return nil
	}
	return s.parameters.parameters()
}

func (s *signature) DecodeParameters(source ParameterSource) (any, error) {
	return s.parameters.decode(source)
}

func (s *signature) shared() *signature { return s }

func (s *signature) live() bool { return len(s.tables) > 0 }

func (s *signature) fail(format string, arguments ...any) {
	s.failWith(fmt.Errorf(format, arguments...))
}

func (s *signature) failWith(err error) {
	if s.err == nil && err != nil {
		s.err = err
	}
}

func (s *signature) compileParameters(parametersType reflect.Type) {
	if s.err != nil {
		return
	}
	specification, err := compileParameters(parametersType, s.path)
	if err != nil {
		s.err = err
		return
	}
	s.parameters = specification
}

// hasPathParameter reports whether the parameters struct carries `path:"name"`.
func (s *signature) hasPathParameter(name string) bool {
	return s.parameters != nil && s.parameters.hasPath(name)
}

type route struct {
	method string
	path   string
}

func (r route) String() string { return r.method + " " + r.path }

// pattern is the route with every placeholder name dropped, so two routes that differ only
// in placeholder names (which routers refuse) index the same.
func (r route) pattern() route {
	segments := strings.Split(r.path, "/")
	for position, segment := range segments {
		if isPlaceholder(segment) {
			segments[position] = "{}"
		}
	}
	return route{method: r.method, path: strings.Join(segments, "/")}
}

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

func isSuccessStatus(status int) bool {
	return status >= http.StatusOK && status <= 299
}

func intOrDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// jsonFieldsOf maps the JSON names of a type's marshaled fields (pointers unwrapped) to the
// fields, as encoding/json picks them (jsonfield.ByName); a type that is not a struct has
// none.
func jsonFieldsOf(itemType reflect.Type) map[string]reflect.StructField {
	return jsonfield.ByName(unwrapPointer(itemType))
}

// unwrapPointer follows pointers to the type they point to; a pointer chain that loops
// (followPointers) is returned at the loop, a pointer type.
func unwrapPointer(fieldType reflect.Type) reflect.Type {
	pointed, _ := followPointers(fieldType)
	return pointed
}

// followPointers follows pointers to the type they point to; ends is false for a chain that
// returns into itself (`type Loop *Loop`), which points to no value. The chain is followed at
// two speeds, so a loop is found without remembering the chain.
func followPointers(fieldType reflect.Type) (pointed reflect.Type, ends bool) {
	slow := fieldType
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
		if fieldType.Kind() != reflect.Pointer {
			return fieldType, true
		}
		fieldType = fieldType.Elem()
		slow = slow.Elem()
		if slow == fieldType {
			return fieldType, false
		}
	}
	return fieldType, true
}

// refusePointerLoops reports a pointer chain that loops anywhere the type reaches: no value
// can be decoded into it (encoding/json would allocate forever) or marshaled from it.
func refusePointerLoops(root reflect.Type) error {
	for _, reached := range reachableTypes(root) {
		if _, ends := followPointers(reached); !ends {
			return fmt.Errorf("the type %s is a pointer that points to itself", reached)
		}
	}
	return nil
}

// requireJSONField checks that a type (pointers unwrapped) marshals a field named name; why
// says who needs it. A scalar type can never carry one and is refused; a map or interface
// cannot be checked and passes.
func requireJSONField(itemType reflect.Type, name, why string) error {
	itemType = unwrapPointer(itemType)
	switch {
	case isJSONScalar(itemType) && !reflect.PointerTo(itemType).Implements(jsonMarshalerType):
		return fmt.Errorf("the type %s is a scalar and can carry no JSON field %q (%s)", itemType, name, why)
	case itemType.Kind() != reflect.Struct:
		return nil
	}
	if _, ok := jsonFieldsOf(itemType)[name]; !ok {
		return fmt.Errorf("the type %s has no JSON field %q (%s)", itemType, name, why)
	}
	return nil
}

// isJSONScalar reports whether values of the type marshal to a JSON string, number, boolean
// or array of such, never to an object.
func isJSONScalar(itemType reflect.Type) bool {
	switch itemType.Kind() {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64,
		reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

var jsonMarshalerType = reflect.TypeFor[json.Marshaler]()

const (
	liveListNeedsID = "a live list is diffed by id"
	keyFieldJSON    = "id"
)

var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
