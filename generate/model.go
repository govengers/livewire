package generate

import (
	"fmt"
	"reflect"
	"slices"
	"unicode"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

// model is what the emitters render: every read, mutation, upload and export of the
// registry with its parameters and shapes, the filterable resources and the named types.
type model struct {
	basePath  string
	types     *typeSet
	resources []*resourceModel
	history   *resourceModel
	reads     []*readModel
	mutations []*mutationModel
	uploads   []*uploadModel
	exports   []*exportModel
}

// parameterModel is one parameter of a read, mutation or upload on the wire; pointer says
// that its Go field is a pointer, so the Go client sends a set zero value.
type parameterModel struct {
	name     string
	in       livewire.ParameterIn
	shape    *shape
	required bool
	pointer  bool
}

// reservedModel says which reserved parameters of a filterable read (D11-D15, D35, D38) a
// read takes: the filter document and sort of filters (nil for none), limit and offset,
// after (cursor mode), field (distinct) and format (export).
type reservedModel struct {
	filters *resourceModel
	sort    bool
	page    bool
	after   bool
	field   bool
	format  bool
}

// readModel is one read; item is the Go type of its item (of a list response, a version,
// or the object), which the Go client imports.
type readModel struct {
	declaration string
	kind        livewire.ReadKind
	path        string
	queryType   string
	parameters  []parameterModel
	reserved    reservedModel
	data        *shape
	item        reflect.Type
	list        bool
	live        bool
}

// mutationModel is one mutation; requestType and responseType are the Go types of its body
// and response (nil for none), which the Go client imports.
type mutationModel struct {
	name           string
	method         string
	path           string
	parametersType string
	parameters     []parameterModel
	body           *shape
	response       *shape
	requestType    reflect.Type
	responseType   reflect.Type
	status         int
	effect         livewire.Effect
	clientID       livewire.ClientIDVersion
}

type uploadModel struct {
	name           string
	path           string
	parametersType string
	parameters     []parameterModel
	response       *shape
	responseType   reflect.Type
	status         int
	contentTypes   []string
	maximumBytes   int64
}

type exportModel struct {
	declaration      string
	path             string
	urlFunction      string
	downloadFunction string
	queryType        string
	reserved         reservedModel
}

// resourceModel is the filter vocabulary of one filterable read: a resource's fields, or
// HistoryFields for every history read (history set). Name is the prefix of its generated
// types.
type resourceModel struct {
	name    string
	target  string
	fields  query.Fields
	history bool
}

// condition is one member of a resource's condition union: a filterable field, the operators
// that take the same operand, and that operand (D12).
type condition struct {
	field     string
	operators []query.Operator
	operand   *shape
}

func (r *resourceModel) searchable() bool {
	return slices.ContainsFunc(r.fields, func(field query.Field) bool { return field.Search })
}

func (r *resourceModel) sortFields() []string {
	var names []string
	for _, field := range r.fields {
		if field.Sort {
			names = append(names, field.Name)
		}
	}
	return names
}

// distinctFields are the fields a distinct read may name: filterable scalars (D15).
func (r *resourceModel) distinctFields() []string {
	var names []string
	for _, field := range r.fields {
		if field.Filter && field.Type != query.TypeStringArray && field.Type != query.TypeJSON {
			names = append(names, field.Name)
		}
	}
	return names
}

// conditions are the members of the condition union, per filterable field one per operand
// its operators take (query.OperatorsOf, in its order): one value, a list of values, a flag.
func (r *resourceModel) conditions() []condition {
	var conditions []condition
	for _, field := range r.fields {
		if !field.Filter {
			continue
		}
		var scalar, list, flag []query.Operator
		for _, operator := range query.OperatorsOf(field.Type) {
			switch operator {
			case query.OperatorIn, query.OperatorNotIn, query.OperatorHasAny, query.OperatorHasAll:
				list = append(list, operator)
			case query.OperatorIsNull, query.OperatorIsEmpty:
				flag = append(flag, operator)
			default:
				scalar = append(scalar, operator)
			}
		}
		operand := filterOperand(field)
		operands := []*shape{operand, {kind: shapeArray, element: operand, minimumItems: 1, maximumItems: query.MaximumListValues}, {kind: shapeBoolean}}
		for group, operators := range [][]query.Operator{scalar, list, flag} {
			if len(operators) > 0 {
				conditions = append(conditions, condition{field: field.Name, operators: operators, operand: operands[group]})
			}
		}
	}
	return conditions
}

func (r *resourceModel) conditionType() string { return r.name + "Condition" }
func (r *resourceModel) filterType() string    { return r.name + "Filter" }
func (r *resourceModel) nodeType() string      { return r.name + "FilterNode" }

// filterOperand is the value a condition on the field compares with (D12): a time may be
// relative, a string is bounded, an array field compares its elements. It is the one model
// both emitters render; a parameter's value (fieldValueShape) is a different thing.
func filterOperand(field query.Field) *shape {
	switch field.Type {
	case query.TypeInteger:
		return &shape{kind: shapeInteger, format: "int64"}
	case query.TypeFloat:
		return &shape{kind: shapeNumber, format: "double"}
	case query.TypeBoolean:
		return &shape{kind: shapeBoolean}
	case query.TypeTime:
		return &shape{kind: shapeTimeValue}
	case query.TypeUUID:
		return &shape{kind: shapeString, format: "uuid"}
	case query.TypeEnum:
		return &shape{kind: shapeEnum, values: field.Enum}
	case query.TypeString, query.TypeText, query.TypeStringArray, query.TypeJSON:
	}
	return &shape{kind: shapeString, maximumLength: query.MaximumStringBytes}
}

var (
	noBodyType    = reflect.TypeFor[livewire.NoBody]()
	noContentType = reflect.TypeFor[livewire.NoContent]()
)

const historyPrefix = "History"

func newModel(registry *livewire.Registry, options Options) (*model, error) {
	types := newTypeSet(options.Enums)
	built := &model{basePath: registry.BasePath(), types: types}
	declarations := registry.Declarations()
	// The guarded fields first: a type is described once, on first sight.
	for _, declaration := range declarations {
		guardDeclaration(types, declaration)
	}
	if err := types.describeLibrary(); err != nil {
		return nil, err
	}
	filters := map[string]*resourceModel{}
	for _, declaration := range declarations {
		if resource, ok := declaration.(*livewire.Resource); ok {
			filters[resource.Name()] = &resourceModel{name: pascalCase(resource.Name()), target: resource.Name(), fields: resource.Fields()}
			built.resources = append(built.resources, filters[resource.Name()])
			if resource.HistoryPath() != "" && built.history == nil {
				built.history = &resourceModel{name: historyPrefix, fields: livewire.HistoryFields(), history: true}
			}
		}
	}
	for _, resource := range slices.Concat(built.resources, []*resourceModel{built.history}) {
		if resource != nil {
			for _, name := range []string{resource.filterType(), resource.nodeType(), resource.conditionType()} {
				types.reserved[name] = true
			}
		}
	}
	for _, declaration := range declarations {
		if err := built.add(declaration, filters[declaration.Name()], options.Presence); err != nil {
			return nil, fmt.Errorf("generate: %s: %w", declaration.Name(), err)
		}
	}
	if err := types.assignNames(); err != nil {
		return nil, err
	}
	return built, nil
}

func guardDeclaration(types *typeSet, declaration livewire.Declaration) {
	switch typed := declaration.(type) {
	case *livewire.Resource:
		var guarded []string
		for _, field := range typed.Fields() {
			if field.Read != nil {
				guarded = append(guarded, field.Name)
			}
		}
		types.guard(typed.ItemType(), guarded)
	case *livewire.List:
		types.guard(typed.ItemType(), fieldNames(typed.Fields()))
	case *livewire.Object:
		types.guard(typed.ItemType(), fieldNames(typed.Fields()))
	}
}

func fieldNames(fields query.Fields) []string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	return names
}

