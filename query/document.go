package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Operator is a filter operator as it is spelled in the document.
type Operator string

// The operators. Which ones a field accepts depends on its type, see OperatorsOf.
const (
	OperatorEqual          Operator = "eq"
	OperatorNotEqual       Operator = "ne"
	OperatorGreater        Operator = "gt"
	OperatorGreaterOrEqual Operator = "gte"
	OperatorLess           Operator = "lt"
	OperatorLessOrEqual    Operator = "lte"
	OperatorIn             Operator = "in"
	OperatorNotIn          Operator = "notIn"
	OperatorContains       Operator = "contains"
	OperatorStartsWith     Operator = "startsWith"
	OperatorIsNull         Operator = "isNull"
	OperatorHas            Operator = "has"
	OperatorHasAny         Operator = "hasAny"
	OperatorHasAll         Operator = "hasAll"
	OperatorIsEmpty        Operator = "isEmpty"
)

var operators = map[FieldType][]Operator{
	TypeString:      {OperatorEqual, OperatorNotEqual, OperatorIn, OperatorNotIn, OperatorContains, OperatorStartsWith, OperatorIsNull},
	TypeText:        {OperatorEqual, OperatorNotEqual, OperatorIn, OperatorNotIn, OperatorContains, OperatorStartsWith, OperatorIsNull},
	TypeInteger:     {OperatorEqual, OperatorNotEqual, OperatorGreater, OperatorGreaterOrEqual, OperatorLess, OperatorLessOrEqual, OperatorIn, OperatorNotIn, OperatorIsNull},
	TypeFloat:       {OperatorEqual, OperatorNotEqual, OperatorGreater, OperatorGreaterOrEqual, OperatorLess, OperatorLessOrEqual, OperatorIn, OperatorNotIn, OperatorIsNull},
	TypeTime:        {OperatorEqual, OperatorNotEqual, OperatorGreater, OperatorGreaterOrEqual, OperatorLess, OperatorLessOrEqual, OperatorIn, OperatorNotIn, OperatorIsNull},
	TypeBoolean:     {OperatorEqual, OperatorNotEqual, OperatorIsNull},
	TypeEnum:        {OperatorEqual, OperatorNotEqual, OperatorIn, OperatorNotIn, OperatorIsNull},
	TypeUUID:        {OperatorEqual, OperatorNotEqual, OperatorIn, OperatorNotIn, OperatorIsNull},
	TypeStringArray: {OperatorHas, OperatorHasAny, OperatorHasAll, OperatorIsEmpty},
}

// OperatorsOf lists the operators a filter may apply to a field of the type (D12), in the
// order the generators emit them. A type that is not filterable (json) has none. The result
// is the caller's copy.
func OperatorsOf(fieldType FieldType) []Operator {
	return slices.Clone(operators[fieldType])
}

// The limits of one document (D12). A document beyond any of them is refused with
// invalid_filter.
const (
	MaximumDepth         = 8
	MaximumNodes         = 200
	MaximumListValues    = 500
	MaximumStringBytes   = 1024
	MaximumDocumentBytes = 16 * 1024
)

// maximumSafeInteger is 2^53: every integer of a document must survive a float64 exactly,
// so the canonical form and the TypeScript side agree on it.
const maximumSafeInteger = 1 << 53

// maximumNumberExponent bounds the exponent of a number literal before big.Rat expands it:
// 1e-999999 would cost milliseconds, and an `in` carries up to 500 values.
const maximumNumberExponent = 400

// Document is a parsed and validated filter document of one resource: `{"where": <node>,
// "search": "..."}`, both parts optional. Parse is the only way to build one, so every
// document compiles. The empty document matches every row.
type Document struct {
	where  *node
	search string
}

// node is one node of the filter tree: exactly one of and, or, not or a condition (field,
// operator, value). value is decoded by the field's type: string, int64, float64, bool,
// time.Time (UTC) or relative; isNull and isEmpty carry a bool; the list operators a
// []any of those.
type node struct {
	and      []*node
	or       []*node
	not      *node
	field    string
	operator Operator
	value    any
}

