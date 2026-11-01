package generate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

// object is one JSON object of the document; encoding/json writes its keys sorted, so the
// document is deterministic.
type object = map[string]any

const (
	schemaPrefix      = "#/components/schemas/"
	errorResponse     = "#/components/responses/Error"
	relativeTimeName  = "RelativeTime"
	relativePattern   = `^[+-]?(?:[0-9]+[smhdw])+$`
	guardedExtension  = "x-livewire-guarded"
	websocketExtended = "x-websocket"
	// The headers of the REST adapter (idempotencyHeader, ExportRowsTrailer);
	// generate stays on the standard library, a test pins them to the adapter's.
	idempotencyHeader = "Idempotency-Key"
	exportRowsTrailer = "Livewire-Export-Rows"
)

// openAPI collects the components while the paths are written.
type openAPI struct {
	model    *model
	schemas  object
	wrappers map[string]bool
	err      error
}

// renderOpenAPI writes the OpenAPI 3.0.3 document (D26: nullable, never anyOf null) of every
// route the REST adapter mounts, as JSON under a YAML comment header.
func renderOpenAPI(api *model, options OpenAPIOptions) ([]byte, error) {
	document := &openAPI{model: api, schemas: object{}, wrappers: map[string]bool{}}
	for _, named := range api.types.ordered {
		document.schemas[named.name] = document.schema(named.definition)
	}
	document.schemas[relativeTimeName] = object{
		"type": "object", "required": []string{"relative"}, "additionalProperties": false,
		"properties": object{"relative": object{"type": "string", "pattern": relativePattern}},
	}
	for _, resource := range slices.Concat(api.resources, []*resourceModel{api.history}) {
		if resource != nil {
			document.filterSchemas(resource)
		}
	}
	paths, err := document.paths()
	if err != nil {
		return nil, err
	}
	if document.err != nil {
		return nil, document.err
	}
	components := object{
		"schemas": document.schemas,
		"responses": object{"Error": object{
			"description": "The one error body, its code from the closed set (design section 3).",
			"content":     object{"application/json": object{"schema": document.library(reflect.TypeFor[livewire.ErrorBody]())}},
		}},
	}
	root := object{"openapi": "3.0.3", "info": info(options), "paths": paths, "components": components}
	if len(options.Servers) > 0 {
		servers := make([]object, 0, len(options.Servers))
		for _, server := range options.Servers {
			entry := object{"url": server.URL}
			setText(entry, "description", server.Description)
			servers = append(servers, entry)
		}
		root["servers"] = servers
	}
	if len(options.SecuritySchemes) > 0 {
		schemes, requirements := object{}, []object{}
		for _, scheme := range options.SecuritySchemes {
			schemes[scheme.Name] = securityScheme(scheme)
			requirements = append(requirements, object{scheme.Name: []string{}})
		}
		components["securitySchemes"] = schemes
		root["security"] = requirements
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("generate: the OpenAPI document: %w", err)
	}
	return append([]byte("# "+header+"\n"), append(encoded, '\n')...), nil
}

func info(options OpenAPIOptions) object {
	entry := object{"title": options.Title, "version": options.Version}
	setText(entry, "description", options.Description)
	return entry
}

func securityScheme(scheme SecurityScheme) object {
	entry := object{"type": scheme.Type}
	setText(entry, "description", scheme.Description)
	switch scheme.Type {
	case "http":
		entry["scheme"] = scheme.Scheme
		setText(entry, "bearerFormat", scheme.BearerFormat)
	case "apiKey":
		entry["in"], entry["name"] = scheme.In, scheme.ParameterName
	case "openIdConnect":
		entry["openIdConnectUrl"] = scheme.OpenIDConnectURL
	}
	return entry
}

func setText(entry object, key, value string) {
	if value != "" {
		entry[key] = value
	}
}

func reference(name string) object {
	return object{"$ref": schemaPrefix + name}
}

// library refers to the schema of a library type under the name it got: its fixed one, or a
// DTO's when a declaration guards one of its fields (describeLibrary).
func (o *openAPI) library(goType reflect.Type) object {
	return reference(o.model.types.named[goType].name)
}

// decorated adds keys to a schema; a $ref takes no siblings in OpenAPI 3.0, so it is wrapped
// in allOf first.
func decorated(schema object, keys object) object {
	if _, isReference := schema["$ref"]; isReference {
		schema = object{"allOf": []object{schema}}
	}
	for key, value := range keys {
		schema[key] = value
	}
	return schema
}

