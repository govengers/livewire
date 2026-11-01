// Package jsonfield reads the fields of a struct type the way encoding/json marshals them,
// for the declarations of the root package and the generators.
package jsonfield

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
)

// Field is one field encoding/json marshals: its JSON key, the struct field (its Index the
// path from the outer type, as reflect's FieldByIndex takes it) and the tag's options.
// OmitEmpty and OmitZero are the `omitempty` and `omitzero` options; Quoted is the `string`
// option on a field it applies to (a bool, number or string, or a pointer to one), whose
// value is written inside a JSON string.
type Field struct {
	Name        string
	StructField reflect.StructField
	OmitEmpty   bool
	OmitZero    bool
	Quoted      bool
}

// Of returns the fields encoding/json marshals for a struct type, in its order (by index
// path), following its typeFields: one embedding level at a time, a type visited once (the
// shallowest occurrence), a type embedded twice at one level cancels its fields, embedded
// structs (by value or pointer) flatten unless their tag names them, a tagged field beats
// untagged ones at the same depth, and a name that stays ambiguous is dropped. A type that
// is not a struct has no fields.
func Of(structType reflect.Type) []Field {
	if structType.Kind() != reflect.Struct {
		return nil
	}
	var fields []Field
	for name, candidates := range collect(structType) {
		if winner, ok := dominant(candidates); ok {
			fields = append(fields, describe(name, winner))
		}
	}
	slices.SortFunc(fields, func(left, right Field) int {
		return slices.Compare(left.StructField.Index, right.StructField.Index)
	})
	return fields
}

// ByName is Of as a map from the JSON key to the struct field.
func ByName(structType reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for _, field := range Of(structType) {
		fields[field.Name] = field.StructField
	}
	return fields
}

func describe(name string, structField reflect.StructField) Field {
	_, options, _ := strings.Cut(structField.Tag.Get("json"), ",")
	field := Field{Name: name, StructField: structField}
	for option := range strings.SplitSeq(options, ",") {
		switch option {
		case "omitempty":
			field.OmitEmpty = true
		case "omitzero":
			field.OmitZero = true
		case "string":
			field.Quoted = quotable(structField.Type)
		}
	}
	return field
}

// quotable reports whether the `string` option applies to a field of the type: encoding/json
// quotes bools, numbers and strings, behind one unnamed pointer level.
func quotable(fieldType reflect.Type) bool {
	if fieldType.Name() == "" && fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	switch fieldType.Kind() {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

type candidate struct {
	field  reflect.StructField
	depth  int
	tagged bool
}

// embeddedLevel is a struct type on one embedding level with the index path that reaches
// it from the outer type.
type embeddedLevel struct {
	structType reflect.Type
	depth      int
	index      []int
}

// collect walks the embedding levels breadth first and returns every candidate by JSON
// name; a type that occurs twice on one level has its candidates listed twice so the
// dominance rule drops them, like encoding/json.
func collect(structType reflect.Type) map[string][]candidate {
	collector := &collector{byName: map[string][]candidate{}, visited: map[reflect.Type]bool{}}
	current := []embeddedLevel{{structType: structType, depth: 0}}
	count := map[reflect.Type]int{structType: 1}
	for len(current) > 0 {
		current, count = collector.collectLevel(current, count)
	}
	return collector.byName
}

type collector struct {
	byName  map[string][]candidate
	visited map[reflect.Type]bool
}

// collectLevel records the named fields of one embedding level and returns the next level
// with how often each type occurs on it.
func (c *collector) collectLevel(current []embeddedLevel, count map[reflect.Type]int) ([]embeddedLevel, map[reflect.Type]int) {
	var next []embeddedLevel
	nextCount := map[reflect.Type]int{}
	for _, level := range current {
		if c.visited[level.structType] {
			continue
		}
		c.visited[level.structType] = true
		for position := range level.structType.NumField() {
			field := level.structType.Field(position)
			field.Index = append(slices.Clone(level.index), position)
			embeddedType := field.Type
			if embeddedType.Kind() == reflect.Pointer {
				embeddedType = embeddedType.Elem() // an embedded field is T or *T, never deeper
			}
			switch classify(field, embeddedType) {
			case skipped:
			case flattens:
				nextCount[embeddedType]++
				if nextCount[embeddedType] == 1 {
					next = append(next, embeddedLevel{structType: embeddedType, depth: level.depth + 1, index: field.Index})
				}
			case named:
				c.record(field, level.depth, count[level.structType])
			}
		}
	}
	return next, nextCount
}

// record lists the candidate once per occurrence of its struct on the level, so a struct
// embedded twice cancels its fields in the dominance rule.
func (c *collector) record(field reflect.StructField, depth, occurrences int) {
	name, tagged, _ := Name(field)
	found := candidate{field: field, depth: depth, tagged: tagged}
	for range max(occurrences, 1) {
		c.byName[name] = append(c.byName[name], found)
	}
}

type class int

const (
	skipped class = iota
	flattens
	named
)

func classify(field reflect.StructField, embeddedType reflect.Type) class {
	_, tagged, included := Name(field)
	embeddedStruct := field.Anonymous && embeddedType.Kind() == reflect.Struct
	switch {
	case !included:
		return skipped
	case embeddedStruct && !tagged:
		return flattens
	case field.IsExported() || embeddedStruct:
		return named
	default:
		return skipped
	}
}

func dominant(candidates []candidate) (reflect.StructField, bool) {
	shallowest := slices.MinFunc(candidates, func(left, right candidate) int { return left.depth - right.depth })
	var atDepth, tagged []candidate
	for _, found := range candidates {
		if found.depth == shallowest.depth {
			atDepth = append(atDepth, found)
			if found.tagged {
				tagged = append(tagged, found)
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

// Name is the field's JSON name, whether the tag names it and whether encoding/json
// marshals the field at all (`json:"-"` does not). A tag with options only
// (`json:",omitempty"`) does not name the field. The name is read back from encoding/json
// itself (a one-field struct with the field's tag is marshaled), so it follows the
// toolchain's own tag rules: which names it accepts, where it cuts an odd one, when it falls
// back to the Go name.
func Name(field reflect.StructField) (name string, tagged, included bool) {
	tagName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if tagName == "" {
		return field.Name, false, true
	}
	key, included := probeKey(field.Tag, probeField)
	if !included {
		return "", false, false
	}
	if key == probeField {
		// Either the tag names the field "Probe" or encoding/json fell back to the Go name: a
		// second probe under another Go name tells them apart.
		if other, _ := probeKey(field.Tag, otherProbeField); other == otherProbeField {
			return field.Name, false, true
		}
	}
	return key, true, true
}

const (
	probeField      = "Probe"
	otherProbeField = "OtherProbe"
)

var stringType = reflect.TypeFor[string]()

// probeKey marshals a one-field struct named goName carrying tag and returns the key
// encoding/json wrote, or false when it wrote none.
func probeKey(tag reflect.StructTag, goName string) (string, bool) {
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