func (m *model) add(declaration livewire.Declaration, filters *resourceModel, presence bool) error {
	switch typed := declaration.(type) {
	case *livewire.Resource:
		return m.addResource(typed, filters, presence)
	case *livewire.List:
		return m.addList(typed)
	case *livewire.Object:
		return m.addObject(typed, presence)
	case *livewire.Mutation:
		return m.addMutation(typed)
	case *livewire.Upload:
		return m.addUpload(typed)
	default:
		return fmt.Errorf("the declaration kind %s is unknown to the generator", declaration.Kind())
	}
}

func (m *model) addResource(resource *livewire.Resource, filters *resourceModel, presence bool) error {
	item, err := m.types.shapeOf(resource.ItemType(), use{})
	if err != nil {
		return err
	}
	name, live := resource.Name(), len(resource.Tables()) > 0
	m.reads = append(m.reads, &readModel{
		declaration: name, kind: livewire.ReadMain, path: resource.Path(), queryType: pascalCase(name) + "ListQuery",
		reserved: reservedModel{filters: filters, sort: true, page: true, after: resource.Cursor()},
		data:     &shape{kind: shapeListResponse, element: item}, item: resource.ItemType(), list: true, live: live,
	})
	key := itemKeyParameter(resource)
	if key != nil {
		m.reads = append(m.reads, &readModel{
			declaration: name, kind: livewire.ReadItem, path: resource.ItemPath(), queryType: pascalCase(name) + "ItemParameters",
			parameters: []parameterModel{*key}, data: item, item: resource.ItemType(), live: live,
		})
	}
	if resource.DistinctPath() != "" {
		distinct, err := m.types.shapeOf(reflect.TypeFor[livewire.DistinctValue](), use{})
		if err != nil {
			return err
		}
		m.reads = append(m.reads, &readModel{
			declaration: name, kind: livewire.ReadDistinct, path: resource.DistinctPath(), queryType: pascalCase(name) + "DistinctQuery",
			reserved: reservedModel{filters: filters, page: true, field: true},
			data:     &shape{kind: shapeArray, element: distinct}, item: reflect.TypeFor[livewire.DistinctValue](), live: live,
		})
	}
	if resource.HistoryPath() != "" {
		m.reads = append(m.reads, &readModel{
			declaration: name, kind: livewire.ReadHistory, path: resource.HistoryPath(), queryType: pascalCase(name) + "HistoryQuery",
			parameters: []parameterModel{*key}, reserved: reservedModel{filters: m.history, sort: true, page: true},
			data: &shape{kind: shapeListResponse, element: &shape{kind: shapeVersion, element: item}}, item: resource.ItemType(), list: true, live: live,
		})
	}
	if presence && key != nil {
		if err := m.addPresence(name, resource.PresencePath(), nil); err != nil {
			return err
		}
	}
	if resource.ExportPath() != "" {
		m.exports = append(m.exports, &exportModel{
			declaration: name, path: resource.ExportPath(), queryType: pascalCase(name) + "ExportQuery",
			urlFunction: lowerFirst(pascalCase(name)) + "ExportURL", downloadFunction: "download" + pascalCase(name) + "Export",
			reserved: reservedModel{filters: filters, sort: true, format: true},
		})
	}
	return nil
}

