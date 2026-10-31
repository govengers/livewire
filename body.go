package livewire

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
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
	walker := newBodyWalker(raw, fields)
	if err := walker.walkBody(requestType); err != nil {
		return nil, err
	}
	return walker.topLevel, nil
}

// pathOfTypeError re-walks a body that passed inspectBody to the innermost value of the
// request type that holds the error's offset and returns its path, or "body". The value found
// must be the one encoding/json names in the error's dotted Field: an error from inside a
// type's own unmarshaler carries an offset into that value, not into raw.
func pathOfTypeError(raw []byte, requestType reflect.Type, fields fieldIndex, typeError *json.UnmarshalTypeError) string {
	walker := newBodyWalker(raw, fields)
	walker.target = typeError.Offset
	reached := errors.Is(walker.walkBody(requestType), errTargetReached)
	if reached && dottedPath(walker.located) == typeError.Field {
		return writePath(walker.located)
	}
	return "body"
}

// errTargetReached ends a walk that looks for the value at the target offset.
var errTargetReached = errors.New("livewire: the target offset is reached")

type bodyWalker struct {
	decoder  *json.Decoder
	raw      []byte
	fields   fieldIndex
	segments []pathSegment
	topLevel map[string]json.RawMessage
	target   int64         // the offset whose value the walk locates, 0 for none
	located  []pathSegment // the path of the value that holds target
}

func newBodyWalker(raw []byte, fields fieldIndex) *bodyWalker {
	walker := &bodyWalker{decoder: json.NewDecoder(bytes.NewReader(raw)), raw: raw, fields: fields, topLevel: map[string]json.RawMessage{}}
	walker.decoder.UseNumber()
	return walker
}

// walkBody walks raw, which must be one JSON object of the request type.
func (w *bodyWalker) walkBody(requestType reflect.Type) error {
	if len(bytes.TrimSpace(w.raw)) == 0 {
		return invalidParameter("body", "required")
	}
	token, err := w.decoder.Token()
	switch {
	case err != nil:
		return invalidParameter("body", "must be valid JSON")
	case token != json.Delim('{'):
		return invalidParameter("body", "must be an object")
	}
	if err := w.walkObject(requestType, true); err != nil {
		return err
	}
	if _, err := w.decoder.Token(); !errors.Is(err, io.EOF) {
		return invalidParameter("body", "must be valid JSON")
	}
	return nil
}

// pathSegment is one step of a body path: a field's JSON name, or the key or position of an
// element (index), which the path writes in brackets.
type pathSegment struct {
	text  string
	index bool
}

// dottedPath writes the segments as encoding/json names a field in its errors: "lines.1.sku".
func dottedPath(segments []pathSegment) string {
	texts := make([]string, len(segments))
	for position, segment := range segments {
		texts[position] = segment.text
	}
	return strings.Join(texts, ".")
}

