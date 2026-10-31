package livewire

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// namespaceResolver translates the validator's Go namespaces of one failed body
// ("createPart.Nested.Note", "audited.createPart.Secret", "order.Lines[1].SKU",
// "settings.Values[a.b]") to the body's JSON paths, resolved against the decoded body: the
// root type is dropped, untagged embedded structs contribute no segment, every other field
// its JSON name, a position in brackets names an element of the slice, and a key in brackets
// a key of the map whose text the namespace continues with. The validator writes keys as
// they are, so a key with brackets in it can read as the path of another (`k].Sub[j`): only a
// namespace the body explains exactly one way, through keys that write back unambiguously
// (their brackets balanced), is reported; anything else is "body", never the namespace as it
// is. Each map's key texts are formatted and sorted once per body.
type namespaceResolver struct {
	rootName string
	fields   fieldIndex
	body     reflect.Value
	keys     map[uintptr]*mapKeyTexts
	steps    int // the key bytes compared so far, so a test can bound the cost
}

// mapKeyTexts are the keys of one map by the text the validator writes for them (a text two
// keys can share), and the texts sorted.
type mapKeyTexts struct {
	sorted []string
	byText map[string][]reflect.Value
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
	walk := &namespaceWalk{resolver: r, namespace: remainder}
	result := walk.atName(r.body, 0)
	if result.count != 1 || !result.balanced {
		return "body"
	}
	return writePath(result.path.segments())
}

// namespaceWalk resolves one namespace. Only a map key can be read more than one way; every
// value of the body is reached by one namespace offset at most (its keys spell its offset),
// so the readings cost at most one visit per value.
type namespaceWalk struct {
	resolver  *namespaceResolver
	namespace string
}

// resolution is how the rest of a namespace resolves from one point: count is the number of
// readings (at most 2, which stands for more than one), path and balanced describe the first.
type resolution struct {
	count    int
	path     *pathNode
	balanced bool
}

// pathNode is a path as a list from its first segment, so readings share their tails.
type pathNode struct {
	segment pathSegment
	next    *pathNode
}

func (n *pathNode) segments() []pathSegment {
	var segments []pathSegment
	for node := n; node != nil; node = node.next {
		segments = append(segments, node.segment)
	}
	return segments
}

// prefixed is the resolution with segment in front.
func (s resolution) prefixed(segment pathSegment) resolution {
	s.path = &pathNode{segment: segment, next: s.path}
	return s
}

// atName resolves the field name at offset in the struct in value, and the rest after it.
func (w *namespaceWalk) atName(value reflect.Value, offset int) resolution {
	rest := w.namespace[offset:]
	nameEnd := strings.IndexAny(rest, ".[")
	if nameEnd < 0 {
		nameEnd = len(rest)
	}
	segment, next, ok := w.resolver.field(value, rest[:nameEnd])
	if !ok {
		return resolution{}
	}
	result := w.after(next, offset+nameEnd)
	if segment == nil || result.count == 0 {
		return result
	}
	return result.prefixed(*segment)
}

// after resolves what follows a name or a key at offset: the end, a dot and a name, or a
// bracket.
func (w *namespaceWalk) after(value reflect.Value, offset int) resolution {
	switch {
	case offset == len(w.namespace):
		return resolution{count: 1, balanced: true}
	case w.namespace[offset] == '.':
		return w.atName(value, offset+1)
	case w.namespace[offset] == '[':
		return w.atBracket(value, offset)
	default:
		return resolution{}
	}
}

// atBracket resolves the position or key in brackets at offset against the slice, array or
// map in value.
func (w *namespaceWalk) atBracket(value reflect.Value, offset int) resolution {
	container := indirect(value)
	switch {
	case isCollection(container.Kind()):
		end := strings.IndexByte(w.namespace[offset:], ']')
		position, err := strconv.Atoi(w.namespace[offset+1 : offset+max(end, 1)])
		inRange := end > 0 && err == nil && position >= 0 && position < container.Len()
		if !inRange {
			return resolution{}
		}
		result := w.after(container.Index(position), offset+end+1)
		if result.count == 0 {
			return result
		}
		return result.prefixed(pathSegment{text: w.namespace[offset+1 : offset+end], index: true})
	case container.Kind() == reflect.Map:
		return w.atKey(container, offset)
	default:
		return resolution{}
	}
}

