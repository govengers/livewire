package livewire

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// inspectBody reads raw once as a token stream alongside the request type and returns the
// body's top-level members by name with their raw text. An unknown member at any depth
// (struct fields, array elements, map values), a member that repeats within one object and
// a body that is not one JSON object are refused with fixed texts naming the member's path.
func inspectBody(raw []byte, requestType reflect.Type, fields fieldIndex) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalidParameter("body", "required")
	}
	walker := &bodyWalker{decoder: json.NewDecoder(bytes.NewReader(raw)), raw: raw, fields: fields, topLevel: map[string]json.RawMessage{}}
	token, err := walker.decoder.Token()
	switch {
	case err != nil:
		return nil, invalidParameter("body", "must be valid JSON")
	case token != json.Delim('{'):
		return nil, invalidParameter("body", "must be an object")
	}
	if err := walker.walkObject(requestType, "", true); err != nil {
		return nil, err
	}
	if _, err := walker.decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidParameter("body", "must be valid JSON")
	}
	return walker.topLevel, nil
}

type bodyWalker struct {
	decoder  *json.Decoder
	raw      []byte
	fields   fieldIndex
	topLevel map[string]json.RawMessage
}

// fieldIndex is the JSON field model of every struct type a request reaches, computed once
// at construction (indexFields) so a request never probes encoding/json.
type fieldIndex map[reflect.Type]map[string]reflect.StructField

func indexFields(requestType reflect.Type) fieldIndex {
	index := fieldIndex{}
	for _, structType := range reachableStructs(requestType) {
		index[structType] = jsonFieldsOf(structType)
	}
	return index
}

func (i fieldIndex) of(structType reflect.Type) map[string]reflect.StructField {
	if known, ok := i[structType]; ok {
		return known
	}
	return jsonFieldsOf(structType)
}

// walkObject consumes the members of an object whose opening brace was read; the members
// are the JSON fields of structType.
func (w *bodyWalker) walkObject(structType reflect.Type, prefix string, top bool) error {
	known := w.fields.of(structType)
	seen := map[string]bool{}
	for {
		token, err := w.decoder.Token()
		if err != nil {
			return invalidParameter("body", "must be valid JSON")
		}
		if token == json.Delim('}') {
			return nil
		}
		key, _ := token.(string)
		path := prefix + key
		field, ok := known[key]
		switch {
		case seen[key]:
			return invalidParameter(path, "duplicate member")
		case !ok:
			return invalidParameter(path, "unknown field")
		}
		seen[key] = true
		start := w.decoder.InputOffset()
		if err := w.walkValue(field.Type, path); err != nil {
			return err
		}
		if top {
			w.topLevel[key] = bytes.TrimLeft(w.raw[start:w.decoder.InputOffset()], " \t\r\n:")
		}
	}
}

// walkValue consumes one value of the type: objects of struct types are walked, elements of
// collections and values of maps descend with their path, everything else is skipped. A
// value of another shape than the type expects is left to the decoder, which names it.
func (w *bodyWalker) walkValue(fieldType reflect.Type, path string) error {
	fieldType = unwrapPointer(fieldType)
	token, err := w.decoder.Token()
	if err != nil {
		return invalidParameter("body", "must be valid JSON")
	}
	switch {
	case hasOwnUnmarshaler(fieldType):
		return w.skipAfter(token)
	case token == json.Delim('{') && fieldType.Kind() == reflect.Struct:
		return w.walkObject(fieldType, path+".", false)
	case token == json.Delim('{') && fieldType.Kind() == reflect.Map:
		return w.walkEntries(fieldType.Elem(), path)
	case token == json.Delim('[') && isCollection(fieldType.Kind()):
		return w.walkElements(fieldType.Elem(), path)
	default:
		return w.skipAfter(token)
	}
}

func (w *bodyWalker) walkEntries(valueType reflect.Type, path string) error {
	seen := map[string]bool{}
	for {
		token, err := w.decoder.Token()
		if err != nil {
			return invalidParameter("body", "must be valid JSON")
		}
		if token == json.Delim('}') {
			return nil
		}
		key, _ := token.(string)
		if seen[key] {
			return invalidParameter(path+"."+key, "duplicate member")
		}
		seen[key] = true
		if err := w.walkValue(valueType, path+"."+key); err != nil {
			return err
		}
	}
}

func (w *bodyWalker) walkElements(elementType reflect.Type, path string) error {
	for position := 0; w.decoder.More(); position++ {
		if err := w.walkValue(elementType, fmt.Sprintf("%s[%d]", path, position)); err != nil {
			return err
		}
	}
	if _, err := w.decoder.Token(); err != nil {
		return invalidParameter("body", "must be valid JSON")
	}
	return nil
}

