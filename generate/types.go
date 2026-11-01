package generate

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/internal/jsonfield"
)

// shapeKind is what a JSON value of a Go type looks like, as encoding/json writes it.
type shapeKind int

const (
	shapeString shapeKind = iota
	shapeNumber
	shapeInteger
	shapeBoolean
	shapeUnknown
	shapeArray
	shapeMap
	shapeObject
	shapeReference
	shapeEnum
	shapeListResponse
	shapeVersion
	// shapeTimeValue is the operand of a condition on a time field: RFC 3339 or a relative time (D12).
	shapeTimeValue
)

// shape is the language-neutral model of one JSON value both emitters render: TypeScript
// from it, OpenAPI from it. Element is an array's element, a map's value, a list response's
// item and a version's item; format is the OpenAPI format (date-time, uuid, byte, int32,
// int64, float, double); unsigned marks an unsigned integer. The limits are the OpenAPI
// bounds of a filter operand (D12), zero for none.
type shape struct {
	kind          shapeKind
	format        string
	nullable      bool
	unsigned      bool
	element       *shape
	properties    []property
	reference     *namedType
	values        []string
	maximumLength int
	minimumItems  int
	maximumItems  int
}

// property is one member of an object: optional when encoding/json may omit it or the
// declaration guards it (D39), guarded for the OpenAPI extension.
type property struct {
	name     string
	shape    *shape
	optional bool
	guarded  bool
}

func (s *shape) withNullable(nullable bool) *shape {
	copied := *s
	copied.nullable = nullable
	return &copied
}

// namedType is a Go type with a name of its own in the generated code: a struct is an
// interface, any other named type an alias. Library types are the runtime's own (types.ts
// re-exports them) and keep their fixed names.
type namedType struct {
	goType     reflect.Type
	name       string
	definition *shape
	library    bool
}

// typeSet collects every named type the declarations reach, in the order they are first
// reached, and names them once all are known.
type typeSet struct {
	named    map[reflect.Type]*namedType
	ordered  []*namedType
	enums    map[reflect.Type][]string
	guarded  map[reflect.Type]map[string]bool
	reserved map[string]bool
}

// use is how a value is reached: the enum values of the field's `enum` tag, and whether the
// value is addressable, which decides whether encoding/json calls a marshaler with a pointer
// receiver (it does not for a map value).
type use struct {
	enum          []string
	unaddressable bool
}

var (
	timeType            = reflect.TypeFor[time.Time]()
	uuidType            = reflect.TypeFor[uuid.UUID]()
	jsonNumberType      = reflect.TypeFor[json.Number]()
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	isZeroerType        = reflect.TypeFor[interface{ IsZero() bool }]()
	errorCodes          = []livewire.Code{livewire.CodeInvalidRequest, livewire.CodeInvalidFilter, livewire.CodeInvalidSort, livewire.CodeInvalidPage, livewire.CodeInvalidCursor, livewire.CodeForbiddenField, livewire.CodeUnauthenticated, livewire.CodeForbidden, livewire.CodeNotFound, livewire.CodeConflict, livewire.CodeInProgress, livewire.CodeInternal}
	changeOperations    = []livewire.ChangeOperation{livewire.ChangeInsert, livewire.ChangeUpdate, livewire.ChangeDelete}
	typeScriptGlobals   = []string{"Array", "Blob", "Boolean", "Date", "Error", "Exclude", "Extract", "File", "FormData", "Function", "Map", "Number", "Object", "Omit", "Partial", "Pick", "Promise", "Readonly", "ReadonlyArray", "Record", "Required", "Response", "Set", "String", "Symbol", "URL"}
	qualifiedNamePart   = regexp.MustCompile(`[A-Za-z0-9_./\-]+`)
	identifierCharacter = regexp.MustCompile(`[^A-Za-z0-9]+`)
)