// itemKeyParameter is the path parameter of a resource's item read, typed by its field.
func itemKeyParameter(resource *livewire.Resource) *parameterModel {
	key := resource.Item()
	if key == nil {
		return nil
	}
	field, _ := resource.Fields().Lookup(key.Field)
	return &parameterModel{name: key.Parameter, in: livewire.InPath, shape: fieldValueShape(field), required: true}
}

// fieldValueShape is the JSON shape of one value of a declared field as a parameter carries
// it: an absolute time, a uuid, an enum value.
func fieldValueShape(field query.Field) *shape {
	switch field.Type {
	case query.TypeInteger:
		return &shape{kind: shapeInteger, format: "int64"}
	case query.TypeTime:
		return &shape{kind: shapeString, format: "date-time"}
	case query.TypeUUID:
		return &shape{kind: shapeString, format: "uuid"}
	case query.TypeEnum:
		return &shape{kind: shapeEnum, values: field.Enum}
	case query.TypeString, query.TypeText, query.TypeFloat, query.TypeBoolean, query.TypeStringArray, query.TypeJSON:
	}
	return &shape{kind: shapeString} // an item key is uuid, string, text, enum, int or time (ItemKey)
}

// typedRead is the parameters and the item shape of a typed-parameters list or object.
func (m *model) typedRead(declaration livewire.Declaration, itemType reflect.Type) ([]parameterModel, *shape, error) {
	item, err := m.types.shapeOf(itemType, use{})
	return m.parametersOf(declaration.Parameters()), item, err
}

