package query

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Page is one page of a list. Offset mode pages by Limit and Offset; cursor mode (D38)
// sets After, the cursor of the last row of the previous page, and ignores Offset.
type Page struct {
	Limit  int
	Offset int
	After  *Cursor
}

// ParsePage reads the limit and offset parameters. An empty limit means maximumLimit, a
// larger one is clamped to it (D14); an empty offset means 0. A negative or unparsable
// value is refused with an *Error of CodeInvalidPage. maximumLimit must be positive.
func ParsePage(limit, offset string, maximumLimit int) (Page, error) {
	page := Page{Limit: maximumLimit}
	if limit != "" {
		parsed, err := parseNonNegative(limit, "limit")
		if err != nil {
			return Page{}, err
		}
		page.Limit = min(parsed, maximumLimit)
	}
	if offset != "" {
		parsed, err := parseNonNegative(offset, "offset")
		if err != nil {
			return Page{}, err
		}
		page.Offset = parsed
	}
	return page, nil
}

func parseNonNegative(raw, path string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, newError(CodeInvalidPage, path, "%q is not a whole number", raw)
	}
	if value < 0 {
		return 0, newError(CodeInvalidPage, path, "must not be negative")
	}
	return value, nil
}

// Cursor marks the last row of a page in cursor mode: the sort it was taken under, the
// row's value of every sort term (nil for NULL) and its id, a uuid. ListQuery.NextCursor
// builds one from the page query's columns; Encode makes it opaque for the client.
type Cursor struct {
	Sort   []SortTerm
	Values []any
	ID     string
}

type cursorWire struct {
	Sort   string            `json:"sort"`
	Values []json.RawMessage `json:"values"`
	ID     string            `json:"id"`
}

// Encode returns the cursor as URL-safe base64 of its JSON form. Values are the row's
// values as a driver returns them: a time.Time, an integer or float of any width, a string,
// a bool, or a uuid as [16]byte, which is written in its hyphenated form. A float32 is
// widened exactly, a non-finite float is written as the string PostgreSQL spells it (NaN,
// Infinity, -Infinity), a timestamptz infinity is the string "infinity" or "-infinity"
// (NextCursor). A time outside the years 0 to 9999 in UTC has no RFC 3339 form and is
// refused. The error of a value encoding/json cannot write is reported by message only; it
// is not part of the API.
func (c *Cursor) Encode() (string, error) {
	values := make([]json.RawMessage, 0, len(c.Values))
	for _, value := range c.Values {
		if moment, isTime := value.(time.Time); isTime && !yearInRange(moment.UTC()) {
			return "", fmt.Errorf("encode cursor value %v: the year is outside %d to %d in UTC", value, minimumYear, maximumYear)
		}
		encoded, err := json.Marshal(wireValue(value))
		if err != nil {
			return "", fmt.Errorf("encode cursor value %v: %s", value, err.Error())
		}
		values = append(values, encoded)
	}
	encoded, err := json.Marshal(cursorWire{Sort: formatSort(c.Sort), Values: values, ID: c.ID})
	if err != nil {
		return "", fmt.Errorf("encode cursor: %s", err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func wireValue(value any) any {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC()
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x", typed[0:4], typed[4:6], typed[6:8], typed[8:10], typed[10:16])
	case float32:
		return wireValue(float64(typed))
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nonFiniteSpelling(typed)
		}
		return typed
	default:
		return value
	}
}

// nonFiniteSpelling is PostgreSQL's input form of a non-finite float8.
func nonFiniteSpelling(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case value > 0:
		return "Infinity"
	default:
		return "-Infinity"
	}
}

