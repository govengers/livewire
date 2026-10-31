package livewire

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
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
// *Upload. The interface is sealed to those five kinds; a type that embeds one of them is
// refused by Registry.Add. Path is relative to the registry's BasePath and uses `{name}`
// placeholders. Name is the wire target and the identifier of the generated clients; Method
// is the HTTP method of the main REST route (GET for reads); Tables are the tables whose
// changes re-run the declaration's subscriptions, none means not live; Check is the static
// gate (D8) that runs before parameters are resolved against the database and before a body
// is read; Parameters describes the parameters struct for the generators; DecodeParameters
// fills a new parameters value (a *Parameters) from source and answers a missing path parameter,
// a repeated scalar parameter or a value of the wrong form with an *Error of 400
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
	partition  PartitionResolver
	parameters *parameterSpec
	registry   *Registry
	err        error
}

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

// Partition is the partition resolver of a live declaration, nil without one.
func (s *signature) Partition() PartitionResolver { return s.partition }

func (s *signature) shared() *signature { return s }

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

func intOrDefault(status, fallback int) int {
	if status == 0 {
		return fallback
	}
	return status
}

// jsonFieldsOf maps the JSON names of a struct's marshaled fields to the fields, following
// encoding/json: embedded structs (by value or pointer) flatten unless their tag names them,
// the shallowest field of a name wins, a tagged field beats untagged ones at the same depth,
// and a name that stays ambiguous is dropped. Each embedded type is visited once, so a
// recursive embedding ends. A type that is not a struct has no fields.
func jsonFieldsOf(itemType reflect.Type) map[string]reflect.StructField {
	itemType = unwrapPointer(itemType)
	fields := map[string]reflect.StructField{}
	if itemType.Kind() != reflect.Struct {
		return fields
	}
	collector := &jsonFieldCollector{visited: map[reflect.Type]bool{itemType: true}}
	collector.collect(itemType, 0)
	for name, candidates := range collector.byName {
		if winner, ok := dominantJSONField(candidates); ok {
			fields[name] = winner
		}
	}
	return fields
}

type jsonFieldCandidate struct {
	field  reflect.StructField
	depth  int
	tagged bool
}

type jsonFieldCollector struct {
	byName  map[string][]jsonFieldCandidate
	visited map[reflect.Type]bool
	next    []reflect.Type
}

// collect walks one depth level at a time (breadth first), like encoding/json, so a
// shallower field is seen before a deeper one of the same name.
func (c *jsonFieldCollector) collect(structType reflect.Type, depth int) {
	if c.byName == nil {
		c.byName = map[string][]jsonFieldCandidate{}
	}
	for position := range structType.NumField() {
		field := structType.Field(position)
		if field.Tag.Get("json") == "-" {
			continue
		}
		name, tagged := jsonName(field)
		embeddedType := unwrapPointer(field.Type)
		flattens := field.Anonymous && !tagged && embeddedType.Kind() == reflect.Struct
		switch {
		case flattens && !c.visited[embeddedType]:
			c.visited[embeddedType] = true
			c.next = append(c.next, embeddedType)
		case flattens:
		case field.IsExported() || field.Anonymous && embeddedType.Kind() == reflect.Struct:
			c.byName[name] = append(c.byName[name], jsonFieldCandidate{field: field, depth: depth, tagged: tagged})
		}
	}
	for len(c.next) > 0 {
		pending := c.next
		c.next = nil
		for _, embedded := range pending {
			c.collect(embedded, depth+1)
		}
	}
}

func dominantJSONField(candidates []jsonFieldCandidate) (reflect.StructField, bool) {
	shallowest := slices.MinFunc(candidates, func(left, right jsonFieldCandidate) int { return left.depth - right.depth })
	var atDepth, tagged []jsonFieldCandidate
	for _, candidate := range candidates {
		if candidate.depth == shallowest.depth {
			atDepth = append(atDepth, candidate)
			if candidate.tagged {
				tagged = append(tagged, candidate)
			}
		}
	}
	switch {
	case len(atDepth) == 1:
		return atDepth[0].field, true
	case len(tagged) == 1:
		return tagged[0].field, true
	default:
		return reflect.StructField{}, false
	}
}

// jsonName is the field's JSON name and whether the tag names it; a tag with options only
// (`json:",omitempty"`) does not name the field.
func jsonName(field reflect.StructField) (name string, tagged bool) {
	tag := field.Tag.Get("json")
	name, _, _ = strings.Cut(tag, ",")
	if name == "" {
		return field.Name, false
	}
	return name, true
}

func unwrapPointer(fieldType reflect.Type) reflect.Type {
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	return fieldType
}

// requireJSONField checks that a struct type (pointers unwrapped) marshals a field named
// name; why says who needs it. A type that is not a struct cannot be checked and passes.
func requireJSONField(itemType reflect.Type, name, why string) error {
	itemType = unwrapPointer(itemType)
	if itemType.Kind() != reflect.Struct {
		return nil
	}
	if _, ok := jsonFieldsOf(itemType)[name]; !ok {
		return fmt.Errorf("the type %s has no JSON field %q (%s)", itemType, name, why)
	}
	return nil
}

const (
	liveListNeedsID = "a live list is diffed by id"
	keyFieldJSON    = "id"
)

var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