func (m *model) addList(list *livewire.List) error {
	parameters, item, err := m.typedRead(list, list.ItemType())
	if err != nil {
		return err
	}
	m.reads = append(m.reads, &readModel{
		declaration: list.Name(), kind: livewire.ReadMain, path: list.Path(), queryType: pascalCase(list.Name()) + "Parameters",
		parameters: parameters, data: &shape{kind: shapeListResponse, element: item}, item: list.ItemType(), list: true, live: len(list.Tables()) > 0,
	})
	return nil
}

func (m *model) addObject(object *livewire.Object, presence bool) error {
	parameters, item, err := m.typedRead(object, object.ItemType())
	if err != nil {
		return err
	}
	m.reads = append(m.reads, &readModel{
		declaration: object.Name(), kind: livewire.ReadMain, path: object.Path(), queryType: pascalCase(object.Name()) + "Parameters",
		parameters: parameters, data: item, item: object.ItemType(), live: len(object.Tables()) > 0,
	})
	if !presence {
		return nil
	}
	return m.addPresence(object.Name(), object.PresencePath(), parameters)
}

// addPresence adds the presence read of a record (D37): the declaration's own parameters and
// the key. It is always live: the presence table re-runs it. An object's own query parameter
// `key` is the key, required here; a `key` of another kind could not be told from it and is
// refused.
func (m *model) addPresence(name, path string, parameters []parameterModel) error {
	presence, err := m.types.shapeOf(reflect.TypeFor[livewire.PresenceItem](), use{})
	if err != nil {
		return err
	}
	key := parameterModel{name: livewire.ParameterPresenceKey, in: livewire.InQuery, shape: &shape{kind: shapeString}, required: true}
	merged := slices.Clone(parameters)
	position := slices.IndexFunc(merged, func(parameter parameterModel) bool { return parameter.name == livewire.ParameterPresenceKey })
	switch {
	case position < 0:
		merged = append(merged, key)
	case merged[position].in == livewire.InQuery && (merged[position].shape.kind == shapeString || merged[position].shape.kind == shapeEnum):
		merged[position].required = true
	default:
		return fmt.Errorf("the parameter %q is the presence key (D37), so it must be a string query parameter", livewire.ParameterPresenceKey)
	}
	m.reads = append(m.reads, &readModel{
		declaration: name, kind: livewire.ReadPresence, path: path, queryType: pascalCase(name) + "PresenceParameters",
		parameters: merged, data: &shape{kind: shapeListResponse, element: presence}, item: reflect.TypeFor[livewire.PresenceItem](), list: true, live: true,
	})
	return nil
}