// libraryTypes are the core's DTOs the runtime declares; their Go types give the OpenAPI
// schemas, the names are fixed.
var libraryTypes = []struct {
	goType reflect.Type
	name   string
}{
	{reflect.TypeFor[livewire.Code](), "ErrorCode"},
	{reflect.TypeFor[livewire.ErrorBody](), "ErrorBody"},
	{reflect.TypeFor[livewire.ChangeOperation](), "ChangeOperation"},
	{reflect.TypeFor[livewire.DistinctValue](), "DistinctValue"},
	{reflect.TypeFor[livewire.PresenceItem](), "PresenceItem"},
	{reflect.TypeFor[livewire.FilterItem](), "FilterItem"},
	{reflect.TypeFor[livewire.FilterRequest](), "FilterRequest"},
	{reflect.TypeFor[livewire.BatchCall](), "BatchCall"},
	{reflect.TypeFor[livewire.BatchResult](), "BatchResult"},
}

// libraryNames are the names the runtime declares besides the library types: no DTO takes
// them either.
var libraryNames = []string{"ListResponse", "Version", "RelativeTime"}

func newTypeSet(enums []Enum) *typeSet {
	set := &typeSet{
		named: map[reflect.Type]*namedType{}, enums: map[reflect.Type][]string{},
		guarded: map[reflect.Type]map[string]bool{}, reserved: map[string]bool{},
	}
	for _, enum := range enums {
		set.enums[enum.Type] = enum.Values
	}
	set.enums[reflect.TypeFor[livewire.Code]()] = stringsOf(errorCodes)
	set.enums[reflect.TypeFor[livewire.ChangeOperation]()] = stringsOf(changeOperations)
	for _, name := range slices.Concat(typeScriptGlobals, libraryNames, libraryTypeNames()) {
		set.reserved[name] = true
	}
	return set
}

// describeLibrary registers the library types under their fixed names, after the guards are
// known: a library type with a guarded field (D39) is a DTO of its own, whose guarded fields
// are optional, since the runtime's declaration is not.
func (s *typeSet) describeLibrary() error {
	for _, library := range libraryTypes {
		named, err := s.reference(library.goType, use{})
		if err != nil {
			return err
		}
		if len(s.guarded[library.goType]) == 0 {
			named.name, named.library = library.name, true
		}
	}
	return nil
}

func stringsOf[Value ~string](values []Value) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	return result
}

// guard marks the JSON fields of an item type that a Read check guards (D39); the generated
// types make them optional.
func (s *typeSet) guard(itemType reflect.Type, names []string) {
	for itemType.Kind() == reflect.Pointer {
		itemType = itemType.Elem()
	}
	if len(names) == 0 {
		return
	}
	if s.guarded[itemType] == nil {
		s.guarded[itemType] = map[string]bool{}
	}
	for _, name := range names {
		s.guarded[itemType][name] = true
	}
}

// shapeOf is the shape of a value of goType as encoding/json writes it, reached as how says.
// The enum of a field's tag replaces the string it reaches through pointers and collections,
// named ones included.
func (s *typeSet) shapeOf(goType reflect.Type, how use) (*shape, error) {
	if goType.Kind() == reflect.Pointer {
		inner, err := s.shapeOf(goType.Elem(), use{enum: how.enum})
		if err != nil {
			return nil, err
		}
		return inner.withNullable(true), nil
	}
	if how.enum != nil && goType.Kind() == reflect.String {
		if _, special := specialShape(goType, how.unaddressable); special {
			return nil, fmt.Errorf("generate: the type %s marshals itself, so an enum tag cannot describe its JSON", goType)
		}
		return &shape{kind: shapeEnum, values: how.enum}, nil
	}
	if _, listed := s.enums[goType]; !listed {
		if special, ok := specialShape(goType, how.unaddressable); ok {
			return special, nil
		}
	}
	named := goType.Name() != "" && goType.PkgPath() != ""
	collection := goType.Kind() == reflect.Slice || goType.Kind() == reflect.Array || goType.Kind() == reflect.Map
	if named && (how.enum == nil || !collection) {
		return s.referenceShape(goType, how)
	}
	return s.structure(goType, how)
}