// skipAfter consumes the rest of the value whose first token was read: nothing for a scalar,
// everything up to the matching close for an object or array.
func (w *bodyWalker) skipAfter(token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok || (delimiter != '{' && delimiter != '[') {
		return nil
	}
	for depth := 1; depth > 0; {
		next, err := w.decoder.Token()
		if err != nil {
			return invalidParameter("body", "must be valid JSON")
		}
		switch next {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

func hasOwnUnmarshaler(fieldType reflect.Type) bool {
	pointer := reflect.PointerTo(fieldType)
	return pointer.Implements(jsonUnmarshalerType) || pointer.Implements(textUnmarshalerType)
}

func isCollection(kind reflect.Kind) bool {
	return kind == reflect.Slice || kind == reflect.Array
}

// reachableStructs lists every struct type the request type reaches through fields,
// pointers, collections and map values, each once, types with their own unmarshaler
// excluded; the validator parses the tags of each on first use.
func reachableStructs(requestType reflect.Type) []reflect.Type {
	var structs []reflect.Type
	visited := map[reflect.Type]bool{}
	var visit func(current reflect.Type)
	visit = func(current reflect.Type) {
		current = unwrapPointer(current)
		for isCollection(current.Kind()) || current.Kind() == reflect.Map {
			current = unwrapPointer(current.Elem())
		}
		if current.Kind() != reflect.Struct || visited[current] || hasOwnUnmarshaler(current) {
			return
		}
		visited[current] = true
		structs = append(structs, current)
		for position := range current.NumField() {
			visit(current.Field(position).Type)
		}
	}
	visit(requestType)
	return structs
}

// jsonPathOf translates the validator's Go namespace ("createPart.Nested.Note",
// "audited.createPart.Secret", "order.Lines[1].SKU", "settings.Values[a.b]") to the body's
// JSON path: the root type is dropped, untagged embedded structs contribute no segment, every
// other field its JSON name, and index or key suffixes stay. A namespace the type cannot
// explain is reported as "body", never as it is.
func jsonPathOf(rootType reflect.Type, structNamespace string) string {
	rootType = unwrapPointer(rootType)
	remainder, found := strings.CutPrefix(structNamespace, rootType.Name())
	if !found {
		return "body"
	}
	var path []string
	currentType := rootType
	for _, segment := range splitNamespace(strings.TrimPrefix(remainder, ".")) {
		goName, suffix := splitIndexSuffix(segment)
		if currentType.Kind() != reflect.Struct {
			return "body"
		}
		field, ok := currentType.FieldByName(goName)
		if !ok {
			return "body"
		}
		name, tagged, _ := jsonName(field)
		if !field.Anonymous || tagged {
			path = append(path, name+suffix)
		}
		currentType = elementTypeOf(field.Type)
	}
	return strings.Join(path, ".")
}

// splitNamespace splits a namespace at the dots outside bracket suffixes, so a map key with
// a dot ("Values[a.b]") stays one segment.
func splitNamespace(namespace string) []string {
	var segments []string
	var current strings.Builder
	depth := 0
	for _, character := range namespace {
		switch {
		case character == '[':
			depth++
		case character == ']' && depth > 0:
			depth--
		case character == '.' && depth == 0:
			segments = append(segments, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(character)
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	return segments
}

// splitIndexSuffix separates "Lines[1]" into "Lines" and "[1]".
func splitIndexSuffix(segment string) (name, suffix string) {
	if open := strings.IndexByte(segment, '['); open >= 0 {
		return segment[:open], segment[open:]
	}
	return segment, ""
}

// elementTypeOf unwraps pointers, collections and maps down to the type a path continues in.
func elementTypeOf(fieldType reflect.Type) reflect.Type {
	fieldType = unwrapPointer(fieldType)
	for isCollection(fieldType.Kind()) || fieldType.Kind() == reflect.Map {
		fieldType = unwrapPointer(fieldType.Elem())
	}
	return fieldType
}

// formatNumberID writes a number the way JavaScript's Number toString does, so a numeric id
// keys the same on the server and in the browser: plain digits for 1e-6 <= |n| < 1e21,
// exponent form outside, and -0 as 0.
func formatNumberID(number float64) string {
	if number == 0 {
		return "0"
	}
	magnitude := number
	if magnitude < 0 {
		magnitude = -magnitude
	}
	if magnitude >= 1e-6 && magnitude < 1e21 {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(number, 'e', -1, 64), "e")
	sign, digits := exponent[:1], strings.TrimLeft(exponent[1:], "0")
	return mantissa + "e" + sign + digits
}
