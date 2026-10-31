package livewire

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ParameterSource hands the decoder the raw values of a request: path values by name and
// query values by name, where a query key may repeat. The REST adapter builds one from the
// route, the engine from a RequestFrame's Params (MapSource).
type ParameterSource interface {
	Path(name string) (string, bool)
	Query(name string) ([]string, bool)
}

// MapSource is a ParameterSource over one value per name, as a socket frame carries them:
// path and query parameters read the same map, a list parameter is one comma-separated
// value.
type MapSource map[string]string

// Path returns the value under name.
func (s MapSource) Path(name string) (string, bool) {
	value, ok := s[name]
	return value, ok
}

// Query returns the value under name as a one-element list.
func (s MapSource) Query(name string) ([]string, bool) {
	value, ok := s[name]
	if !ok {
		return nil, false
	}
	return []string{value}, true
}

// ParameterIn is where a parameter travels.
type ParameterIn string

// The parameter locations; they are the struct tags of a parameters struct.
const (
	InPath  ParameterIn = "path"
	InQuery ParameterIn = "query"
)

// Parameter describes one parameter of a declaration for the generators: its name on the
// wire, where it travels, the Go type of its struct field (pointer and slice types as
// declared), the enum values of an `enum:"a,b"` tag, and whether the wire carries a list.
// A path parameter is required, a query parameter optional.
type Parameter struct {
	Name string
	In   ParameterIn
	Type reflect.Type
	Enum []string
	List bool
}

// Required reports whether a request must carry the parameter.
func (p Parameter) Required() bool {
	return p.In == InPath
}

// NoParameters is the parameters type of a declaration without parameters.
type NoParameters struct{}

var (
	uuidType = reflect.TypeFor[uuid.UUID]()
	timeType = reflect.TypeFor[time.Time]()
)

type parameterField struct {
	Parameter
	index []int
}

// parameterSpec is a compiled parameters struct: decode fills a new value of its type.
type parameterSpec struct {
	parametersType reflect.Type
	fields         []parameterField
}

// compileParameters validates a parameters struct and the path it serves: every tagged
// field has a supported type, every `{name}` of the path has a path field and the other way
// round. The error is a declaration mistake.
func compileParameters(parametersType reflect.Type, path string) (*parameterSpec, error) {
	if parametersType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("parameters type %s is not a struct", parametersType)
	}
	spec := &parameterSpec{parametersType: parametersType}
	if err := spec.collect(parametersType, nil); err != nil {
		return nil, err
	}
	placeholders, err := pathPlaceholders(path)
	if err != nil {
		return nil, err
	}
	for _, field := range spec.fields {
		if field.In == InPath && !slices.Contains(placeholders, field.Name) {
			return nil, fmt.Errorf("path parameter %q is not in the path %q", field.Name, path)
		}
	}
	for _, placeholder := range placeholders {
		if !spec.hasPath(placeholder) {
			return nil, fmt.Errorf("the path %q has no field tagged path:%q", path, placeholder)
		}
	}
	return spec, nil
}

func (s *parameterSpec) collect(structType reflect.Type, prefix []int) error {
	for position := range structType.NumField() {
		field := structType.Field(position)
		index := append(slices.Clone(prefix), position)
		location, name := parameterTag(field)
		if name == "" {
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				if err := s.collect(field.Type, index); err != nil {
					return err
				}
			}
			continue
		}
		parameter, err := describeParameter(field, location, name)
		if err != nil {
			return err
		}
		if s.lookup(name) != nil {
			return fmt.Errorf("parameter %q is declared twice", name)
		}
		s.fields = append(s.fields, parameterField{Parameter: parameter, index: index})
	}
	return nil
}

func (s *parameterSpec) lookup(name string) *parameterField {
	for position := range s.fields {
		if s.fields[position].Name == name {
			return &s.fields[position]
		}
	}
	return nil
}

func (s *parameterSpec) hasPath(name string) bool {
	field := s.lookup(name)
	return field != nil && field.In == InPath
}

func (s *parameterSpec) parameters() []Parameter {
	parameters := make([]Parameter, 0, len(s.fields))
	for _, field := range s.fields {
		parameter := field.Parameter
		parameter.Enum = slices.Clone(parameter.Enum)
		parameters = append(parameters, parameter)
	}
	return parameters
}

func parameterTag(field reflect.StructField) (ParameterIn, string) {
	if name := field.Tag.Get(string(InPath)); name != "" {
		return InPath, name
	}
	if name := field.Tag.Get(string(InQuery)); name != "" {
		return InQuery, name
	}
	return "", ""
}