// referenceShape refers to the declaration of a named type; a nil-able one may be null.
func (s *typeSet) referenceShape(goType reflect.Type, how use) (*shape, error) {
	if how.unaddressable {
		if err := refuseAddressDependent(goType); err != nil {
			return nil, err
		}
	}
	reference, err := s.reference(goType, use{})
	if err != nil {
		return nil, err
	}
	nilable := goType.Kind() == reflect.Slice || goType.Kind() == reflect.Map
	return &shape{kind: shapeReference, reference: reference, nullable: nilable}, nil
}

// specialShape covers the types that marshal themselves: time.Time as an RFC 3339 string,
// uuid.UUID as a string with its format, json.Number as a number, another json.Marshaler as a
// value nothing is known about and an encoding.TextMarshaler as a string. A method with a
// pointer receiver counts only for an addressable value, as for encoding/json.
func specialShape(goType reflect.Type, unaddressable bool) (*shape, bool) {
	switch {
	case goType == timeType:
		return &shape{kind: shapeString, format: "date-time"}, true
	case goType == uuidType:
		return &shape{kind: shapeString, format: "uuid"}, true
	case goType == jsonNumberType:
		return &shape{kind: shapeNumber}, true
	case implements(goType, jsonMarshalerType, unaddressable):
		return &shape{kind: shapeUnknown}, true
	case implements(goType, textMarshalerType, unaddressable):
		return &shape{kind: shapeString}, true
	default:
		return nil, false
	}
}

func implements(goType, method reflect.Type, unaddressable bool) bool {
	return goType.Implements(method) || (!unaddressable && reflect.PointerTo(goType).Implements(method))
}

// reference returns the named type of goType, describing it on first sight; the type is
// registered before its definition is built, so a recursive type refers to itself.
func (s *typeSet) reference(goType reflect.Type, how use) (*namedType, error) {
	if named, ok := s.named[goType]; ok {
		return named, nil
	}
	named := &namedType{goType: goType}
	s.named[goType] = named
	s.ordered = append(s.ordered, named)
	if values, ok := s.enums[goType]; ok {
		named.definition = &shape{kind: shapeEnum, values: values}
		return named, nil
	}
	definition, err := s.structure(goType, how)
	if err != nil {
		return nil, err
	}
	named.definition = definition.withNullable(false)
	return named, nil
}

// structure is the shape of goType by its kind, whatever its name.
func (s *typeSet) structure(goType reflect.Type, how use) (*shape, error) {
	switch goType.Kind() {
	case reflect.Bool:
		return &shape{kind: shapeBoolean}, nil
	case reflect.Int, reflect.Int64:
		return &shape{kind: shapeInteger, format: "int64"}, nil
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return &shape{kind: shapeInteger, format: "int32"}, nil
	case reflect.Uint8, reflect.Uint16:
		return &shape{kind: shapeInteger, format: "int32", unsigned: true}, nil
	case reflect.Uint, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return &shape{kind: shapeInteger, format: "int64", unsigned: true}, nil
	case reflect.Float32:
		return &shape{kind: shapeNumber, format: "float"}, nil
	case reflect.Float64:
		return &shape{kind: shapeNumber, format: "double"}, nil
	case reflect.String:
		return &shape{kind: shapeString}, nil
	case reflect.Interface:
		return &shape{kind: shapeUnknown}, nil
	case reflect.Slice:
		return s.sliceShape(goType, how)
	case reflect.Array:
		element, err := s.shapeOf(goType.Elem(), how)
		if err != nil {
			return nil, err
		}
		return &shape{kind: shapeArray, element: element}, nil
	case reflect.Map:
		return s.mapShape(goType, how)
	case reflect.Struct:
		return s.objectShape(goType, how)
	default:
		return nil, fmt.Errorf("generate: encoding/json cannot marshal the type %s (%s)", goType, goType.Kind())
	}
}