// ParseCursor decodes an encoded cursor against the resource's fields: the sort it carries
// must parse (ParseSort), every value must be of its field's type or null, and the id must
// be a uuid. Values are column contents, not document values, so the D12 limits do not
// apply to them: any string length, any int64, the non-finite floats and infinite times of
// Encode, and any enum text (a retired value is still a row's value; a forged value on a
// native PostgreSQL enum column fails in the database with SQLSTATE 22P02, which the loader
// maps to CodeInvalidCursor in cursor mode). Compile then refuses the cursor if the
// request's sort differs. Every refusal is an *Error with CodeInvalidCursor at path "after".
func ParseCursor(fields Fields, encoded string) (*Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, cursorError("is not a cursor")
	}
	var wire cursorWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, cursorError("is not a cursor")
	}
	sort, err := ParseSort(fields, wire.Sort)
	if err != nil {
		return nil, cursorError("names a sort this resource does not have")
	}
	if len(wire.Values) != len(sort) {
		return nil, cursorError("does not match its sort")
	}
	id, err := decodeUUID(wire.ID, "after")
	if err != nil {
		return nil, cursorError("carries an id that is not a uuid")
	}
	values := make([]any, 0, len(sort))
	for index, term := range sort {
		field, _ := fields.Lookup(term.Field)
		value, err := decodeCursorValue(field, wire.Values[index])
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return &Cursor{Sort: sort, Values: values, ID: id.(string)}, nil
}

func (c *Cursor) matches(sort []SortTerm) bool {
	return slices.Equal(c.Sort, sort) && len(c.Values) == len(sort)
}

func cursorError(message string) *Error {
	return newError(CodeInvalidCursor, "after", "%s", message)
}

// decodeCursorValue reads one sort value in the domain of the column, not of a document:
// any string length, any int64, a float, a bool, an RFC 3339 time, a uuid.
func decodeCursorValue(field Field, raw json.RawMessage) (any, error) {
	decoded, err := decodeJSON(raw)
	if err != nil {
		return nil, cursorError("is not a cursor")
	}
	if decoded == nil {
		return nil, nil //nolint:nilnil // a null value is a NULL sort value, a valid cursor position
	}
	if text, isString := decoded.(string); isString && strings.ContainsRune(text, 0) {
		return nil, cursorError("carries a string with U+0000")
	}
	var value any
	var ok bool
	switch field.Type {
	case TypeString, TypeText, TypeEnum:
		value, ok = decoded.(string)
	case TypeBoolean:
		value, ok = decoded.(bool)
	case TypeInteger:
		value, ok = cursorInteger(decoded)
	case TypeFloat:
		value, ok = cursorFloat(decoded)
	case TypeTime:
		value, ok = cursorTime(decoded)
	case TypeUUID:
		value, ok = cursorUUID(decoded)
	case TypeStringArray, TypeJSON:
		ok = false
	}
	if !ok {
		return nil, cursorError(fmt.Sprintf("carries a value that is not of type %s", field.Type))
	}
	return value, nil
}

func cursorInteger(decoded any) (any, bool) {
	number, ok := decoded.(json.Number)
	if !ok {
		return nil, false
	}
	value, err := number.Int64()
	return value, err == nil
}

// cursorFloat reads a number, or one of the non-finite spellings Encode writes.
func cursorFloat(decoded any) (any, bool) {
	if text, isString := decoded.(string); isString {
		value, err := strconv.ParseFloat(text, 64)
		return value, err == nil && (math.IsNaN(value) || math.IsInf(value, 0))
	}
	number, ok := decoded.(json.Number)
	if !ok {
		return nil, false
	}
	value, err := number.Float64()
	return value, err == nil
}

// cursorTime reads an RFC 3339 time, or "infinity" and "-infinity", which stay strings and
// bind as PostgreSQL's infinite timestamps.
func cursorTime(decoded any) (any, bool) {
	text, ok := decoded.(string)
	if !ok {
		return nil, false
	}
	if text == "infinity" || text == "-infinity" {
		return text, true
	}
	value, err := time.Parse(time.RFC3339Nano, text)
	return value.UTC(), err == nil && yearInRange(value.UTC())
}

func cursorUUID(decoded any) (any, bool) {
	value, err := decodeUUID(decoded, "after")
	return value, err == nil
}
