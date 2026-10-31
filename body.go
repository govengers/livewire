package livewire

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// inspectBody reads raw once as a token stream alongside the request type and returns the
// body's top-level members by name with their raw text. An unknown member at any depth
// (struct fields, array elements, map values), a member that repeats within one object
// (skipped subtrees included) and a body that is not one JSON object are refused with fixed
// texts naming the member's path. The path is kept as a stack of segments and written only
// for the refusal, so a deep body costs memory in proportion to its size.
func inspectBody(raw []byte, requestType reflect.Type, fields fieldIndex) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalidParameter("body", "required")
	}
	walker := &bodyWalker{decoder: json.NewDecoder(bytes.NewReader(raw)), raw: raw, fields: fields, topLevel: map[string]json.RawMessage{}}
	walker.decoder.UseNumber()
	token, err := walker.decoder.Token()
	switch {
	case err != nil:
		return nil, invalidParameter("body", "must be valid JSON")
	case token != json.Delim('{'):
		return nil, invalidParameter("body", "must be an object")
	}
	if err := walker.walkObject(requestType, true); err != nil {
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
	segments []string
	topLevel map[string]json.RawMessage
}

// fieldIndex is the JSON field model of every struct type a request reaches, computed once
// at construction (indexFields), so the walk and the validation paths never probe
// encoding/json at request time.
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

// jsonNameOf returns the JSON name of the struct's field with the Go name, from the index.
func (i fieldIndex) jsonNameOf(structType reflect.Type, goName string) (string, bool) {
	for name, field := range i.of(structType) {
		if field.Name == goName {
			return name, true
		}
	}
	return "", false
}

// path writes the segments as the client reads them: "lines[1].sku", "values[a.b].note".
func (w *bodyWalker) path() string {
	var path strings.Builder
	for position, segment := range w.segments {
		if position > 0 && !strings.HasPrefix(segment, "[") {
			path.WriteByte('.')
		}
		path.WriteString(segment)
	}
	return path.String()
}

func (w *bodyWalker) refuse(message string) error {
	return invalidParameter(w.path(), message)
}

func (w *bodyWalker) push(segment string) { w.segments = append(w.segments, segment) }

func (w *bodyWalker) pop() { w.segments = w.segments[:len(w.segments)-1] }

// walkObject consumes the members of an object whose opening brace was read; the members
// are the JSON fields of structType.
func (w *bodyWalker) walkObject(structType reflect.Type, top bool) error {
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
		w.push(key)
		field, ok := known[key]
		switch {
		case seen[key]:
			return w.refuse("duplicate member")
		case !ok:
			return w.refuse("unknown field")
		}
		seen[key] = true
		start := w.decoder.InputOffset()
		if err := w.walkValue(field.Type); err != nil {
			return err
		}
		if top {
			w.topLevel[key] = bytes.TrimLeft(w.raw[start:w.decoder.InputOffset()], " \t\r\n:")
		}
		w.pop()
	}
}

// walkValue consumes one value of the type: objects of struct types are walked, elements of
// collections and values of maps descend with their segment, everything else is skipped
// (duplicate members are still refused there). A value of another shape than the type
// expects is left to the decoder, which names it.
func (w *bodyWalker) walkValue(fieldType reflect.Type) error {
	fieldType = unwrapPointer(fieldType)
	token, err := w.decoder.Token()
	if err != nil {
		return invalidParameter("body", "must be valid JSON")
	}
	switch {
	case hasOwnUnmarshaler(fieldType):
		return w.skipAfter(token)
	case token == json.Delim('{') && fieldType.Kind() == reflect.Struct:
		return w.walkObject(fieldType, false)
	case token == json.Delim('{') && fieldType.Kind() == reflect.Map:
		return w.walkEntries(fieldType.Elem())
	case token == json.Delim('[') && isCollection(fieldType.Kind()):
		return w.walkElements(fieldType.Elem())
	default:
		return w.skipAfter(token)
	}
}

func (w *bodyWalker) walkEntries(valueType reflect.Type) error {
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
		w.push("[" + key + "]")
		if seen[key] {
			return w.refuse("duplicate member")
		}
		seen[key] = true
		if err := w.walkValue(valueType); err != nil {
			return err
		}
		w.pop()
	}
}

func (w *bodyWalker) walkElements(elementType reflect.Type) error {
	for position := 0; w.decoder.More(); position++ {
		w.push("[" + strconv.Itoa(position) + "]")
		if err := w.walkValue(elementType); err != nil {
			return err
		}
		w.pop()
	}
	if _, err := w.decoder.Token(); err != nil {
		return invalidParameter("body", "must be valid JSON")
	}
	return nil
}

// skipAfter consumes the rest of a value the type does not model, whose first token was read:
// nothing for a scalar, every member of an object (repeated members refused) or element of
// an array.
func (w *bodyWalker) skipAfter(token json.Token) error {
	switch {
	case token == json.Delim('{'):
		return w.skipObject()
	case token == json.Delim('['):
		return w.skipArray()
	default:
		return nil
	}
}

func (w *bodyWalker) skipObject() error {
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
		w.push(key)
		if seen[key] {
			return w.refuse("duplicate member")
		}
		seen[key] = true
		if err := w.skipValue(); err != nil {
			return err
		}
		w.pop()
	}
}

func (w *bodyWalker) skipArray() error {
	for position := 0; w.decoder.More(); position++ {
		w.push("[" + strconv.Itoa(position) + "]")
		if err := w.skipValue(); err != nil {
			return err
		}
		w.pop()
	}
	if _, err := w.decoder.Token(); err != nil {
		return invalidParameter("body", "must be valid JSON")
	}
	return nil
}

func (w *bodyWalker) skipValue() error {
	token, err := w.decoder.Token()
	if err != nil {
		return invalidParameter("body", "must be valid JSON")
	}
	return w.skipAfter(token)
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
// included (the validator descends into them, the body walk does not). Every type is marked
// before its elements are followed, so a recursive collection (`type Tree map[string]Tree`)
// ends the walk.
func reachableStructs(requestType reflect.Type) []reflect.Type {
	var structs []reflect.Type
	visited := map[reflect.Type]bool{}
	var visit func(current reflect.Type)
	visit = func(current reflect.Type) {
		if visited[current] {
			return
		}
		visited[current] = true
		switch {
		case current.Kind() == reflect.Pointer, isCollection(current.Kind()), current.Kind() == reflect.Map:
			visit(current.Elem())
		case current.Kind() == reflect.Struct:
			structs = append(structs, current)
			for position := range current.NumField() {
				visit(current.Field(position).Type)
			}
		}
	}
	visit(requestType)
	return structs
}