func (o *openAPI) schema(value *shape) object {
	var schema object
	switch value.kind {
	case shapeString:
		schema = object{"type": "string"}
	case shapeNumber:
		schema = object{"type": "number"}
	case shapeInteger:
		schema = object{"type": "integer"}
		if value.unsigned {
			schema["minimum"] = 0
		}
	case shapeBoolean:
		schema = object{"type": "boolean"}
	case shapeUnknown:
		return object{}
	case shapeArray:
		schema = object{"type": "array", "items": o.schema(value.element)}
	case shapeMap:
		schema = object{"type": "object", "additionalProperties": o.schema(value.element)}
	case shapeObject:
		schema = o.objectSchema(value.properties)
	case shapeReference:
		schema = reference(value.reference.name)
	case shapeEnum:
		schema = object{"type": "string", "enum": value.values}
	case shapeListResponse, shapeVersion:
		schema = o.wrapper(value)
	case shapeTimeValue:
		schema = object{"oneOf": []object{{"type": "string", "format": "date-time"}, reference(relativeTimeName)}}
	}
	setText(schema, "format", value.format)
	for key, bound := range map[string]int{"maxLength": value.maximumLength, "minItems": value.minimumItems, "maxItems": value.maximumItems} {
		if bound > 0 {
			schema[key] = bound
		}
	}
	if value.nullable {
		return nullable(schema, schemaType(value))
	}
	return schema
}

// nullable admits null besides the schema (D26): `nullable: true` beside its type, and around
// a $ref, which takes no siblings, an allOf that carries the referenced schema's type, since
// OpenAPI 3.0.3 reads nullable without a type as no null at all.
func nullable(schema object, typeName string) object {
	keys := object{"nullable": true}
	if _, isReference := schema["$ref"]; isReference && typeName != "" {
		keys["type"] = typeName
		if typeName == "array" {
			keys["items"] = object{} // an array type needs items; the referenced schema constrains them
		}
	}
	return decorated(schema, keys)
}

// schemaType is the OpenAPI type of a shape, "" for a value of any type.
func schemaType(value *shape) string {
	switch value.kind {
	case shapeString, shapeEnum:
		return "string"
	case shapeNumber:
		return "number"
	case shapeInteger:
		return "integer"
	case shapeBoolean:
		return "boolean"
	case shapeArray:
		return "array"
	case shapeMap, shapeObject, shapeListResponse, shapeVersion:
		return "object"
	case shapeReference:
		return schemaType(value.reference.definition)
	case shapeUnknown, shapeTimeValue:
	}
	return ""
}