// sliceShape: a nil slice is null, its elements are addressable; a []byte whose element does
// not marshal itself is a base64 string.
func (s *typeSet) sliceShape(goType reflect.Type, how use) (*shape, error) {
	element := goType.Elem()
	pointer := reflect.PointerTo(element)
	marshalsItself := pointer.Implements(jsonMarshalerType) || pointer.Implements(textMarshalerType)
	if element.Kind() == reflect.Uint8 && !marshalsItself {
		return &shape{kind: shapeString, format: "byte", nullable: true}, nil
	}
	elementShape, err := s.shapeOf(element, use{enum: how.enum})
	if err != nil {
		return nil, err
	}
	return &shape{kind: shapeArray, element: elementShape, nullable: true}, nil
}

// mapShape: a nil map is null; the keys are strings in JSON, a key type encoding/json takes
// is a string, an integer or a TextMarshaler by its own method set; the values are not
// addressable.
func (s *typeSet) mapShape(goType reflect.Type, how use) (*shape, error) {
	key := goType.Key()
	switch key.Kind() {
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
	default:
		if !key.Implements(textMarshalerType) {
			return nil, fmt.Errorf("generate: encoding/json cannot marshal the map type %s: its key is no string, integer or TextMarshaler (with a value receiver)", goType)
		}
	}
	value, err := s.shapeOf(goType.Elem(), use{enum: how.enum, unaddressable: true})
	if err != nil {
		return nil, err
	}
	return &shape{kind: shapeMap, element: value, nullable: true}, nil
}

// objectShape lists a struct's JSON fields as encoding/json writes them (jsonfield.Of): a
// field encoding/json may omit is optional and, present, carries no null its omission stands
// for (unless an IsZero method decides omitzero); the `string` option makes a string of a
// field that does not marshal itself; a guarded field (D39) is optional.
func (s *typeSet) objectShape(structType reflect.Type, how use) (*shape, error) {
	object := &shape{kind: shapeObject}
	for _, field := range jsonfield.Of(structType) {
		fieldType := field.StructField.Type
		emptied := field.OmitEmpty && emptyable(fieldType)
		// An IsZero method may call a nil collection non-zero; a nil pointer is omitted either way.
		zeroed := field.OmitZero && (fieldType.Kind() == reflect.Pointer || !implements(fieldType, isZeroerType, false))
		fieldUse := use{enum: enumTag(field.StructField), unaddressable: how.unaddressable}
		var fieldShape *shape
		var err error
		switch {
		case field.Quoted && !marshalsItself(fieldType, how.unaddressable):
			fieldShape = &shape{kind: shapeString, nullable: fieldType.Kind() == reflect.Pointer && !emptied && !field.OmitZero}
		case emptied || zeroed:
			fieldShape, err = s.presentShape(fieldType, fieldUse)
		default:
			fieldShape, err = s.shapeOf(fieldType, fieldUse)
		}
		if err != nil {
			return nil, fmt.Errorf("%w (the field %s of %s)", err, field.StructField.Name, structType)
		}
		guarded := s.guarded[structType][field.Name]
		optional := emptied || field.OmitZero || guarded || reachedThroughPointer(structType, field.StructField.Index)
		object.properties = append(object.properties, property{name: field.Name, shape: fieldShape, optional: optional, guarded: guarded})
	}
	return object, nil
}

// refuseAddressDependent refuses a named struct reached where it is not addressable (a map
// value) whose field marshals itself only through a pointer receiver: its one definition says
// how an addressable value marshals, encoding/json writes the field's underlying value there.
//
// The walk follows what stays unaddressable inside a map value: struct fields, named or not,
// and array elements, at any depth; a pointer, a slice and a map make their contents
// addressable again (or are refused on their own as map values).
func refuseAddressDependent(goType reflect.Type) error {
	return addressDependent(goType, goType.String(), map[reflect.Type]bool{})
}

