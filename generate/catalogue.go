package generate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/govengers/livewire"
)

// ToolKind is what a tool of the catalogue runs (D85).
type ToolKind string

// The tool kinds.
const (
	ToolRead     ToolKind = "read"     // a read of a declaration: Engine.Read
	ToolMutation ToolKind = "mutation" // a mutation: Engine.Call
	ToolUpload   ToolKind = "upload"   // an upload: Engine.Upload
)

// The members of a tool's arguments besides the declaration's parameters (D85): a mutation's
// body under ArgumentBody, an upload's file as ArgumentContent with its ArgumentContentType,
// written as ArgumentContentEncoding says (ContentText, the default, or ContentBase64).
// Every other member is a parameter of the declaration, the reserved ones of a filterable
// read included (filter as a JSON document, not as text).
const (
	ArgumentBody            = "body"
	ArgumentContent         = "content"
	ArgumentContentType     = "contentType"
	ArgumentContentEncoding = "contentEncoding"
	ContentText             = "text"
	ContentBase64           = "base64"
)

// maximumToolNameBytes is the longest tool name every model provider accepts.
const maximumToolNameBytes = 64

// Tool is one operation of the registry as an agent calls it (D85): its name (the OpenAPI
// operation id), a description, the kind and whether it changes data, what it runs (the
// declaration, the read kind, the REST method and path), and three JSON Schemas (draft
// 2020-12, self-contained with $defs): Parameters, the tool's arguments as a model sends
// them (the declaration's parameters and, for a mutation, the body under "body"); Body, the
// mutation's body alone (nil for a read, an upload and a mutation without one); Result, what
// the tool answers (the REST body; JSON null for a mutation without content).
type Tool struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Kind        ToolKind          `json:"kind"`
	Mutating    bool              `json:"mutating"`
	Target      string            `json:"target"`
	Read        livewire.ReadKind `json:"read,omitempty"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Parameters  json.RawMessage   `json:"parameters"`
	Body        json.RawMessage   `json:"body,omitempty"`
	Result      json.RawMessage   `json:"result"`
}

// clone copies the schemas, so a caller that changes a Tool's bytes changes no other copy.
func (t Tool) clone() Tool {
	t.Parameters, t.Body, t.Result = bytes.Clone(t.Parameters), bytes.Clone(t.Body), bytes.Clone(t.Result)
	return t
}

// CatalogueOptions configures NewCatalogue: Enums and Presence as for Write (Options).
type CatalogueOptions struct {
	Enums    []Enum
	Presence bool
}

// Catalogue is every read, mutation and upload of a registry as a tool (D85): the main, item,
// distinct and history reads of a resource, a typed list's and an object's read, the
// presence reads when the engine serves presence, every mutation and every upload. Exports
// (a CSV stream), batches and stored filters are not tools. A Catalogue is immutable and safe
// for concurrent use.
type Catalogue struct {
	registry *livewire.Registry
	tools    []Tool
	byName   map[string]int
}

// NewCatalogue describes the registry's operations as tools, deriving their schemas as the
// OpenAPI document derives its own. It validates and copies the options and refuses what
// Render refuses, a tool name longer than 64 bytes (what model providers accept) and a
// mutation or upload parameter whose name is one of the argument members of its body or file
// (ArgumentBody and the content members).
func NewCatalogue(registry *livewire.Registry, options *CatalogueOptions) (*Catalogue, error) {
	if registry == nil {
		return nil, errors.New("generate: NewCatalogue needs a registry")
	}
	if options == nil {
		options = &CatalogueOptions{}
	}
	resolved := Options{Enums: slices.Clone(options.Enums), Presence: options.Presence}
	for position := range resolved.Enums {
		enum := &resolved.Enums[position]
		enum.Values = slices.Clone(enum.Values)
		if err := validateEnum(*enum, resolved.Enums[:position]); err != nil {
			return nil, err
		}
	}
	api, err := newModel(registry, resolved)
	if err != nil {
		return nil, err
	}
	if _, err := renderTypeScript(api); err != nil { // the operation ids are TypeScript identifiers (render)
		return nil, err
	}
	tools, err := describeTools(api)
	if err != nil {
		return nil, err
	}
	catalogue := &Catalogue{registry: registry, tools: tools, byName: make(map[string]int, len(tools))}
	for position, tool := range tools {
		catalogue.byName[tool.Name] = position
	}
	return catalogue, nil
}

// Registry is the registry the catalogue describes.
func (c *Catalogue) Registry() *livewire.Registry { return c.registry }

// Tools returns every tool, reads first in declaration order, then mutations, then uploads,
// as copies.
func (c *Catalogue) Tools() []Tool {
	tools := make([]Tool, len(c.tools))
	for position, tool := range c.tools {
		tools[position] = tool.clone()
	}
	return tools
}

// Lookup returns a copy of the tool of the name.
func (c *Catalogue) Lookup(name string) (Tool, bool) {
	position, ok := c.byName[name]
	if !ok {
		return Tool{}, false
	}
	return c.tools[position].clone(), true
}

// catalogueFile is the catalogue as Options.CatalogueFile writes it: JSON cannot carry a
// comment, so the generated header is its $comment member.
type catalogueFile struct {
	Comment string `json:"$comment"`
	Tools   []Tool `json:"tools"`
}

func renderCatalogue(api *model) ([]byte, error) {
	tools, err := describeTools(api)
	if err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(catalogueFile{Comment: header, Tools: tools}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("generate: the tool catalogue: %w", err)
	}
	return append(encoded, '\n'), nil
}

// describeTools derives the tools of the model from the components of its OpenAPI document.
func describeTools(api *model) ([]Tool, error) {
	document := newOpenAPI(api)
	var tools []Tool
	for _, read := range api.reads {
		properties, required := document.argumentProperties(read.parameters, read.reserved)
		tools = append(tools, Tool{
			Name: read.functionName(), Description: describeReadTool(read), Kind: ToolRead,
			Target: read.declaration, Read: read.kind, Method: http.MethodGet, Path: api.basePath + read.path,
			Parameters: document.toolSchema(argumentsSchema(properties, required)), Result: document.toolSchema(document.schema(read.data)),
		})
	}
	for _, mutation := range api.mutations {
		properties, required := document.argumentProperties(mutation.parameters, reservedModel{})
		tool := Tool{
			Name: mutation.name, Description: describeMutationTool(mutation), Kind: ToolMutation, Mutating: true,
			Target: mutation.name, Method: mutation.method, Path: api.basePath + mutation.path,
			Result: json.RawMessage(`{"type":"null"}`),
		}
		if err := refuseArgumentNames(mutation.name, properties); err != nil {
			return nil, err
		}
		if mutation.body != nil {
			body := document.schema(mutation.body)
			properties[ArgumentBody], required = body, append(required, ArgumentBody)
			tool.Body = document.toolSchema(body)
		}
		if mutation.response != nil {
			tool.Result = document.toolSchema(document.schema(mutation.response))
		}
		tool.Parameters = document.toolSchema(argumentsSchema(properties, required))
		tools = append(tools, tool)
	}
	for _, upload := range api.uploads {
		properties, required := document.argumentProperties(upload.parameters, reservedModel{})
		if err := refuseArgumentNames(upload.name, properties); err != nil {
			return nil, err
		}
		properties[ArgumentContent] = object{"type": "string", "description": "The file: its text as it is, or its bytes in base64 with contentEncoding base64; at most " + strconv.FormatInt(upload.maximumBytes, 10) + " bytes."}
		properties[ArgumentContentType] = object{"type": "string", "enum": upload.contentTypes, "description": "The file's content type."}
		properties[ArgumentContentEncoding] = object{"type": "string", "enum": []string{ContentText, ContentBase64}, "description": "How content is written; text by default."}
		tools = append(tools, Tool{
			Name: upload.name, Description: describeUploadTool(upload), Kind: ToolUpload, Mutating: true,
			Target: upload.name, Method: http.MethodPost, Path: api.basePath + upload.path,
			Parameters: document.toolSchema(argumentsSchema(properties, append(required, ArgumentContent, ArgumentContentType))),
			Result:     document.toolSchema(document.schema(upload.response)),
		})
	}
	if document.err != nil {
		return nil, document.err
	}
	for _, tool := range tools {
		if len(tool.Name) > maximumToolNameBytes {
			return nil, fmt.Errorf("generate: the tool name %s is longer than %d bytes, which model providers refuse; shorten the declaration's name", tool.Name, maximumToolNameBytes)
		}
	}
	return tools, nil
}

// refuseArgumentNames refuses a parameter that the members of a body or file would shadow.
func refuseArgumentNames(name string, properties object) error {
	for _, reserved := range []string{ArgumentBody, ArgumentContent, ArgumentContentType, ArgumentContentEncoding} {
		if _, taken := properties[reserved]; taken {
			return fmt.Errorf("generate: %s: the parameter %q is the tool argument of the body or file (D85); rename it", name, reserved)
		}
	}
	return nil
}

// argumentProperties are the parameters of an operation as the members of a tool's
// arguments: each parameter's schema (an inline filter as its document) with its description,
// and the names a call must carry.
func (o *openAPI) argumentProperties(parameters []parameterModel, reserved reservedModel) (object, []string) {
	properties, required := object{}, []string{}
	for _, parameter := range o.parameters(parameters, reserved) {
		name, _ := parameter["name"].(string)
		schema, ok := parameter["schema"].(object)
		if !ok { // the filter: a JSON document, not its text
			schema = parameter["content"].(object)["application/json"].(object)["schema"].(object)
		}
		if description, ok := parameter["description"].(string); ok {
			schema = decorated(cloneObject(schema), object{"description": description})
		}
		properties[name] = schema
		if parameter["required"] == true {
			required = append(required, name)
		}
	}
	return properties, required
}

func argumentsSchema(properties object, required []string) object {
	schema := object{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func cloneObject(value object) object {
	cloned := make(object, len(value))
	for key, member := range value {
		cloned[key] = member
	}
	return cloned
}

const definitionsPrefix = "#/$defs/"

// toolSchema turns an OpenAPI 3.0.3 schema of the document into a self-contained JSON Schema
// (draft 2020-12): references point into $defs, which holds every component it reaches,
// `nullable` becomes anyOf with null, an object of declared members admits no other, and the
// x- extensions are dropped.
func (o *openAPI) toolSchema(schema object) json.RawMessage {
	references := map[string]bool{}
	converted, _ := jsonSchema(schema, references).(object)
	definitions := object{}
	for pending := sortedKeys(references); len(pending) > 0; pending = pending[1:] {
		name := pending[0]
		if _, done := definitions[name]; done {
			continue
		}
		reached := map[string]bool{}
		definitions[name] = jsonSchema(o.schemas[name], reached)
		pending = append(pending, sortedKeys(reached)...)
	}
	if len(definitions) > 0 {
		converted["$defs"] = definitions
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		o.err = fmt.Errorf("generate: a tool schema: %w", err)
	}
	return encoded
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// jsonSchema converts one value of an OpenAPI schema, collecting the components it refers to.
func jsonSchema(value any, references map[string]bool) any {
	switch typed := value.(type) {
	case object:
		converted := object{}
		for key, member := range typed {
			switch {
			case key == "nullable" || strings.HasPrefix(key, "x-"):
			case key == "$ref":
				name := strings.TrimPrefix(member.(string), schemaPrefix)
				references[name] = true
				converted[key] = definitionsPrefix + name
			case key == "properties": // its keys are member names, not keywords
				properties := object{}
				for name, property := range member.(object) {
					properties[name] = jsonSchema(property, references)
				}
				converted[key] = properties
			default:
				converted[key] = jsonSchema(member, references)
			}
		}
		if _, open := typed["additionalProperties"]; !open && typed["type"] == "object" && typed["properties"] != nil {
			converted["additionalProperties"] = false // a body's unknown members are refused (D40); an answer has none
		}
		if typed["nullable"] == true {
			return object{"anyOf": []any{converted, object{"type": "null"}}}
		}
		return converted
	case []object:
		converted := make([]any, len(typed))
		for position, member := range typed {
			converted[position] = jsonSchema(member, references)
		}
		return converted
	default:
		return value
	}
}

func describeReadTool(read *readModel) string {
	name := read.declaration
	var text string
	switch read.kind {
	case livewire.ReadItem:
		text = "Reads one item of " + name + " by its " + read.parameters[0].name + "."
	case livewire.ReadDistinct:
		text = "Counts the distinct values of one field of " + name + " under an optional filter; answers [{value, count}]."
	case livewire.ReadHistory:
		text = "Lists the versions of one item of " + name + " by its " + read.parameters[0].name + ": what changed, when, by whom; answers {limit, offset, total, items}."
	case livewire.ReadPresence:
		text = "Lists who is present on the record of " + name + " named by key."
	case livewire.ReadMain:
		switch {
		case read.reserved.filters != nil:
			text = "Lists " + name + ": one page of the items under an optional filter document, sort, limit and offset; answers {limit, offset, total, items}."
		case read.list:
			text = "Lists " + name + "; answers {limit, offset, total, items}."
		default:
			text = "Reads " + name + "."
		}
	}
	return withDeclared(read.description, text)
}

func describeMutationTool(mutation *mutationModel) string {
	text := "Changes data: " + mutation.method + " " + mutation.path + "."
	if mutation.response == nil {
		text += " Answers nothing."
	}
	return withDeclared(mutation.description, text)
}

func describeUploadTool(upload *uploadModel) string {
	return withDeclared(upload.description, "Uploads a file: POST "+upload.path+", content of type "+strings.Join(upload.contentTypes, ", ")+", at most "+strconv.FormatInt(upload.maximumBytes, 10)+" bytes.")
}

// withDeclared puts the declaration's own description before the kind's.
func withDeclared(declared, text string) string {
	if declared == "" {
		return text
	}
	return strings.TrimSpace(declared) + " " + text
}