// relative is a time relative to the database clock of the read, written as
// `{"relative": "-7d"}` and compiled to now() plus the duration.
type relative struct {
	duration time.Duration
}

// IsTimeDependent reports whether the document carries a relative time, which makes its
// result move with the clock without a write; the engine re-runs such subscriptions on a
// tick.
func (d *Document) IsTimeDependent() bool {
	return d != nil && d.where != nil && d.where.isTimeDependent()
}

func (n *node) isTimeDependent() bool {
	for _, child := range slices.Concat(n.and, n.or) {
		if child.isTimeDependent() {
			return true
		}
	}
	if n.not != nil {
		return n.not.isTimeDependent()
	}
	return isRelative(n.value) || slices.ContainsFunc(listValues(n.value), isRelative)
}

func isRelative(value any) bool {
	_, ok := value.(relative)
	return ok
}

func listValues(value any) []any {
	items, _ := value.([]any)
	return items
}

// FieldNames returns the distinct fields the conditions name, sorted. The root package
// evaluates the Read checks of D39 on them.
func (d *Document) FieldNames() []string {
	names := map[string]bool{}
	if d != nil && d.where != nil {
		d.where.collectFieldNames(names)
	}
	return slices.Sorted(maps.Keys(names))
}

func (n *node) collectFieldNames(names map[string]bool) {
	for _, child := range slices.Concat(n.and, n.or) {
		child.collectFieldNames(names)
	}
	if n.not != nil {
		n.not.collectFieldNames(names)
	}
	if n.field != "" {
		names[n.field] = true
	}
}

// Parse validates raw against the resource's fields and returns the document. A condition
// may only name a field declared Filter, with an operator of the field's type (OperatorsOf)
// and a value of that type; search needs a field declared Search. Every refusal is an
// *Error with CodeInvalidFilter naming the path of the offending part.
func Parse(fields Fields, raw []byte) (*Document, error) {
	if len(raw) > MaximumDocumentBytes {
		return nil, filterError("", "the document is %d bytes, at most %d are allowed", len(raw), MaximumDocumentBytes)
	}
	if err := refuseDuplicateKeys(raw); err != nil {
		return nil, err
	}
	top, err := decodeJSON(raw)
	if err != nil {
		return nil, filterError("", "is not valid JSON")
	}
	object, ok := top.(map[string]any)
	if !ok {
		return nil, filterError("", "must be an object")
	}
	document := &Document{}
	documentParser := &parser{fields: fields}
	// The keys in a fixed order: a document with several faults names the same one on every
	// request, whatever the map iteration order.
	for _, key := range slices.Sorted(maps.Keys(object)) {
		switch key {
		case "search":
			document.search, err = documentParser.search(object[key])
		case "where":
			document.where, err = documentParser.node(object[key], "where", 1)
		default:
			err = filterError(key, "unknown key %q", key)
		}
		if err != nil {
			return nil, err
		}
	}
	// The canonical form is what a stored filter registers and re-parses (D13); numbers and
	// escapes can grow it, so it must fit the limit as well.
	canonical, err := Canonical(document)
	if err != nil || len(canonical) > MaximumDocumentBytes {
		return nil, filterError("", "the canonical form of the document is larger than %d bytes", MaximumDocumentBytes)
	}
	return document, nil
}

type parser struct {
	fields Fields
	nodes  int
}

func (p *parser) search(value any) (string, error) {
	term, err := decodeString(value, "search")
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(p.fields, func(field Field) bool { return field.Search }) {
		return "", filterError("search", "this resource has no searchable field")
	}
	return term, nil
}