func addressDependent(goType reflect.Type, path string, visited map[reflect.Type]bool) error {
	if visited[goType] {
		return nil
	}
	visited[goType] = true
	switch goType.Kind() {
	case reflect.Array:
		return addressValue(goType.Elem(), path+"[]", visited)
	case reflect.Struct:
		for _, field := range jsonfield.Of(goType) {
			if err := addressValue(field.StructField.Type, path+"."+field.StructField.Name, visited); err != nil {
				return err
			}
		}
	default:
	}
	return nil
}

// addressValue refuses a value inside a map value that marshals itself through a pointer
// receiver only, and walks on into it.
func addressValue(valueType reflect.Type, path string, visited map[reflect.Type]bool) error {
	for _, method := range []reflect.Type{jsonMarshalerType, textMarshalerType} {
		if !valueType.Implements(method) && reflect.PointerTo(valueType).Implements(method) {
			return fmt.Errorf("generate: %s is inside a map value, where it is not addressable, and marshals itself through a pointer receiver only: give the method a value receiver or the map pointer values", path)
		}
	}
	if _, special := specialShape(valueType, true); special {
		return nil
	}
	return addressDependent(valueType, path, visited)
}

// marshalsItselfAnyway reports whether a value of the type, addressable or not, has a marshaler,
// which takes precedence over listed enum values.
func marshalsItselfAnyway(goType reflect.Type) bool {
	return implements(goType, jsonMarshalerType, false) || implements(goType, textMarshalerType, false)
}

// marshalsItself reports whether a field's value (behind one unnamed pointer level, where the
// `string` option looks) is written by its own marshaler, which takes precedence over the
// option in encoding/json.
func marshalsItself(fieldType reflect.Type, unaddressable bool) bool {
	if fieldType.Name() == "" && fieldType.Kind() == reflect.Pointer {
		fieldType, unaddressable = fieldType.Elem(), false
	}
	if fieldType == jsonNumberType {
		return false // encoding/json quotes a json.Number under the string option
	}
	_, special := specialShape(fieldType, unaddressable)
	return special
}

// reachedThroughPointer reports whether a field is promoted through an embedded pointer:
// encoding/json leaves it out while that pointer is nil.
func reachedThroughPointer(structType reflect.Type, index []int) bool {
	current := structType
	for _, position := range index[:len(index)-1] {
		embedded := current.Field(position).Type
		if embedded.Kind() == reflect.Pointer {
			return true
		}
		current = embedded
	}
	return false
}

// presentShape is the shape of a field encoding/json omits when it is nil: present, its
// outer pointer is not nil and its slice or map not nil either.
func (s *typeSet) presentShape(fieldType reflect.Type, how use) (*shape, error) {
	if fieldType.Kind() == reflect.Pointer {
		return s.shapeOf(fieldType.Elem(), use{enum: how.enum})
	}
	fieldShape, err := s.shapeOf(fieldType, how)
	if err != nil {
		return nil, err
	}
	return fieldShape.withNullable(false), nil
}

// emptyable reports whether omitempty can omit a value of the type: encoding/json's isEmpty
// knows no empty struct, and an array is empty only at length zero.
func emptyable(fieldType reflect.Type) bool {
	switch fieldType.Kind() {
	case reflect.Struct:
		return false
	case reflect.Array:
		return fieldType.Len() == 0
	default:
		return true
	}
}

func enumTag(field reflect.StructField) []string {
	values, ok := field.Tag.Lookup("enum")
	if !ok || values == "" {
		return nil
	}
	return strings.Split(values, ",")
}

// assignNames gives every named type a unique name: its Go name (type arguments folded in,
// the first letter upper case), qualified with more and more of its type arguments' and then
// its own package path while it collides with a reserved name or another type. Only types of
// one package and Go name (local to two functions) are numbered, in the order they were
// first reached; two other types that no qualification tells apart (Go names that differ in
// case only) are refused.
func (s *typeSet) assignNames() error {
	var free []*namedType
	candidates := map[*namedType][]string{}
	for _, named := range s.ordered {
		if !named.library {
			free = append(free, named)
			candidates[named] = candidateNames(named.goType)
		}
	}
	level := s.qualify(free, candidates)
	current := func(named *namedType) string { return candidates[named][level[named]] }
	taken := map[string]string{}
	for _, named := range free {
		name := current(named)
		if s.reserved[name] {
			return fmt.Errorf("generate: the type %s would be named %s, a name the generated code takes; rename it", identityOf(named.goType), name)
		}
		unique, err := numbered(name, identityOf(named.goType), taken)
		if err != nil {
			return err
		}
		taken[unique] = identityOf(named.goType)
		named.name = unique
	}
	return nil
}

