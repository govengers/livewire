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

// jsonFieldsOf maps the JSON names of a struct's marshaled fields to the fields, following
// encoding/json's typeFields: one embedding level at a time, a type visited once (the
// shallowest occurrence), a type embedded twice at one level cancels its fields, embedded
// structs (by value or pointer) flatten unless their tag names them, a tagged field beats
// untagged ones at the same depth, and a name that stays ambiguous is dropped. A type that
// is not a struct has no fields.
func jsonFieldsOf(itemType reflect.Type) map[string]reflect.StructField {
	itemType = unwrapPointer(itemType)
	fields := map[string]reflect.StructField{}
	if itemType.Kind() != reflect.Struct {
		return fields
	}
	byName := collectJSONFields(itemType)
	for name, candidates := range byName {
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

type embeddedLevel struct {
	structType reflect.Type
	depth      int
}

// collectJSONFields walks the embedding levels breadth first and returns every candidate by
// JSON name; a type that occurs twice on one level has its candidates listed twice so the
// dominance rule drops them, like encoding/json.
func collectJSONFields(itemType reflect.Type) map[string][]jsonFieldCandidate {
	collector := &jsonFieldCollector{byName: map[string][]jsonFieldCandidate{}, visited: map[reflect.Type]bool{}}
	current := []embeddedLevel{{structType: itemType, depth: 0}}
	count := map[reflect.Type]int{itemType: 1}
	for len(current) > 0 {
		current, count = collector.collectLevel(current, count)
	}
	return collector.byName
}

type jsonFieldCollector struct {
	byName  map[string][]jsonFieldCandidate
	visited map[reflect.Type]bool
}

// collectLevel records the named fields of one embedding level and returns the next level
// with how often each type occurs on it.
func (c *jsonFieldCollector) collectLevel(current []embeddedLevel, count map[reflect.Type]int) ([]embeddedLevel, map[reflect.Type]int) {
	var next []embeddedLevel
	nextCount := map[reflect.Type]int{}
	for _, level := range current {
		if c.visited[level.structType] {
			continue
		}
		c.visited[level.structType] = true
		for position := range level.structType.NumField() {
			field := level.structType.Field(position)
			embeddedType := unwrapPointer(field.Type)
			switch classifyJSONField(field, embeddedType) {
			case jsonFieldSkipped:
			case jsonFieldFlattens:
				nextCount[embeddedType]++
				if nextCount[embeddedType] == 1 {
					next = append(next, embeddedLevel{structType: embeddedType, depth: level.depth + 1})
				}
			case jsonFieldNamed:
				c.record(field, level.depth, count[level.structType])
			}
		}
	}
	return next, nextCount
}

// record lists the candidate once per occurrence of its struct on the level, so a struct
// embedded twice cancels its fields in the dominance rule.
func (c *jsonFieldCollector) record(field reflect.StructField, depth, occurrences int) {
	name, tagged, _ := jsonName(field)
	candidate := jsonFieldCandidate{field: field, depth: depth, tagged: tagged}
	for range max(occurrences, 1) {
		c.byName[name] = append(c.byName[name], candidate)
	}
}

type jsonFieldClass int

const (
	jsonFieldSkipped jsonFieldClass = iota
	jsonFieldFlattens
	jsonFieldNamed
)

func classifyJSONField(field reflect.StructField, embeddedType reflect.Type) jsonFieldClass {
	_, tagged, included := jsonName(field)
	embeddedStruct := field.Anonymous && embeddedType.Kind() == reflect.Struct
	switch {
	case !included:
		return jsonFieldSkipped
	case embeddedStruct && !tagged:
		return jsonFieldFlattens
	case field.IsExported() || embeddedStruct:
		return jsonFieldNamed
	default:
		return jsonFieldSkipped
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

// jsonName is the field's JSON name, whether the tag names it and whether encoding/json
// marshals the field at all (`json:"-"` does not). A tag with options only
// (`json:",omitempty"`) does not name the field. The name is read back from encoding/json
// itself (a one-field struct with the field's tag is marshaled), so it follows the
// toolchain's own tag rules: which names it accepts, where it cuts an odd one, when it falls
// back to the Go name.
func jsonName(field reflect.StructField) (name string, tagged, included bool) {
	tagName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if tagName == "" {
		return field.Name, false, true
	}
	key, included := probeJSONKey(field.Tag, jsonProbeField)
	if !included {
		return "", false, false
	}
	if key == jsonProbeField {
		// Either the tag names the field "Probe" or encoding/json fell back to the Go name: a
		// second probe under another Go name tells them apart.
		if other, _ := probeJSONKey(field.Tag, jsonOtherProbeField); other == jsonOtherProbeField {
			return field.Name, false, true
		}
	}
	return key, true, true
}

const (
	jsonProbeField      = "Probe"
	jsonOtherProbeField = "OtherProbe"
)

var stringType = reflect.TypeFor[string]()

// probeJSONKey marshals a one-field struct named goName carrying tag and returns the key
// encoding/json wrote, or false when it wrote none.
func probeJSONKey(tag reflect.StructTag, goName string) (string, bool) {
	probeType := reflect.StructOf([]reflect.StructField{{Name: goName, Type: stringType, Tag: tag}})
	probe := reflect.New(probeType).Elem()
	probe.Field(0).SetString("x")
	marshaled, err := json.Marshal(probe.Interface())
	if err != nil {
		return "", false
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(marshaled, &keys) != nil {
		return "", false
	}
	for key := range keys {
		return key, true
	}
	return "", false
}

func unwrapPointer(fieldType reflect.Type) reflect.Type {
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	return fieldType
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