func (p *parser) node(value any, path string, depth int) (*node, error) {
	if depth > MaximumDepth {
		return nil, filterError(path, "the filter is nested deeper than %d levels", MaximumDepth)
	}
	p.nodes++
	if p.nodes > MaximumNodes {
		return nil, filterError(path, "the filter has more than %d nodes", MaximumNodes)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, filterError(path, "must be an object")
	}
	_, isCondition := object["field"]
	switch {
	case len(object) == 1 && object["and"] != nil:
		children, err := p.children(object["and"], path+".and", depth)
		return &node{and: children}, err
	case len(object) == 1 && object["or"] != nil:
		children, err := p.children(object["or"], path+".or", depth)
		return &node{or: children}, err
	case len(object) == 1 && object["not"] != nil:
		child, err := p.node(object["not"], path+".not", depth+1)
		return &node{not: child}, err
	case isCondition && len(object) <= 3:
		return p.condition(object, path)
	default:
		return nil, filterError(path, "a node is exactly one of and, or, not or a condition {field, op, value}")
	}
}

func (p *parser) children(value any, path string, depth int) ([]*node, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, filterError(path, "must be a list of nodes")
	}
	if len(items) == 0 {
		return nil, filterError(path, "must not be empty")
	}
	nodes := make([]*node, 0, len(items))
	for index, item := range items {
		child, err := p.node(item, fmt.Sprintf("%s[%d]", path, index), depth+1)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, child)
	}
	return nodes, nil
}

func (p *parser) condition(object map[string]any, path string) (*node, error) {
	name, err := decodeString(object["field"], path+".field")
	if err != nil {
		return nil, err
	}
	operatorName, err := decodeString(object["op"], path+".op")
	if err != nil {
		return nil, err
	}
	field, ok := p.fields.Lookup(name)
	if !ok || !field.Filter {
		return nil, filterError(path+".field", "unknown or unfilterable field %q", name)
	}
	operator := Operator(operatorName)
	if !slices.Contains(operators[field.Type], operator) {
		return nil, filterError(path+".op", "operator %q is not allowed on %s field %q", operatorName, field.Type, name)
	}
	raw, present := object["value"]
	if !present {
		return nil, filterError(path+".value", "a value is required")
	}
	value, err := decodeValue(field, operator, raw, path+".value")
	if err != nil {
		return nil, err
	}
	return &node{field: name, operator: operator, value: value}, nil
}

func decodeValue(field Field, operator Operator, raw any, path string) (any, error) {
	switch operator {
	case OperatorIsNull, OperatorIsEmpty:
		flag, ok := raw.(bool)
		if !ok {
			return nil, filterError(path, "%s takes true or false", operator)
		}
		return flag, nil
	case OperatorIn, OperatorNotIn, OperatorHasAny, OperatorHasAll:
		return decodeList(field, raw, path)
	default:
		return decodeScalar(field, raw, path)
	}
}