// atKey counts the readings of the key at offset: every key of the map whose text, closed by
// a bracket, the namespace continues with and whose entry resolves the rest. Keys that share
// a text are one reading each.
func (w *namespaceWalk) atKey(container reflect.Value, offset int) resolution {
	texts := w.resolver.keyTextsOf(container)
	var total resolution
	for _, text := range w.resolver.keysStarting(texts, w.namespace[offset+1:]) {
		closed := offset + 1 + len(text)
		if closed == len(w.namespace) || w.namespace[closed] != ']' {
			continue
		}
		for _, mapKey := range texts.byText[text] {
			result := w.after(container.MapIndex(mapKey), closed+1)
			if result.count == 0 {
				continue
			}
			key := jsonKeyText(mapKey, text)
			result = result.prefixed(pathSegment{text: key, index: true})
			result.balanced = result.balanced && hasBalancedBrackets(key)
			result.count = min(total.count+result.count, 2)
			total = result
			if total.count == 2 {
				return total
			}
		}
	}
	return total
}

// field resolves the field name of the struct in value: its JSON name as the segment, none
// for an untagged embedded struct.
func (r *namespaceResolver) field(value reflect.Value, name string) (*pathSegment, reflect.Value, bool) {
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
		return &pathSegment{text: jsonName}, next, true
	}
	return nil, next, field.Anonymous
}

// keysStarting returns the key texts text starts with, shortest first. The sorted texts that
// share text's first bytes form one range, narrowed byte by byte with a binary search; the
// shortest text of the range sorts first, so a text that ends where the range stands is at
// its start; a range of one key is compared at once. The cost is the matched length times
// the logarithm of the number of keys, never a hash of each prefix.
func (r *namespaceResolver) keysStarting(texts *mapKeyTexts, text string) []string {
	var found []string
	low, high := 0, len(texts.sorted)
	for position := 0; low < high; position++ {
		if len(texts.sorted[low]) == position {
			found = append(found, texts.sorted[low])
			low++
		}
		if position == len(text) || low == high {
			break
		}
		if high-low == 1 {
			only := texts.sorted[low]
			r.steps += len(only) - position
			if strings.HasPrefix(text, only) {
				found = append(found, only)
			}
			break
		}
		next := text[position]
		r.steps++
		low += sort.Search(high-low, func(index int) bool { return texts.sorted[low+index][position] >= next })
		high = low + sort.Search(high-low, func(index int) bool { return texts.sorted[low+index][position] > next })
	}
	return found
}

func (r *namespaceResolver) keyTextsOf(container reflect.Value) *mapKeyTexts {
	if known, ok := r.keys[container.Pointer()]; ok {
		return known
	}
	texts := &mapKeyTexts{byText: make(map[string][]reflect.Value, container.Len())}
	for _, mapKey := range container.MapKeys() {
		text := fmt.Sprintf("%v", mapKey) // the validator's own formatting of a key
		if _, shared := texts.byText[text]; !shared {
			texts.sorted = append(texts.sorted, text)
		}
		texts.byText[text] = append(texts.byText[text], mapKey)
	}
	slices.Sort(texts.sorted)
	r.keys[container.Pointer()] = texts
	return texts
}

// jsonKeyText is the key as the body carries it: a string key's own text (a String method
// may format it otherwise), else the validator's text, which for integer keys is the same.
func jsonKeyText(mapKey reflect.Value, validatorText string) string {
	if mapKey.Kind() == reflect.String {
		return mapKey.String()
	}
	return validatorText
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