func describeParameter(field reflect.StructField, location ParameterIn, name string) (Parameter, error) {
	parameter := Parameter{Name: name, In: location, Type: field.Type}
	if !field.IsExported() {
		return parameter, fmt.Errorf("parameter %q is the unexported field %s", name, field.Name)
	}
	if field.Tag.Get(string(InPath)) != "" && field.Tag.Get(string(InQuery)) != "" {
		return parameter, fmt.Errorf("parameter %q is tagged path and query", name)
	}
	if enum, ok := field.Tag.Lookup("enum"); ok {
		parameter.Enum = strings.Split(enum, ",")
	}
	scalar := field.Type
	if scalar.Kind() == reflect.Slice {
		if location != InQuery || scalar.Elem().Kind() != reflect.String {
			return parameter, fmt.Errorf("parameter %q: a list parameter is a query parameter of string elements, not %s", name, field.Type)
		}
		parameter.List = true
		scalar = scalar.Elem()
	}
	if scalar.Kind() == reflect.Pointer {
		scalar = scalar.Elem()
	}
	if !supportedParameterType(scalar) {
		return parameter, fmt.Errorf("parameter %q has the unsupported type %s", name, field.Type)
	}
	if parameter.Enum != nil && scalar.Kind() != reflect.String {
		return parameter, fmt.Errorf("parameter %q has enum values but is %s, not a string", name, field.Type)
	}
	return parameter, nil
}

func supportedParameterType(scalar reflect.Type) bool {
	if scalar == uuidType || scalar == timeType {
		return true
	}
	switch scalar.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	default:
		return false
	}
}

// pathPlaceholders returns the `{name}` segments of a route path, which must start with a
// slash and have no empty segment; a placeholder fills a whole segment.
func pathPlaceholders(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("the path %q does not start with a slash", path)
	}
	var placeholders []string
	for _, segment := range strings.Split(path[1:], "/") {
		switch {
		case segment == "":
			return nil, fmt.Errorf("the path %q has an empty segment", path)
		case strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") && len(segment) > 2:
			name := segment[1 : len(segment)-1]
			if strings.ContainsAny(name, "{}") || slices.Contains(placeholders, name) {
				return nil, fmt.Errorf("the path %q has a malformed or repeated placeholder %q", path, segment)
			}
			placeholders = append(placeholders, name)
		case strings.ContainsAny(segment, "{}"):
			return nil, fmt.Errorf("the path %q has a malformed placeholder %q", path, segment)
		}
	}
	return placeholders, nil
}

// decode fills a new parameters value from source. A missing or empty path parameter and a
// value of the wrong form answer Invalid naming the parameter; a missing query parameter
// leaves its field zero.
func (s *parameterSpec) decode(source ParameterSource) (any, error) {
	parameters := reflect.New(s.parametersType)
	for _, field := range s.fields {
		values, err := readValues(source, field.Parameter)
		if err != nil {
			return nil, err
		}
		if values == nil {
			continue
		}
		target := parameters.Elem().FieldByIndex(field.index)
		if field.List {
			err = setList(target, field.Parameter, values)
		} else {
			err = setScalar(target, field.Parameter, values[0])
		}
		if err != nil {
			return nil, err
		}
	}
	return parameters.Interface(), nil
}

// readValues returns the raw values of a parameter, nil when it is absent.
func readValues(source ParameterSource, parameter Parameter) ([]string, error) {
	var values []string
	if parameter.In == InPath {
		value, ok := source.Path(parameter.Name)
		if !ok || value == "" {
			return nil, invalidParameter(parameter.Name, "required")
		}
		values = []string{value}
	} else {
		var ok bool
		values, ok = source.Query(parameter.Name)
		if !ok || len(values) == 0 || (len(values) == 1 && values[0] == "") {
			return nil, nil
		}
	}
	return values, nil
}

func setList(target reflect.Value, parameter Parameter, values []string) error {
	list := reflect.MakeSlice(target.Type(), 0, len(values))
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			if item == "" {
				continue
			}
			if err := checkEnum(parameter, item); err != nil {
				return err
			}
			list = reflect.Append(list, reflect.ValueOf(item).Convert(target.Type().Elem()))
		}
	}
	target.Set(list)
	return nil
}

func setScalar(target reflect.Value, parameter Parameter, raw string) error {
	if target.Kind() == reflect.Pointer {
		target.Set(reflect.New(target.Type().Elem()))
		target = target.Elem()
	}
	switch {
	case target.Type() == uuidType:
		id, err := uuid.Parse(raw)
		if err != nil {
			return invalidParameter(parameter.Name, "must be a uuid")
		}
		target.Set(reflect.ValueOf(id))
	case target.Type() == timeType:
		instant, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return invalidParameter(parameter.Name, "must be an RFC 3339 time")
		}
		target.Set(reflect.ValueOf(instant.UTC()))
	case target.Kind() == reflect.String:
		if err := checkEnum(parameter, raw); err != nil {
			return err
		}
		target.SetString(raw)
	case target.Kind() == reflect.Bool:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return invalidParameter(parameter.Name, "must be a boolean")
		}
		target.SetBool(value)
	default:
		value, err := strconv.ParseInt(raw, 10, target.Type().Bits())
		if err != nil {
			return invalidParameter(parameter.Name, "must be an integer")
		}
		target.SetInt(value)
	}
	return nil
}

func checkEnum(parameter Parameter, value string) error {
	if parameter.Enum == nil || slices.Contains(parameter.Enum, value) {
		return nil
	}
	return invalidParameter(parameter.Name, "must be one of "+strings.Join(parameter.Enum, ", "))
}