func (o *openAPI) objectSchema(properties []property) object {
	members, required := object{}, []string{}
	for _, member := range properties {
		schema := o.schema(member.shape)
		if member.guarded {
			schema = decorated(schema, object{guardedExtension: true})
		}
		members[member.name] = schema
		if !member.optional {
			required = append(required, member.name)
		}
	}
	schema := object{"type": "object", "properties": members}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// wrapper is the schema of a ListResponse or Version of an item: a named component
// ("PartListResponse", "PartVersion") when the item is a named type, inline otherwise.
func (o *openAPI) wrapper(value *shape) object {
	element := o.schema(value.element)
	var definition object
	if value.kind == shapeVersion {
		definition = object{"type": "object", "required": []string{"id", "operation", "validFrom", "validTo", "actor", "realActor", "mutation", "requestId", "data"}, "properties": object{
			"id": object{"type": "integer", "format": "int64"}, "operation": o.library(reflect.TypeFor[livewire.ChangeOperation]()),
			"validFrom": object{"type": "string", "format": "date-time"}, "validTo": object{"type": "string", "format": "date-time", "nullable": true},
			"actor": object{"type": "string"}, "realActor": object{"type": "string"}, "mutation": object{"type": "string"},
			"requestId": object{"type": "string"}, "data": nullable(element, schemaType(value.element)),
		}}
	} else {
		definition = object{"type": "object", "required": []string{"limit", "offset", "total", "items"}, "properties": object{
			"limit": object{"type": "integer", "format": "int64"}, "offset": object{"type": "integer", "format": "int64"},
			"total": object{"type": "integer", "format": "int64"}, "items": object{"type": "array", "items": element},
			"next": object{"type": "string", "description": "The cursor of the next page (cursor mode, D38); absent at the end."},
		}}
	}
	name := wrapperName(value)
	if name == "" {
		return definition
	}
	if _, taken := o.schemas[name]; taken && !o.wrappers[name] {
		o.err = fmt.Errorf("generate: the OpenAPI schema %s of a list or version and a type of the same name collide; rename the type", name)
	}
	o.schemas[name], o.wrappers[name] = definition, true
	return reference(name)
}

// wrapperName names the component of a ListResponse or Version after its named item, ""
// when the item has no name.
func wrapperName(value *shape) string {
	suffix := "ListResponse"
	if value.kind == shapeVersion {
		suffix = "Version"
	}
	element := value.element
	switch {
	case element.nullable:
		return ""
	case element.kind == shapeReference:
		return element.reference.name + suffix
	case element.kind == shapeVersion:
		if inner := wrapperName(element); inner != "" {
			return inner + suffix
		}
	}
	return ""
}

// filterSchemas adds the filter document schemas of a resource (D12): the document, its
// node and its conditions, one per field and operand (resourceModel.conditions).
func (o *openAPI) filterSchemas(resource *resourceModel) {
	var conditions []object
	for _, member := range resource.conditions() {
		conditions = append(conditions, object{
			"type": "object", "required": []string{"field", "op", "value"}, "additionalProperties": false,
			"properties": object{
				"field": object{"type": "string", "enum": []string{member.field}},
				"op":    object{"type": "string", "enum": stringsOf(member.operators)},
				"value": o.schema(member.operand),
			},
		})
	}
	condition := object{"not": object{}} // no filterable field: no condition validates
	if len(conditions) > 0 {
		condition = object{"oneOf": conditions}
	}
	o.schemas[resource.conditionType()] = condition
	node := reference(resource.nodeType())
	branch := func(key string, value object) object {
		return object{"type": "object", "required": []string{key}, "additionalProperties": false, "properties": object{key: value}}
	}
	list := object{"type": "array", "items": node, "minItems": 1}
	o.schemas[resource.nodeType()] = object{"oneOf": []object{reference(resource.conditionType()), branch("and", list), branch("or", list), branch("not", node)}}
	properties := object{"where": node}
	if resource.searchable() {
		properties["search"] = object{"type": "string", "maxLength": query.MaximumStringBytes}
	}
	o.schemas[resource.filterType()] = object{
		"type": "object", "properties": properties, "additionalProperties": false,
		"description": "A filter document (D12) of " + resource.name + ": at most " + strconv.Itoa(query.MaximumDepth) + " levels and " + strconv.Itoa(query.MaximumNodes) + " nodes.",
	}
}

// pathItems maps a full path to its operations by lower-case method.
type pathItems map[string]object

func (p pathItems) add(path, method string, operation object) error {
	item, ok := p[path]
	if !ok {
		item = object{}
		p[path] = item
	}
	key := strings.ToLower(method)
	if _, taken := item[key]; taken {
		return fmt.Errorf("generate: two operations at %s %s", method, path)
	}
	item[key] = operation
	return nil
}

func (o *openAPI) paths() (pathItems, error) {
	paths := pathItems{}
	base := o.model.basePath
	for _, read := range o.model.reads {
		if err := paths.add(base+read.path, http.MethodGet, o.readOperation(read)); err != nil {
			return nil, err
		}
	}
	for _, mutation := range o.model.mutations {
		if err := paths.add(base+mutation.path, mutation.method, o.mutationOperation(mutation)); err != nil {
			return nil, err
		}
	}
	for _, upload := range o.model.uploads {
		if err := paths.add(base+upload.path, http.MethodPost, o.uploadOperation(upload)); err != nil {
			return nil, err
		}
	}
	for _, export := range o.model.exports {
		if err := paths.add(base+export.path, http.MethodGet, o.exportOperation(export)); err != nil {
			return nil, err
		}
	}
	for path, operations := range o.libraryOperations() {
		for method, operation := range operations {
			if err := paths.add(base+path, method, operation); err != nil {
				return nil, err
			}
		}
	}
	return paths, nil
}

// errorResponses lists the refusals an operation may answer, each with the error body.
func errorResponses(responses object, statuses ...int) object {
	for _, status := range statuses {
		responses[strconv.Itoa(status)] = object{"$ref": errorResponse}
	}
	return responses
}

func jsonContent(schema object) object {
	return object{"application/json": object{"schema": schema}}
}

// readOperation describes a read; x-websocket names its socket target and operations, and a
// presence read's join and leave, which the engine resolves against the declaration itself
// (D46), under presence.
func (o *openAPI) readOperation(read *readModel) object {
	operations := []string{string(livewire.OperationQuery)}
	if read.live {
		operations = append(operations, string(livewire.OperationSubscribe))
	}
	websocket := object{"target": read.targetName(), "ops": operations}
	if read.kind == livewire.ReadPresence {
		websocket["presence"] = object{"target": read.declaration, "ops": []string{string(livewire.OperationJoin), string(livewire.OperationLeave)}}
	}
	responses := object{"200": object{"description": "The " + describeRead(read) + ".", "content": jsonContent(o.schema(read.data))}}
	return object{
		"operationId": read.functionName(), "tags": []string{read.declaration},
		"parameters":      o.parameters(read.parameters, read.reserved),
		"responses":       errorResponses(responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError),
		websocketExtended: websocket,
	}
}

func describeRead(read *readModel) string {
	switch read.kind {
	case livewire.ReadItem:
		return "item"
	case livewire.ReadDistinct:
		return "distinct values (D15)"
	case livewire.ReadHistory:
		return "versions of the item (D34)"
	case livewire.ReadPresence:
		return "presences on the record (D37)"
	case livewire.ReadMain:
	}
	if read.list {
		return "list (D14)"
	}
	return "object"
}

func (o *openAPI) parameters(parameters []parameterModel, reserved reservedModel) []object {
	result := []object{}
	for _, parameter := range parameters {
		entry := object{"name": parameter.name, "in": string(parameter.in), "required": parameter.required, "schema": o.schema(parameter.shape)}
		if parameter.shape.kind == shapeArray {
			entry["style"], entry["explode"] = "form", false
		}
		result = append(result, entry)
	}
	query := func(name string, schema object, description string) {
		entry := object{"name": name, "in": "query", "required": false, "schema": schema}
		setText(entry, "description", description)
		result = append(result, entry)
	}
	if reserved.filters != nil {
		result = append(result, object{
			"name": livewire.ParameterFilter, "in": "query", "required": false, "description": "An inline filter document; registered as a stored filter on subscribe (D13).",
			"content": jsonContent(reference(reserved.filters.filterType())),
		})
		query(livewire.ParameterFilterID, object{"type": "string"}, "A stored filter's id; excludes filter.")
	}
	if reserved.field {
		result = append(result, object{"name": livewire.ParameterField, "in": "query", "required": true, "schema": object{"type": "string", "enum": reserved.filters.distinctFields()}})
	}
	if reserved.sort {
		query(livewire.ParameterSort, object{"type": "string"}, "Comma-separated fields, \"-\" for descending, of: "+strings.Join(reserved.filters.sortFields(), ", ")+".")
	}
	if reserved.page {
		query(livewire.ParameterLimit, object{"type": "integer", "minimum": 0}, "Clamped to the resource's MaximumLimit (D14).")
		query(livewire.ParameterOffset, object{"type": "integer", "minimum": 0}, "")
	}
	if reserved.after {
		query(livewire.ParameterAfter, object{"type": "string"}, "The `next` of the previous page (cursor mode, D38).")
	}
	if reserved.format {
		query(livewire.ParameterFormat, object{"type": "string", "enum": []string{livewire.ExportCSV}}, "")
	}
	return result
}

func idempotencyParameter() object {
	return object{
		"name": idempotencyHeader, "in": "header", "required": false, "description": "Replays the recorded answer of the same key (D16); a UUID v7 by the generated clients.",
		"schema": object{"type": "string"},
	}
}

func (o *openAPI) mutationOperation(mutation *mutationModel) object {
	success := object{"description": "The mutation's answer."}
	if mutation.response != nil {
		success["content"] = jsonContent(o.schema(mutation.response))
	}
	operation := object{
		"operationId": mutation.name, "tags": []string{mutation.name},
		"parameters":      append(o.parameters(mutation.parameters, reservedModel{}), idempotencyParameter()),
		"responses":       errorResponses(object{strconv.Itoa(mutation.status): success}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError),
		websocketExtended: object{"target": mutation.name, "ops": []string{string(livewire.OperationCall)}},
	}
	if mutation.body != nil {
		operation["requestBody"] = object{"required": true, "content": jsonContent(o.schema(mutation.body))}
	}
	if mutation.effect.Kind != "" {
		operation["x-livewire-effect"] = object{"kind": string(mutation.effect.Kind), "target": mutation.effect.Target}
	}
	return operation
}

func (o *openAPI) uploadOperation(upload *uploadModel) object {
	content := object{}
	binary := object{"type": "string", "format": "binary"}
	for _, contentType := range upload.contentTypes {
		content[contentType] = object{"schema": binary}
	}
	content["multipart/form-data"] = object{
		"schema":   object{"type": "object", "required": []string{"file"}, "properties": object{"file": binary}},
		"encoding": object{"file": object{"contentType": strings.Join(upload.contentTypes, ", ")}},
	}
	return object{
		"operationId": upload.name, "tags": []string{upload.name},
		"description": "At most " + strconv.FormatInt(upload.maximumBytes, 10) + " bytes (D17).",
		"parameters":  append(o.parameters(upload.parameters, reservedModel{}), idempotencyParameter()),
		"requestBody": object{"required": true, "content": content},
		"responses": errorResponses(object{strconv.Itoa(upload.status): object{"description": "The upload's answer.", "content": jsonContent(o.schema(upload.response))}},
			http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError),
	}
}

func (o *openAPI) exportOperation(export *exportModel) object {
	csv := object{
		"description": "Every row under scope, filter and sort (D35). A complete export ends with the trailer " + exportRowsTrailer + " (the rows written); a failure after the first byte aborts the body (D60).",
		"content":     object{"text/csv": object{"schema": object{"type": "string"}}},
	}
	return object{
		"operationId": export.downloadFunction, "tags": []string{export.declaration},
		"parameters": o.parameters(nil, export.reserved),
		"responses":  errorResponses(object{"200": csv}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError),
	}
}

// libraryOperations are the routes the library serves itself (D13, D33, D59).
func (o *openAPI) libraryOperations() map[string]map[string]object {
	var targets []string
	for _, read := range o.model.reads {
		if read.reserved.filters != nil && read.kind != livewire.ReadDistinct {
			targets = append(targets, read.targetName())
		}
	}
	resource := object{"type": "string"}
	if len(targets) > 0 {
		resource["enum"] = targets
	}
	refusals := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError}
	return map[string]map[string]object{
		livewire.BatchPath: {http.MethodPost: {
			"operationId": "batch", "tags": []string{livewire.BatchTarget}, "parameters": []object{idempotencyParameter()},
			"requestBody": object{"required": true, "content": jsonContent(object{"type": "array", "items": o.library(reflect.TypeFor[livewire.BatchCall]())})},
			"responses": errorResponses(object{"200": object{"description": "One result per call, in order (D47).", "content": jsonContent(object{"type": "array", "items": o.library(reflect.TypeFor[livewire.BatchResult]())})}},
				http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError),
			websocketExtended: object{"target": livewire.BatchTarget, "ops": []string{string(livewire.OperationBatch)}},
		}},
		livewire.FiltersPath: {
			http.MethodPost: {
				"operationId": "saveFilter", "tags": []string{livewire.FiltersTarget},
				"requestBody": object{"required": true, "content": jsonContent(o.library(reflect.TypeFor[livewire.FilterRequest]()))},
				"responses":   errorResponses(object{"201": object{"description": "The stored filter.", "content": jsonContent(o.library(reflect.TypeFor[livewire.FilterItem]()))}}, refusals...),
			},
			http.MethodGet: {
				"operationId": "listFilters", "tags": []string{livewire.FiltersTarget},
				"parameters": []object{{"name": "resource", "in": "query", "required": false, "schema": resource}},
				"responses":  errorResponses(object{"200": object{"description": "The caller's named filters.", "content": jsonContent(o.wrapper(&shape{kind: shapeListResponse, element: &shape{kind: shapeReference, reference: o.model.types.named[reflect.TypeFor[livewire.FilterItem]()]}}))}}, refusals...),
			},
		},
		livewire.FiltersPath + "/{id}": {http.MethodDelete: {
			"operationId": "deleteFilter", "tags": []string{livewire.FiltersTarget},
			"parameters": []object{{"name": "id", "in": "path", "required": true, "schema": object{"type": "string"}}},
			"responses":  errorResponses(object{"204": object{"description": "Removed, or not there."}}, refusals...),
		}},
	}
}