// writePath writes the segments as the client reads them: "lines[1].sku", "values[a.b].note".
func writePath(segments []pathSegment) string {
	var path strings.Builder
	for position, segment := range segments {
		switch {
		case segment.index:
			path.WriteString("[" + segment.text + "]")
			continue
		case position > 0:
			path.WriteByte('.')
		}
		path.WriteString(segment.text)
	}
	return path.String()
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

// jsonNameOf returns the JSON name of the struct's field, found by its index path: two
// fields can share a Go name (a promoted field of an embedded struct under another JSON name).
func (i fieldIndex) jsonNameOf(structType reflect.Type, field reflect.StructField) (string, bool) {
	for name, known := range i.of(structType) {
		if slices.Equal(known.Index, field.Index) {
			return name, true
		}
	}
	return "", false
}

func (w *bodyWalker) refuse(message string) error {
	return invalidParameter(writePath(w.segments), message)
}

func (w *bodyWalker) push(segment pathSegment) { w.segments = append(w.segments, segment) }

func (w *bodyWalker) pop() { w.segments = w.segments[:len(w.segments)-1] }

// nextMember reads the key of the next member of an object whose opening brace was read and
// pushes it as a segment (an index for the entries of a map); the caller pops it once the
// value is consumed. done is true at the closing brace. A key already in seen (the keys of
// the object so far) is refused: the duplicate-member rule of every object the walk reads.
func (w *bodyWalker) nextMember(seen map[string]bool, index bool) (key string, done bool, err error) {
	token, err := w.decoder.Token()
	switch {
	case err != nil:
		return "", false, invalidParameter("body", "must be valid JSON")
	case token == json.Delim('}'):
		return "", true, nil
	}
	key, _ = token.(string)
	w.push(pathSegment{text: key, index: index})
	if seen[key] {
		return "", false, w.refuse("duplicate member")
	}
	seen[key] = true
	return key, false, nil
}

// walkObject consumes the members of an object whose opening brace was read; the members
// are the JSON fields of structType.
func (w *bodyWalker) walkObject(structType reflect.Type, top bool) error {
	known := w.fields.of(structType)
	seen := map[string]bool{}
	for {
		key, done, err := w.nextMember(seen, false)
		if err != nil || done {
			return err
		}
		field, ok := known[key]
		if !ok {
			return w.refuse("unknown field")
		}
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

// walkValue consumes one value of the type; a walk that locates a target offset ends at the
// first innermost value that holds it, from the end of the token before it (where a map
// key's error points) to its own end.
func (w *bodyWalker) walkValue(fieldType reflect.Type) error {
	start := w.decoder.InputOffset()
	if err := w.consumeValue(fieldType); err != nil {
		return err
	}
	holdsTarget := start <= w.target && w.target <= w.decoder.InputOffset()
	if holdsTarget {
		w.located = slices.Clone(w.segments)
		return errTargetReached
	}
	return nil
}

// consumeValue consumes one value of the type: objects of struct types are walked, elements
// of collections and values of maps descend with their segment, everything else is skipped
// (duplicate members are still refused there). A value of another shape than the type
// expects is left to the decoder, which names it.
func (w *bodyWalker) consumeValue(fieldType reflect.Type) error {
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
		_, done, err := w.nextMember(seen, true)
		if err != nil || done {
			return err
		}
		if err := w.walkValue(valueType); err != nil {
			return err
		}
		w.pop()
	}
}

func (w *bodyWalker) walkElements(elementType reflect.Type) error {
	for position := 0; w.decoder.More(); position++ {
		w.push(pathSegment{text: strconv.Itoa(position), index: true})
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
		_, done, err := w.nextMember(seen, false)
		if err != nil || done {
			return err
		}
		if err := w.skipValue(); err != nil {
			return err
		}
		w.pop()
	}
}

func (w *bodyWalker) skipArray() error {
	for position := 0; w.decoder.More(); position++ {
		w.push(pathSegment{text: strconv.Itoa(position), index: true})
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

// hasElements reports whether values of the kind hold elements: a collection or a map.
func hasElements(kind reflect.Kind) bool {
	return isCollection(kind) || kind == reflect.Map
}

// hasElementType reports whether a type of the kind leads on to another through Elem: a
// pointer, a collection or a map.
func hasElementType(kind reflect.Kind) bool {
	return kind == reflect.Pointer || hasElements(kind)
}

// reachableTypes lists every type the root reaches through fields, pointers, collections and
// map values, each once, types with their own unmarshaler included (the validator descends
// into them, the body walk does not). Every type is marked before its elements are
// followed, so a recursive type (`type Tree map[string]Tree`, `type Loop *Loop`) ends the
// walk.
func reachableTypes(root reflect.Type) []reflect.Type {
	var reached []reflect.Type
	visited := map[reflect.Type]bool{}
	var visit func(current reflect.Type)
	visit = func(current reflect.Type) {
		if visited[current] {
			return
		}
		visited[current] = true
		reached = append(reached, current)
		switch {
		case hasElementType(current.Kind()):
			visit(current.Elem())
		case current.Kind() == reflect.Struct:
			for position := range current.NumField() {
				visit(current.Field(position).Type)
			}
		}
	}
	visit(root)
	return reached
}

// reachableStructs lists the struct types among reachableTypes.
func reachableStructs(root reflect.Type) []reflect.Type {
	return slices.DeleteFunc(reachableTypes(root), func(reached reflect.Type) bool { return reached.Kind() != reflect.Struct })
}