// qualify raises the candidate level of every type whose name is reserved or shared with a
// type of another identity until nothing changes or the candidates run out.
func (s *typeSet) qualify(free []*namedType, candidates map[*namedType][]string) map[*namedType]int {
	level := map[*namedType]int{}
	current := func(named *namedType) string { return candidates[named][level[named]] }
	for changed := true; changed; {
		changed = false
		identities := map[string]map[string]bool{}
		for _, named := range free {
			if identities[current(named)] == nil {
				identities[current(named)] = map[string]bool{}
			}
			identities[current(named)][identityOf(named.goType)] = true
		}
		for _, named := range free {
			collides := s.reserved[current(named)] || len(identities[current(named)]) > 1
			if collides && level[named] < len(candidates[named])-1 {
				level[named]++
				changed = true
			}
		}
	}
	return level
}

// numbered is name, or name with the next free number when a type of the same identity took
// it (types local to two functions); a type of another identity is refused.
func numbered(name, identity string, taken map[string]string) (string, error) {
	unique := name
	for number := 2; ; number++ {
		owner, ok := taken[unique]
		if !ok {
			return unique, nil
		}
		if owner != identity {
			return "", fmt.Errorf("generate: the types %s and %s are both named %s in the generated code; rename one", owner, identity, name)
		}
		unique = name + strconv.Itoa(number)
	}
}

// identityOf is a type's package path and Go name, type arguments included.
func identityOf(goType reflect.Type) string {
	return goType.PkgPath() + "." + goType.Name()
}

// candidateNames are the names of a type from the plain Go name, through the ones whose type
// arguments carry more and more of their package paths, to the one qualified with its own
// whole package path.
func candidateNames(goType reflect.Type) []string {
	outer, arguments, _ := strings.Cut(goType.Name(), "[")
	parts := qualifiedNamePart.FindAllString(arguments, -1)
	depth := 0
	for _, part := range parts {
		if at := strings.LastIndex(part, "."); at >= 0 {
			depth = max(depth, len(strings.Split(part[:at], "/")))
		}
	}
	var names []string
	for level := 0; level <= depth; level++ {
		names = append(names, pascalCase(outer)+foldArguments(parts, level))
	}
	base := names[len(names)-1]
	segments := strings.Split(goType.PkgPath(), "/")
	qualifier := ""
	for position := len(segments) - 1; position >= 0; position-- {
		qualifier = pascalCase(segments[position]) + qualifier
		names = append(names, qualifier+base)
	}
	return names
}

// foldArguments writes the type arguments of a generic name, each with the last level
// segments of its package path: "example.com/part.Item" is "Item" at 0, "PartItem" at 1.
func foldArguments(parts []string, level int) string {
	var builder strings.Builder
	for _, part := range parts {
		path, name := "", part
		if at := strings.LastIndex(part, "."); at >= 0 {
			path, name = part[:at], part[at+1:]
		}
		if path != "" && level > 0 {
			segments := strings.Split(path, "/")
			for _, segment := range segments[max(0, len(segments)-level):] {
				builder.WriteString(pascalCase(segment))
			}
		}
		builder.WriteString(pascalCase(name))
	}
	return builder.String()
}

// pascalCase keeps the identifier characters of text, each run after a separator starting
// upper case.
func pascalCase(text string) string {
	var builder strings.Builder
	for _, word := range identifierCharacter.Split(text, -1) {
		if word == "" {
			continue
		}
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		builder.WriteString(string(runes))
	}
	return builder.String()
}