func decodeList(field Field, raw any, path string) (any, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, filterError(path, "takes a list of values")
	}
	if len(items) == 0 {
		return nil, filterError(path, "the list must not be empty")
	}
	if len(items) > MaximumListValues {
		return nil, filterError(path, "the list has more than %d values", MaximumListValues)
	}
	values := make([]any, 0, len(items))
	for index, item := range items {
		value, err := decodeScalar(field, item, fmt.Sprintf("%s[%d]", path, index))
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// decodeScalar reads one document value of the field's type under the D12 limits. Numbers
// arrive as json.Number.
func decodeScalar(field Field, raw any, path string) (any, error) {
	switch field.Type {
	case TypeString, TypeText, TypeStringArray:
		return decodeString(raw, path)
	case TypeEnum:
		return decodeEnum(field, raw, path)
	case TypeUUID:
		return decodeUUID(raw, path)
	case TypeBoolean:
		flag, ok := raw.(bool)
		if !ok {
			return nil, filterError(path, "must be true or false")
		}
		return flag, nil
	case TypeInteger:
		return decodeInteger(raw, path)
	case TypeFloat:
		return decodeFloat(raw, path)
	case TypeTime:
		return decodeTime(raw, path)
	case TypeJSON:
		return nil, filterError(path, "the field is not filterable")
	}
	return nil, filterError(path, "the field is not filterable")
}

// decodeString is the one gate every document string passes: values, list elements, enum
// and relative spellings, times and the search term. PostgreSQL text holds no U+0000, so a
// string with one is refused here instead of failing in the database.
func decodeString(raw any, path string) (string, error) {
	text, ok := raw.(string)
	if !ok {
		return "", filterError(path, "must be a string")
	}
	if len(text) > MaximumStringBytes {
		return "", filterError(path, "the string is longer than %d bytes", MaximumStringBytes)
	}
	if strings.ContainsRune(text, 0) {
		return "", filterError(path, "the string must not contain U+0000")
	}
	return text, nil
}

func decodeEnum(field Field, raw any, path string) (any, error) {
	text, err := decodeString(raw, path)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(field.Enum, text) {
		return nil, filterError(path, "%q is not one of %v", text, field.Enum)
	}
	return text, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// decodeUUID accepts the hyphenated form in any case and keeps it in lower case, the one
// spelling PostgreSQL and the canonical form use.
func decodeUUID(raw any, path string) (any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, filterError(path, "must be a string")
	}
	if !uuidPattern.MatchString(text) {
		return nil, filterError(path, "%q is not a uuid", text)
	}
	return strings.ToLower(text), nil
}

// decodeInteger checks integrality and the 2^53 bound on the exact decimal, before a
// conversion to float64 could round 2^53+1 down to 2^53 or 1.0000000000000001 to 1.
func decodeInteger(raw any, path string) (any, error) {
	exact, err := decodeExactNumber(raw, path)
	if err != nil {
		return nil, err
	}
	if !exact.IsInt() {
		return nil, filterError(path, "must be an integer")
	}
	if new(big.Rat).Abs(exact).Cmp(new(big.Rat).SetInt64(maximumSafeInteger)) > 0 {
		return nil, filterError(path, "the integer must be at most 2^53 in magnitude")
	}
	return exact.Num().Int64(), nil
}

// decodeFloat refuses a literal that a float64 cannot hold: an infinite one, or a non-zero
// one that would silently become 0.
func decodeFloat(raw any, path string) (any, error) {
	exact, err := decodeExactNumber(raw, path)
	if err != nil {
		return nil, err
	}
	approximate, _ := exact.Float64()
	if math.IsInf(approximate, 0) {
		return nil, filterError(path, "the number is too large for a float")
	}
	if approximate == 0 && exact.Sign() != 0 {
		return nil, filterError(path, "the number is too small for a float")
	}
	return approximate, nil
}

func decodeExactNumber(raw any, path string) (*big.Rat, error) {
	number, ok := raw.(json.Number)
	if !ok {
		return nil, filterError(path, "must be a number")
	}
	literal := number.String()
	if at := strings.IndexAny(literal, "eE"); at >= 0 {
		exponent, err := strconv.Atoi(literal[at+1:])
		if err != nil || !exponentInRange(exponent) {
			return nil, filterError(path, "the exponent must be between -%d and %d", maximumNumberExponent, maximumNumberExponent)
		}
	}
	exact, ok := new(big.Rat).SetString(literal)
	if !ok {
		return nil, filterError(path, "must be a number")
	}
	return exact, nil
}

func exponentInRange(exponent int) bool {
	return exponent >= -maximumNumberExponent && exponent <= maximumNumberExponent
}

// decodeTime reads an RFC 3339 string or {"relative": "-7d"}. An absolute time is kept in
// UTC, so the value bound to the statement is the instant the canonical form shows.
func decodeTime(raw any, path string) (any, error) {
	if object, ok := raw.(map[string]any); ok {
		return decodeRelativeTime(object, path)
	}
	if _, isString := raw.(string); !isString {
		return nil, filterError(path, `a time is an RFC 3339 string or {"relative": "-7d"}`)
	}
	text, err := decodeString(raw, path)
	if err != nil {
		return nil, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil, filterError(path, "%q is not an RFC 3339 time", text)
	}
	if !yearInRange(parsed.UTC()) {
		return nil, filterError(path, "%q is outside the years %d to %d in UTC", text, minimumYear, maximumYear)
	}
	return parsed.UTC(), nil
}

// The years RFC 3339 can spell: a time outside them has no canonical form and no cursor.
const (
	minimumYear = 0
	maximumYear = 9999
)

func yearInRange(value time.Time) bool {
	return value.Year() >= minimumYear && value.Year() <= maximumYear
}

func decodeRelativeTime(object map[string]any, path string) (any, error) {
	if len(object) != 1 {
		return nil, filterError(path, `a time is an RFC 3339 string or {"relative": "-7d"}`)
	}
	expression, err := decodeString(object["relative"], path)
	if err != nil {
		return nil, err
	}
	duration, err := parseRelative(expression)
	if err != nil {
		return nil, filterError(path, "%v", err)
	}
	return relative{duration: duration}, nil
}

var (
	relativePattern = regexp.MustCompile(`^[+-]?(?:[0-9]+[smhdw])+$`)
	relativePart    = regexp.MustCompile(`([0-9]+)([smhdw])`)
	relativeUnits   = map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
)

// maximumRelative keeps a relative time far inside what a time.Duration and a PostgreSQL
// interval hold: a hundred years.
const maximumRelative = 100 * 366 * 24 * time.Hour

// parseRelative reads a signed sequence of <integer><unit> with the units s, m, h, d and w:
// "-7d", "-1h30m", "+2w". A missing sign means "+". Every part is bounded before it is
// multiplied, so no sum can overflow.
func parseRelative(expression string) (time.Duration, error) {
	if !relativePattern.MatchString(expression) {
		return 0, fmt.Errorf("%q is not a relative time like -7d or -1h30m (units s m h d w)", expression)
	}
	var total time.Duration
	for _, part := range relativePart.FindAllStringSubmatch(expression, -1) {
		unit := relativeUnits[part[2]]
		count, err := strconv.ParseInt(part[1], 10, 64)
		if err != nil || count > int64(maximumRelative/unit) {
			return 0, fmt.Errorf("%q is out of range", expression)
		}
		total += time.Duration(count) * unit
		if total > maximumRelative {
			return 0, fmt.Errorf("%q is out of range", expression)
		}
	}
	if expression[0] == '-' {
		total = -total
	}
	return total, nil
}

// decodeJSON reads exactly one JSON value with numbers kept as json.Number.
func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the document")
	}
	return value, nil
}

