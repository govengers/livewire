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

// parameterModel is one parameter of a read, mutation or upload on the wire.
type parameterModel struct {
	name     string
	in       livewire.ParameterIn
	shape    *shape
	required bool
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

type readModel struct {
	declaration string
	kind        livewire.ReadKind
	path        string
	queryType   string
	parameters  []parameterModel
	reserved    reservedModel
	data        *shape
	list        bool
	live        bool
}

type mutationModel struct {
	name           string
	method         string
	path           string
	parametersType string
	parameters     []parameterModel
	body           *shape
	response       *shape
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
// HistoryFields for every history read. Name is the prefix of its generated types.
type resourceModel struct {
	name   string
	target string
	fields query.Fields
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

func (r *resourceModel) conditionType() string { return r.name + "Condition" }
func (r *resourceModel) filterType() string    { return r.name + "Filter" }
func (r *resourceModel) nodeType() string      { return r.name + "FilterNode" }

var (
	noBodyType    = reflect.TypeFor[livewire.NoBody]()
	noContentType = reflect.TypeFor[livewire.NoContent]()
)

const historyPrefix = "History"

func newModel(registry *livewire.Registry, options Options) (*model, error) {
	types, err := newTypeSet(options.Enums)
	if err != nil {
		return nil, err
	}
	built := &model{basePath: registry.BasePath(), types: types}
	declarations := registry.Declarations()
	// The guarded fields first: a type is described once, on first sight.
	for _, declaration := range declarations {
		guardDeclaration(types, declaration)
	}
	for _, declaration := range declarations {
		if resource, ok := declaration.(*livewire.Resource); ok {
			built.resources = append(built.resources, &resourceModel{name: pascalCase(resource.Name()), target: resource.Name(), fields: resource.Fields()})
			if resource.HistoryPath() != "" && built.history == nil {
				built.history = &resourceModel{name: historyPrefix, fields: livewire.HistoryFields()}
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
	for position, declaration := range declarations {
		if err := built.add(declaration, built.resourceAt(declarations, position), options.Presence); err != nil {
			return nil, fmt.Errorf("generate: %s: %w", declaration.Name(), err)
		}
	}
	types.assignNames()
	return built, nil
}

// resourceAt is the resource model of the declaration at position, nil for another kind.
func (m *model) resourceAt(declarations []livewire.Declaration, position int) *resourceModel {
	if _, ok := declarations[position].(*livewire.Resource); !ok {
		return nil
	}
	index := 0
	for _, declaration := range declarations[:position] {
		if _, ok := declaration.(*livewire.Resource); ok {
			index++
		}
	}
	return m.resources[index]
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

func (m *model) add(declaration livewire.Declaration, resource *resourceModel, presence bool) error {
	switch typed := declaration.(type) {
	case *livewire.Resource:
		return m.addResource(typed, resource, presence)
	case *livewire.List:
		return m.addTyped(typed, typed.ItemType(), true, false)
	case *livewire.Object:
		return m.addTyped(typed, typed.ItemType(), false, presence)
	case *livewire.Mutation:
		return m.addMutation(typed)
	case *livewire.Upload:
		return m.addUpload(typed)
	default:
		return fmt.Errorf("the declaration kind %s is unknown to the generator", declaration.Kind())
	}
}

func (m *model) addResource(resource *livewire.Resource, filters *resourceModel, presence bool) error {
	item, err := m.types.shapeOf(resource.ItemType(), nil)
	if err != nil {
		return err
	}
	name, live := resource.Name(), len(resource.Tables()) > 0
	m.reads = append(m.reads, &readModel{
		declaration: name, kind: livewire.ReadMain, path: resource.Path(), queryType: pascalCase(name) + "ListQuery",
		reserved: reservedModel{filters: filters, sort: true, page: true, after: resource.Cursor()},
		data:     &shape{kind: shapeListResponse, element: item}, list: true, live: live,
	})
	key := itemKeyParameter(resource)
	if key != nil {
		m.reads = append(m.reads, &readModel{
			declaration: name, kind: livewire.ReadItem, path: resource.ItemPath(), queryType: pascalCase(name) + "ItemParameters",
			parameters: []parameterModel{*key}, data: item, live: live,
		})
	}
	if resource.DistinctPath() != "" {
		distinct, err := m.types.shapeOf(reflect.TypeFor[livewire.DistinctValue](), nil)
		if err != nil {
			return err
		}
		m.reads = append(m.reads, &readModel{
			declaration: name, kind: livewire.ReadDistinct, path: resource.DistinctPath(), queryType: pascalCase(name) + "DistinctQuery",
			reserved: reservedModel{filters: filters, page: true, field: true},
			data:     &shape{kind: shapeArray, element: distinct}, live: live,
		})
	}
	if resource.HistoryPath() != "" {
		m.reads = append(m.reads, &readModel{
			declaration: name, kind: livewire.ReadHistory, path: resource.HistoryPath(), queryType: pascalCase(name) + "HistoryQuery",
			parameters: []parameterModel{*key}, reserved: reservedModel{filters: m.history, sort: true, page: true},
			data: &shape{kind: shapeListResponse, element: &shape{kind: shapeVersion, element: item}}, list: true, live: live,
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

// fieldValueShape is the JSON shape of one value of a declared field.
func fieldValueShape(field query.Field) *shape {
	switch field.Type {
	case query.TypeInteger:
		return &shape{kind: shapeInteger, format: "int64"}
	case query.TypeFloat:
		return &shape{kind: shapeNumber, format: "double"}
	case query.TypeBoolean:
		return &shape{kind: shapeBoolean}
	case query.TypeTime:
		return &shape{kind: shapeString, format: "date-time"}
	case query.TypeUUID:
		return &shape{kind: shapeString, format: "uuid"}
	case query.TypeEnum:
		return &shape{kind: shapeEnum, values: field.Enum}
	case query.TypeStringArray:
		return &shape{kind: shapeArray, element: &shape{kind: shapeString}}
	case query.TypeJSON:
		return &shape{kind: shapeUnknown}
	case query.TypeString, query.TypeText:
	}
	return &shape{kind: shapeString}
}

// typedRead is a list or an object: its typed parameters and its item type.
type typedRead interface {
	livewire.Declaration
	PresencePath() string
}

func (m *model) addTyped(declaration livewire.Declaration, itemType reflect.Type, list, presence bool) error {
	parameters, err := m.parametersOf(declaration.Parameters())
	if err != nil {
		return err
	}
	item, err := m.types.shapeOf(itemType, nil)
	if err != nil {
		return err
	}
	data := item
	if list {
		data = &shape{kind: shapeListResponse, element: item}
	}
	name := declaration.Name()
	m.reads = append(m.reads, &readModel{
		declaration: name, kind: livewire.ReadMain, path: declaration.Path(), queryType: pascalCase(name) + "Parameters",
		parameters: parameters, data: data, list: list, live: len(declaration.Tables()) > 0,
	})
	if object, ok := declaration.(typedRead); ok && presence {
		return m.addPresence(name, object.PresencePath(), parameters)
	}
	return nil
}

// addPresence adds the presence read of a record (D37): the declaration's own parameters and
// the key. It is always live: the presence table re-runs it.
func (m *model) addPresence(name, path string, parameters []parameterModel) error {
	presence, err := m.types.shapeOf(reflect.TypeFor[livewire.PresenceItem](), nil)
	if err != nil {
		return err
	}
	key := parameterModel{name: livewire.ParameterPresenceKey, in: livewire.InQuery, shape: &shape{kind: shapeString}, required: true}
	m.reads = append(m.reads, &readModel{
		declaration: name, kind: livewire.ReadPresence, path: path, queryType: pascalCase(name) + "PresenceParameters",
		parameters: append(slices.Clone(parameters), key), data: &shape{kind: shapeListResponse, element: presence}, list: true, live: true,
	})
	return nil
}

func (m *model) addMutation(mutation *livewire.Mutation) error {
	parameters, err := m.parametersOf(mutation.Parameters())
	if err != nil {
		return err
	}
	added := &mutationModel{
		name: mutation.Name(), method: mutation.Method(), path: mutation.Path(), parametersType: pascalCase(mutation.Name()) + "Parameters",
		parameters: parameters, status: mutation.Status(), effect: mutation.Effect(), clientID: mutation.ClientID(),
	}
	if mutation.RequestType() != noBodyType {
		if added.body, err = m.types.shapeOf(mutation.RequestType(), nil); err != nil {
			return err
		}
	}
	if mutation.ResponseType() != noContentType {
		if added.response, err = m.types.shapeOf(mutation.ResponseType(), nil); err != nil {
			return err
		}
	}
	m.mutations = append(m.mutations, added)
	return nil
}

func (m *model) addUpload(upload *livewire.Upload) error {
	parameters, err := m.parametersOf(upload.Parameters())
	if err != nil {
		return err
	}
	response, err := m.types.shapeOf(upload.ResponseType(), nil)
	if err != nil {
		return err
	}
	m.uploads = append(m.uploads, &uploadModel{
		name: upload.Name(), path: upload.Path(), parametersType: pascalCase(upload.Name()) + "Parameters",
		parameters: parameters, response: response, status: upload.Status(), contentTypes: upload.ContentTypes(), maximumBytes: upload.MaximumBytes(),
	})
	return nil
}

// parametersOf types the parameters of a parameters struct: a list is an array of its
// scalars, a pointer is the value it points to; enum values come from the tag or from the
// Enums option for a named string type.
func (m *model) parametersOf(parameters []livewire.Parameter) ([]parameterModel, error) {
	result := make([]parameterModel, 0, len(parameters))
	for _, parameter := range parameters {
		scalarType := parameter.Type
		if parameter.List {
			scalarType = scalarType.Elem()
		}
		if scalarType.Kind() == reflect.Pointer {
			scalarType = scalarType.Elem()
		}
		scalar := m.scalarShape(scalarType, parameter.Enum)
		if parameter.List {
			scalar = &shape{kind: shapeArray, element: scalar}
		}
		result = append(result, parameterModel{name: parameter.Name, in: parameter.In, shape: scalar, required: parameter.Required()})
	}
	return result, nil
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