func (m *model) addMutation(mutation *livewire.Mutation) error {
	added := &mutationModel{
		name: mutation.Name(), method: mutation.Method(), path: mutation.Path(), parametersType: pascalCase(mutation.Name()) + "Parameters",
		parameters: m.parametersOf(mutation.Parameters()), status: mutation.Status(), effect: mutation.Effect(), clientID: mutation.ClientID(),
	}
	var err error
	if mutation.RequestType() != noBodyType {
		added.requestType = mutation.RequestType()
		if added.body, err = m.types.shapeOf(mutation.RequestType(), use{}); err != nil {
			return err
		}
	}
	if mutation.ResponseType() != noContentType {
		added.responseType = mutation.ResponseType()
		if added.response, err = m.types.shapeOf(mutation.ResponseType(), use{}); err != nil {
			return err
		}
	}
	m.mutations = append(m.mutations, added)
	return nil
}

func (m *model) addUpload(upload *livewire.Upload) error {
	response, err := m.types.shapeOf(upload.ResponseType(), use{})
	if err != nil {
		return err
	}
	m.uploads = append(m.uploads, &uploadModel{
		name: upload.Name(), path: upload.Path(), parametersType: pascalCase(upload.Name()) + "Parameters",
		parameters: m.parametersOf(upload.Parameters()), response: response, responseType: upload.ResponseType(),
		status: upload.Status(), contentTypes: upload.ContentTypes(), maximumBytes: upload.MaximumBytes(),
	})
	return nil
}

// parametersOf types the parameters of a parameters struct: a list is an array of its
// scalars, a pointer is the value it points to; enum values come from the tag or from the
// Enums option for a named string type.
func (m *model) parametersOf(parameters []livewire.Parameter) []parameterModel {
	result := make([]parameterModel, 0, len(parameters))
	for _, parameter := range parameters {
		scalarType := parameter.Type
		if parameter.List {
			scalarType = scalarType.Elem()
		}
		pointer := scalarType.Kind() == reflect.Pointer
		if pointer {
			scalarType = scalarType.Elem()
		}
		scalar := m.scalarShape(scalarType, parameter.Enum)
		if parameter.List {
			scalar = &shape{kind: shapeArray, element: scalar}
		}
		result = append(result, parameterModel{name: parameter.Name, in: parameter.In, shape: scalar, required: parameter.Required(), pointer: pointer && !parameter.List})
	}
	return result
}

func (m *model) scalarShape(scalarType reflect.Type, enum []string) *shape {
	if values, ok := m.types.enums[scalarType]; ok && enum == nil {
		enum = values
	}
	switch {
	case enum != nil:
		return &shape{kind: shapeEnum, values: enum}
	case scalarType == uuidType:
		return &shape{kind: shapeString, format: "uuid"}
	case scalarType == timeType:
		return &shape{kind: shapeString, format: "date-time"}
	case scalarType.Kind() == reflect.Bool:
		return &shape{kind: shapeBoolean}
	case scalarType.Kind() == reflect.String:
		return &shape{kind: shapeString}
	default:
		return &shape{kind: shapeInteger, format: "int64"}
	}
}

// targetName is the socket target of a read (D46).
func (r *readModel) targetName() string {
	if r.kind == livewire.ReadMain {
		return r.declaration
	}
	return r.declaration + "." + string(r.kind)
}

func (r *readModel) functionName() string {
	suffix := map[livewire.ReadKind]string{
		livewire.ReadMain: "", livewire.ReadItem: "Item", livewire.ReadDistinct: "Distinct",
		livewire.ReadHistory: "History", livewire.ReadPresence: "Presence",
	}[r.kind]
	return "get" + pascalCase(r.declaration) + suffix
}

// pathParameters are the parameters a REST path carries.
func pathParameters(parameters []parameterModel) []string {
	var names []string
	for _, parameter := range parameters {
		if parameter.in == livewire.InPath {
			names = append(names, parameter.name)
		}
	}
	return names
}

func lowerFirst(text string) string {
	runes := []rune(text)
	if len(runes) > 0 {
		runes[0] = unicode.ToLower(runes[0])
	}
	return string(runes)
}
