package livewire

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// namespaceResolver translates the validator's Go namespaces of one failed body
// ("createPart.Nested.Note", "audited.createPart.Secret", "order.Lines[1].SKU",
// "settings.Values[a.b]") to the body's JSON paths, resolved against the decoded body: the
// root type is dropped, untagged embedded structs contribute no segment, every other field
// its JSON name, a position in brackets names an element of the slice, and a key in brackets
// the one key of the map whose text the namespace continues with. The validator writes keys
// as they are, so a key with brackets in it can read as the path of another (`k].Sub[j`): a
// namespace the body does not explain, or explains more than one way, is reported as "body",
// never as it is. Each map's key texts are formatted once per body.
type namespaceResolver struct {
	rootName string
	fields   fieldIndex
	body     reflect.Value
	keys     map[uintptr]*mapKeyTexts
}

// mapKeyTexts are the keys of one map by the text the validator writes for them; a text that
// two keys share maps to the zero Value.
type mapKeyTexts struct {
	byText  map[string]reflect.Value
	longest int
}

func newNamespaceResolver(rootName string, fields fieldIndex, body any) *namespaceResolver {
	return &namespaceResolver{rootName: rootName, fields: fields, body: reflect.ValueOf(body), keys: map[uintptr]*mapKeyTexts{}}
}

// pathOf returns the JSON path of the failure at structNamespace, or "body".
func (r *namespaceResolver) pathOf(structNamespace string) string {
	remainder, found := strings.CutPrefix(structNamespace, r.rootName+".")
	if !found {
		return "body"
	}
	value := r.body
	var path []pathSegment
	for {
		nameEnd := strings.IndexAny(remainder, ".[")
		if nameEnd < 0 {
			nameEnd = len(remainder)
		}
		segments, next, ok := r.field(value, remainder[:nameEnd])
		if !ok {
			return "body"
		}
		path = append(path, segments...)
		value, remainder = next, remainder[nameEnd:]
		for strings.HasPrefix(remainder, "[") {
			element, ok := r.element(value, remainder)
			if !ok {
				return "body"
			}
			path = append(path, pathSegment{text: element.key, index: true})
			value, remainder = element.value, element.rest
		}
		if remainder == "" {
			return writePath(path)
		}
		rest, dotted := strings.CutPrefix(remainder, ".")
		if !dotted {
			return "body"
		}
		remainder = rest
	}
}

// field resolves the field name of the struct in value: its JSON name as the segment, none
// for an untagged embedded struct.
func (r *namespaceResolver) field(value reflect.Value, name string) ([]pathSegment, reflect.Value, bool) {
	structValue := indirect(value)
	if structValue.Kind() != reflect.Struct {
		return nil, reflect.Value{}, false
	}
	field, ok := structValue.Type().FieldByName(name)
	if !ok {
		return nil, reflect.Value{}, false
	}
	next, err := structValue.FieldByIndexErr(field.Index)
	if err != nil {
		return nil, reflect.Value{}, false
	}
	if jsonName, named := r.fields.jsonNameOf(structValue.Type(), field); named {
		return []pathSegment{{text: jsonName}}, next, true
	}
	return nil, next, field.Anonymous
}

// resolvedElement is a key or position the namespace names, resolved in the body: its text
// for the path, its value, and the namespace after its closing bracket.
type resolvedElement struct {
	key   string
	value reflect.Value
	rest  string
}

// element resolves the bracketed key or position remainder starts with against the slice,
// array or map in value.
func (r *namespaceResolver) element(value reflect.Value, remainder string) (resolvedElement, bool) {
	container := indirect(value)
	switch {
	case isCollection(container.Kind()):
		end := strings.IndexByte(remainder, ']')
		position, err := strconv.Atoi(remainder[1:max(end, 1)])
		inRange := end > 0 && err == nil && position >= 0 && position < container.Len()
		if !inRange {
			return resolvedElement{}, false
		}
		return resolvedElement{key: remainder[1:end], value: container.Index(position), rest: remainder[end+1:]}, true
	case container.Kind() == reflect.Map:
		return r.entry(container, remainder)
	default:
		return resolvedElement{}, false
	}
}

// entry finds the one key of the map whose text, in brackets, remainder starts with and that
// writes back unambiguously (its brackets balanced).
func (r *namespaceResolver) entry(container reflect.Value, remainder string) (resolvedElement, bool) {
	texts := r.keyTextsOf(container)
	candidates, end := 0, 0
	for position := 1; position < len(remainder) && position <= texts.longest+1; position++ {
		if remainder[position] != ']' || !continuesAfter(remainder, position) {
			continue
		}
		if _, known := texts.byText[remainder[1:position]]; known {
			candidates++
			end = position
		}
	}
	if candidates != 1 {
		return resolvedElement{}, false
	}
	mapKey := texts.byText[remainder[1:end]]
	if !mapKey.IsValid() {
		return resolvedElement{}, false
	}
	key := jsonKeyText(mapKey, remainder[1:end])
	if !hasBalancedBrackets(key) {
		return resolvedElement{}, false
	}
	return resolvedElement{key: key, value: container.MapIndex(mapKey), rest: remainder[end+1:]}, true
}

// jsonKeyText is the key as the body carries it: a string key's own text (a String method
// may format it otherwise), else the validator's text, which for integer keys is the same.
func jsonKeyText(mapKey reflect.Value, validatorText string) string {
	if mapKey.Kind() == reflect.String {
		return mapKey.String()
	}
	return validatorText
}

func (r *namespaceResolver) keyTextsOf(container reflect.Value) *mapKeyTexts {
	if known, ok := r.keys[container.Pointer()]; ok {
		return known
	}
	texts := &mapKeyTexts{byText: make(map[string]reflect.Value, container.Len())}
	for _, mapKey := range container.MapKeys() {
		text := fmt.Sprintf("%v", mapKey) // the validator's own formatting of a key
		if _, shared := texts.byText[text]; shared {
			texts.byText[text] = reflect.Value{}
			continue
		}
		texts.byText[text] = mapKey
		texts.longest = max(texts.longest, len(text))
	}
	r.keys[container.Pointer()] = texts
	return texts
}

// continuesAfter reports whether the bracket at end closes a segment: the namespace ends
// there or goes on with a field or another key.
func continuesAfter(remainder string, end int) bool {
	next := end + 1
	return next == len(remainder) || remainder[next] == '.' || remainder[next] == '['
}

// hasBalancedBrackets reports whether every bracket in text closes one opened before it.
func hasBalancedBrackets(text string) bool {
	depth := 0
	for position := range len(text) {
		switch text[position] {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// indirect follows pointers and interfaces to the value they hold; at nil it returns the
// zero Value, whose kind is no struct, collection or map.
func indirect(value reflect.Value) reflect.Value {
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		value = value.Elem()
	}
	return value
}