// refuseDuplicateKeys walks the token stream once: encoding/json keeps the last of two
// equal keys silently, so a client whose parser keeps the first would evaluate another
// document than the server. Malformed JSON is refused here as well.
func refuseDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // a number beyond float64 is for decodeFloat to refuse by name
	var frames []*objectFrame
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return filterError("", "is not valid JSON")
		}
		delimiter, isDelimiter := token.(json.Delim)
		switch {
		case isDelimiter && (delimiter == '}' || delimiter == ']'):
			frames = frames[:max(len(frames)-1, 0)]
		case isDelimiter:
			openFrame(frames).sawValue()
			frames = append(frames, &objectFrame{keys: objectKeys(delimiter), expectKey: true})
		default:
			if err := openFrame(frames).sawToken(token); err != nil {
				return err
			}
		}
	}
}

func openFrame(frames []*objectFrame) *objectFrame {
	if len(frames) == 0 {
		return nil
	}
	return frames[len(frames)-1]
}

// objectFrame tracks one open object (or array: keys nil) of the token walk.
type objectFrame struct {
	keys      map[string]bool
	expectKey bool
}

func objectKeys(delimiter json.Delim) map[string]bool {
	if delimiter == '{' {
		return map[string]bool{}
	}
	return nil
}

func (f *objectFrame) sawValue() {
	if f != nil && f.keys != nil {
		f.expectKey = true
	}
}

func (f *objectFrame) sawToken(token json.Token) error {
	if f == nil || f.keys == nil || !f.expectKey {
		f.sawValue()
		return nil
	}
	key, _ := token.(string)
	if f.keys[key] {
		return filterError("", "duplicate key %q", key)
	}
	f.keys[key] = true
	f.expectKey = false
	return nil
}
