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
)

// shape is the language-neutral model of one JSON value both emitters render: TypeScript
// from it, OpenAPI from it. Element is an array's element, a map's value, a list response's
// item and a version's item; Format is the OpenAPI format (date-time, uuid, byte, int32,
// int64, float, double); Unsigned marks an unsigned integer.
type shape struct {
	kind       shapeKind
	format     string
	nullable   bool
	unsigned   bool
	element    *shape
	properties []property
	reference  *namedType
	values     []string
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

var (
	timeType            = reflect.TypeFor[time.Time]()
	uuidType            = reflect.TypeFor[uuid.UUID]()
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
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

func newTypeSet(enums []Enum) (*typeSet, error) {
	set := &typeSet{
		named: map[reflect.Type]*namedType{}, enums: map[reflect.Type][]string{},
		guarded: map[reflect.Type]map[string]bool{}, reserved: map[string]bool{},
	}
	for _, enum := range enums {
		set.enums[enum.Type] = enum.Values
	}
	set.enums[reflect.TypeFor[livewire.Code]()] = stringsOf(errorCodes)
	set.enums[reflect.TypeFor[livewire.ChangeOperation]()] = stringsOf(changeOperations)
	for _, name := range slices.Concat(typeScriptGlobals, libraryNames) {
		set.reserved[name] = true
	}
	for _, library := range libraryTypes {
		named, err := set.reference(library.goType)
		if err != nil {
			return nil, err
		}
		named.name, named.library = library.name, true
		set.reserved[library.name] = true
	}
	return set, nil
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

// shapeOf is the shape of a value of goType as encoding/json writes it; enum, from a field's
// `enum` tag, replaces the string it reaches through pointers and collections.
func (s *typeSet) shapeOf(goType reflect.Type, enum []string) (*shape, error) {
	if goType.Kind() == reflect.Pointer {
		inner, err := s.shapeOf(goType.Elem(), enum)
		if err != nil {
			return nil, err
		}
		return inner.withNullable(true), nil
	}
	if enum != nil && goType.Kind() == reflect.String {
		return &shape{kind: shapeEnum, values: enum}, nil
	}
	if _, listed := s.enums[goType]; !listed {
		if special, ok := specialShape(goType); ok {
			return special, nil
		}
	}
	if goType.Name() != "" && goType.PkgPath() != "" {
		named, err := s.reference(goType)
		if err != nil {
			return nil, err
		}
		nilable := goType.Kind() == reflect.Slice || goType.Kind() == reflect.Map
		return &shape{kind: shapeReference, reference: named, nullable: nilable}, nil
	}
	return s.structure(goType, enum)
}

// specialShape covers the types that marshal themselves: time.Time as an RFC 3339 string,
// a TextMarshaler as a string (uuid.UUID with its format), any other json.Marshaler as a
// value nothing is known about.
func specialShape(goType reflect.Type) (*shape, bool) {
	pointer := reflect.PointerTo(goType)
	switch {
	case goType == timeType:
		return &shape{kind: shapeString, format: "date-time"}, true
	case goType == uuidType:
		return &shape{kind: shapeString, format: "uuid"}, true
	case goType.Implements(jsonMarshalerType) || pointer.Implements(jsonMarshalerType):
		return &shape{kind: shapeUnknown}, true
	case goType.Implements(textMarshalerType) || pointer.Implements(textMarshalerType):
		return &shape{kind: shapeString}, true
	default:
		return nil, false
	}
}

// reference returns the named type of goType, describing it on first sight; the type is
// registered before its definition is built, so a recursive type refers to itself.
func (s *typeSet) reference(goType reflect.Type) (*namedType, error) {
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
	definition, err := s.structure(goType, nil)
	if err != nil {
		return nil, err
	}
	named.definition = definition.withNullable(false)
	return named, nil
}

// structure is the shape of goType by its kind, whatever its name.
func (s *typeSet) structure(goType reflect.Type, enum []string) (*shape, error) {
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
		return s.sliceShape(goType, enum)
	case reflect.Array:
		element, err := s.shapeOf(goType.Elem(), enum)
		if err != nil {
			return nil, err
		}
		return &shape{kind: shapeArray, element: element}, nil
	case reflect.Map:
		return s.mapShape(goType, enum)
	case reflect.Struct:
		return s.objectShape(goType)
	default:
		return nil, fmt.Errorf("generate: encoding/json cannot marshal the type %s (%s)", goType, goType.Kind())
	}
}

// sliceShape: a nil slice is null; a []byte whose element does not marshal itself is a
// base64 string.
func (s *typeSet) sliceShape(goType reflect.Type, enum []string) (*shape, error) {
	element := goType.Elem()
	pointer := reflect.PointerTo(element)
	marshalsItself := pointer.Implements(jsonMarshalerType) || pointer.Implements(textMarshalerType)
	if element.Kind() == reflect.Uint8 && !marshalsItself {
		return &shape{kind: shapeString, format: "byte", nullable: true}, nil
	}
	elementShape, err := s.shapeOf(element, enum)
	if err != nil {
		return nil, err
	}
	return &shape{kind: shapeArray, element: elementShape, nullable: true}, nil
}

// mapShape: a nil map is null; the keys are strings in JSON.
func (s *typeSet) mapShape(goType reflect.Type, enum []string) (*shape, error) {
	key := goType.Key()
	switch key.Kind() {
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
	default:
		if !key.Implements(textMarshalerType) && !reflect.PointerTo(key).Implements(textMarshalerType) {
			return nil, fmt.Errorf("generate: encoding/json cannot marshal the map type %s: its key is no string, integer or TextMarshaler", goType)
		}
	}
	value, err := s.shapeOf(goType.Elem(), enum)
	if err != nil {
		return nil, err
	}
	return &shape{kind: shapeMap, element: value, nullable: true}, nil
}

// objectShape lists a struct's JSON fields as encoding/json writes them (jsonfield.Of): a
// field encoding/json may omit is optional and, present, carries no null its omission stands
// for; the `string` option makes a string of it; a guarded field (D39) is optional.
func (s *typeSet) objectShape(structType reflect.Type) (*shape, error) {
	object := &shape{kind: shapeObject}
	for _, field := range jsonfield.Of(structType) {
		fieldType := field.StructField.Type
		omitted := field.OmitZero || (field.OmitEmpty && emptyable(fieldType))
		var fieldShape *shape
		var err error
		switch {
		case field.Quoted:
			fieldShape = &shape{kind: shapeString, nullable: fieldType.Kind() == reflect.Pointer && !omitted}
		case omitted:
			fieldShape, err = s.presentShape(fieldType, enumTag(field.StructField))
		default:
			fieldShape, err = s.shapeOf(fieldType, enumTag(field.StructField))
		}
		if err != nil {
			return nil, fmt.Errorf("%w (the field %s of %s)", err, field.StructField.Name, structType)
		}
		guarded := s.guarded[structType][field.Name]
		optional := omitted || guarded || reachedThroughPointer(structType, field.StructField.Index)
		object.properties = append(object.properties, property{name: field.Name, shape: fieldShape, optional: optional, guarded: guarded})
	}
	return object, nil
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
func (s *typeSet) presentShape(fieldType reflect.Type, enum []string) (*shape, error) {
	if fieldType.Kind() == reflect.Pointer {
		return s.shapeOf(fieldType.Elem(), enum)
	}
	fieldShape, err := s.shapeOf(fieldType, enum)
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
// the first letter upper case), qualified with more and more of its package path while it
// collides with a reserved name or a type of another package, and numbered in the order the
// types were first reached when only that tells them apart (types of one package and name,
// local to two functions).
func (s *typeSet) assignNames() {
	var free []*namedType
	candidates := map[*namedType][]string{}
	level := map[*namedType]int{}
	for _, named := range s.ordered {
		if !named.library {
			free = append(free, named)
			candidates[named] = candidateNames(named.goType)
		}
	}
	for changed := true; changed; {
		changed = false
		packages := map[string]map[string]bool{}
		for _, named := range free {
			name := candidates[named][level[named]]
			if packages[name] == nil {
				packages[name] = map[string]bool{}
			}
			packages[name][named.goType.PkgPath()] = true
		}
		for _, named := range free {
			name := candidates[named][level[named]]
			collides := s.reserved[name] || len(packages[name]) > 1
			if collides && level[named] < len(candidates[named])-1 {
				level[named]++
				changed = true
			}
		}
	}
	taken := map[string]bool{}
	for name := range s.reserved {
		taken[name] = true
	}
	for _, named := range free {
		name := candidates[named][level[named]]
		unique := name
		for number := 2; taken[unique]; number++ {
			unique = name + strconv.Itoa(number)
		}
		taken[unique] = true
		named.name = unique
	}
}

// candidateNames are the names of a type from the plain Go name to the one qualified with
// its whole package path.
func candidateNames(goType reflect.Type) []string {
	base := baseName(goType.Name())
	names := []string{base}
	segments := strings.Split(goType.PkgPath(), "/")
	qualifier := ""
	for position := len(segments) - 1; position >= 0; position-- {
		qualifier = pascalCase(segments[position]) + qualifier
		names = append(names, qualifier+base)
	}
	return names
}

// baseName folds the type arguments of an instantiated generic type into its name:
// "Page[example.com/part.Item]" is "PageItem".
func baseName(goName string) string {
	outer, arguments, _ := strings.Cut(goName, "[")
	name := pascalCase(outer)
	for _, part := range qualifiedNamePart.FindAllString(arguments, -1) {
		if at := strings.LastIndexAny(part, "./"); at >= 0 {
			part = part[at+1:]
		}
		name += pascalCase(part)
	}
	return name
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
