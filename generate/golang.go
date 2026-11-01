package generate

import (
	"encoding"
	"encoding/json"
	"fmt"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/internal/jsonfield"
	"github.com/govengers/livewire/query"
)

// The packages the generated Go code imports besides the application's.
const (
	livewirePath = "github.com/govengers/livewire"
	clientPath   = "github.com/govengers/livewire/client"
	queryPath    = "github.com/govengers/livewire/query"
	uuidPath     = "github.com/google/uuid"
)

const goHeader = "// " + header + "\n\n"

// goIdentifiers keeps the top-level identifiers of one generated Go package, so two never
// collide.
type goIdentifiers struct {
	taken map[string]string
}

func newGoIdentifiers(fixed ...string) *goIdentifiers {
	identifiers := &goIdentifiers{taken: map[string]string{}}
	for _, name := range fixed {
		identifiers.taken[name] = "the generated code"
	}
	return identifiers
}

func (i *goIdentifiers) add(name, owner string) error {
	if earlier, ok := i.taken[name]; ok {
		return fmt.Errorf("generate: %s and %s are both named %s in the Go client; rename one of the declarations", earlier, owner, name)
	}
	if !token.IsIdentifier(name) {
		return fmt.Errorf("generate: %s would be named %q, which is no Go identifier", owner, name)
	}
	i.taken[name] = owner
	return nil
}

func (i *goIdentifiers) names() []string {
	names := make([]string, 0, len(i.taken))
	for name := range i.taken {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// goClient renders the Go client package (D19, D67): client.go with the Client, one method
// per read, mutation, upload and export and the library's routes, and filters.go with the
// filter document types of every filterable read.
type goClient struct {
	api         *model
	options     GoClientOptions
	identifiers *goIdentifiers
}

func renderGoClient(api *model, options GoClientOptions) ([]file, error) {
	renderer := &goClient{api: api, options: options, identifiers: newGoIdentifiers("Client", "New", "SaveFilter", "ListFilters", "DeleteFilter", "Batch", "idempotencyKey")}
	if err := renderer.declareIdentifiers(); err != nil {
		return nil, err
	}
	clientSource, err := renderer.clientFile()
	if err != nil {
		return nil, err
	}
	filtersSource, err := renderer.filtersFile()
	if err != nil {
		return nil, err
	}
	return []file{{path: "client.go", content: clientSource}, {path: "filters.go", content: filtersSource}}, nil
}

// declareIdentifiers takes every top-level name before anything renders, so an import alias
// takes none of them.
func (g *goClient) declareIdentifiers() error {
	var owned []ownedNames
	for _, read := range g.api.reads {
		names := []string{read.queryType, goReadName("Get", read)}
		if read.live {
			names = append(names, goReadName("Subscribe", read))
		}
		owned = append(owned, ownedNames{names, "the read " + read.targetName()})
	}
	for _, mutation := range g.api.mutations {
		name := pascalCase(mutation.name)
		names := []string{mutation.parametersType, name, "Batch" + name}
		if mutation.clientID != livewire.ClientIDNone {
			names = append(names, "Prepare"+name, "with"+name+"ClientID")
		}
		owned = append(owned, ownedNames{names, "the mutation " + mutation.name})
	}
	for _, upload := range g.api.uploads {
		owned = append(owned, ownedNames{[]string{upload.parametersType, pascalCase(upload.name)}, "the upload " + upload.name})
	}
	for _, export := range g.api.exports {
		owned = append(owned, ownedNames{[]string{export.queryType, "Export" + pascalCase(export.declaration)}, "the export of " + export.declaration})
	}
	for _, resource := range g.filterResources() {
		owned = append(owned, ownedNames{filterNames(resource), "the filter types of " + resource.name})
	}
	for _, entry := range owned {
		for _, name := range entry.names {
			if err := g.identifiers.add(name, entry.owner); err != nil {
				return err
			}
		}
	}
	return nil
}

// filterNames are the identifiers of a resource's filter vocabulary.
func filterNames(resource *resourceModel) []string {
	names := []string{resource.name + "Field", resource.filterType()}
	for _, function := range filterFunctions(resource) {
		names = append(names, function.name)
	}
	for _, declared := range filterTypes(resource) {
		names = append(names, declared.name)
		for _, constant := range declared.constants {
			names = append(names, constant.name)
		}
	}
	return names
}

// ownedNames are the identifiers one declaration takes.
type ownedNames struct {
	names []string
	owner string
}

func (g *goClient) filterResources() []*resourceModel {
	resources := slices.Clone(g.api.resources)
	if g.api.history != nil {
		resources = append(resources, g.api.history)
	}
	return resources
}

func goReadName(prefix string, read *readModel) string {
	return prefix + strings.TrimPrefix(read.functionName(), "get")
}

// goSource is one generated Go file: its imports and its body.
type goSource struct {
	file *goFile
	body strings.Builder
}

func (g *goClient) newSource() *goSource {
	return &goSource{file: newGoFile(g.options.ImportPath, g.identifiers.names())}
}

func (g *goClient) finish(name, documentation string, source *goSource) ([]byte, error) {
	return formatGo(name, goHeader+documentation+"package "+g.options.Package+"\n\n"+source.file.importBlock()+source.body.String())
}

func (g *goClient) clientFile() ([]byte, error) {
	source := g.newSource()
	runtime, livewireAlias := source.file.use(clientPath, "client"), source.file.use(livewirePath, "livewire")
	context := source.file.use("context", "context")
	fmt.Fprintf(&source.body, clientPreamble, basePathOrRoot(g.api.basePath), runtime, context, livewireAlias)
	for _, read := range g.api.reads {
		if err := g.read(source, read); err != nil {
			return nil, err
		}
	}
	for _, mutation := range g.api.mutations {
		if err := g.mutation(source, mutation); err != nil {
			return nil, err
		}
	}
	for _, upload := range g.api.uploads {
		if err := g.upload(source, upload); err != nil {
			return nil, err
		}
	}
	for _, export := range g.api.exports {
		if err := g.export(source, export); err != nil {
			return nil, err
		}
	}
	documentation := fmt.Sprintf("// Package %s is the Go client of the API at %s (D19, D67), generated from\n// its livewire registry: build a Client with New and call one method per read, mutation,\n// upload and export.\n", g.options.Package, basePathOrRoot(g.api.basePath))
	return g.finish("client.go", documentation, source)
}

func basePathOrRoot(basePath string) string {
	if basePath == "" {
		return "/"
	}
	return basePath
}

// clientPreamble is the Client, New and the library's routes: the base path, the runtime's,
// context's and livewire's aliases.
const clientPreamble = `// Client is the client of the API: its REST routes and one socket for every subscription,
// as the identity of one API key (D20). It is safe for concurrent use.
type Client struct {
	runtime *%[2]s.Client
}

// New validates and copies options (DES-001): BaseURL is the API's URL with its base path
// %[1]s, APIKey the key the server resolves.
func New(options *%[2]s.Options) (*Client, error) {
	runtime, err := %[2]s.New(options)
	if err != nil {
		return nil, err
	}
	return &Client{runtime: runtime}, nil
}

// SaveFilter stores a filter (POST /filters, D13); an unnamed one is recovered when the
// server evicts it (D54).
func (c *Client) SaveFilter(ctx %[3]s.Context, filterRequest *%[4]s.FilterRequest) (*%[4]s.FilterItem, error) {
	return c.runtime.SaveFilter(ctx, filterRequest)
}

// ListFilters answers the caller's named filters of a resource (or "name.history"), of every
// one for "".
func (c *Client) ListFilters(ctx %[3]s.Context, resource string) (*%[4]s.ListResponse[%[4]s.FilterItem], error) {
	return c.runtime.ListFilters(ctx, resource)
}

// DeleteFilter removes one of the caller's filters.
func (c *Client) DeleteFilter(ctx %[3]s.Context, id string) error {
	return c.runtime.DeleteFilter(ctx, id)
}

// Batch runs several mutations in one transaction, all or nothing (POST /batch, D33, D47);
// build the calls with the Batch functions of the mutations.
func (c *Client) Batch(ctx %[3]s.Context, calls []%[4]s.BatchCall, options *%[2]s.CallOptions) ([]%[4]s.BatchResult, error) {
	return c.runtime.Batch(ctx, calls, options)
}

func idempotencyKey(options *%[2]s.CallOptions) string {
	if options == nil {
		return ""
	}
	return options.IdempotencyKey
}
`

// parametersStruct writes the struct of a parameter set and its wire method, and reports
// whether there is one.
func (g *goClient) parametersStruct(source *goSource, name, description string, parameters []wireParameter) (bool, error) {
	if len(parameters) == 0 {
		return false, nil
	}
	runtime := source.file.use(clientPath, "client")
	var fields, statements strings.Builder
	seen := map[string]bool{}
	for _, parameter := range parameters {
		field := goIdentifier(parameter.name)
		if seen[field] {
			return false, fmt.Errorf("generate: %s: two parameters are both the Go field %s", name, field)
		}
		seen[field] = true
		usage := parameter.usage
		if parameter.required {
			usage += "; required"
		}
		fmt.Fprintf(&fields, "\t// %s: %s.\n\t%s %s\n", field, usage, field, g.fieldType(source.file, parameter))
		statements.WriteString(wireStatement(runtime, parameter, field))
	}
	fmt.Fprintf(&source.body, "\n// %s %s.\ntype %s struct {\n%s}\n", name, description, name, fields.String())
	fmt.Fprintf(&source.body, "\nfunc (parameters %s) wire() (%s.Parameters, error) {\n\twire := %s.Parameters{}\n%s\treturn wire, nil\n}\n", name, runtime, runtime, statements.String())
	return true, nil
}

func (g *goClient) fieldType(file *goFile, parameter wireParameter) string {
	var fieldType string
	switch parameter.kind {
	case valueInteger:
		fieldType = "int64"
		if parameter.page {
			fieldType = "int"
		}
	case valueBoolean:
		fieldType = "bool"
	case valueUUID:
		fieldType = file.use(uuidPath, "uuid") + ".UUID"
	case valueTime:
		fieldType = file.use("time", "time") + ".Time"
	case valueList:
		if parameter.filters != nil {
			return "[]" + parameter.filters.name + "SortTerm"
		}
		return "[]string"
	case valueFilter:
		return "*" + parameter.filters.filterType()
	case valueString:
		fieldType = "string"
		if parameter.filters != nil {
			fieldType = parameter.filters.name + "DistinctField"
		}
	}
	if parameter.pointer {
		return "*" + fieldType
	}
	return fieldType
}

func wireStatement(runtime string, parameter wireParameter, field string) string {
	switch {
	case parameter.kind == valueList && parameter.filters != nil:
		return fmt.Sprintf("\twire.Set(%q, %s.Strings(parameters.%s))\n", parameter.name, runtime, field)
	case parameter.kind == valueString && parameter.filters != nil:
		return fmt.Sprintf("\twire.Set(%q, string(parameters.%s))\n", parameter.name, field)
	case parameter.kind == valueFilter:
		return fmt.Sprintf("\tif parameters.%[1]s != nil {\n\t\tif err := wire.SetJSON(%[2]q, parameters.%[1]s); err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t}\n", field, parameter.name)
	case parameter.path:
		return fmt.Sprintf("\twire.SetPath(%q, parameters.%s)\n", parameter.name, field)
	default:
		return fmt.Sprintf("\twire.Set(%q, parameters.%s)\n", parameter.name, field)
	}
}

// call is what one generated method needs: its parameters argument and the statements that
// build the wire parameters, returning zero on failure.
type call struct {
	argument string
	prepare  string
}

func callOf(runtime, parametersType string, present bool, zero string) call {
	if !present {
		return call{prepare: "\tvar wire " + runtime + ".Parameters\n"}
	}
	failure := "err"
	if zero != "" {
		failure = zero + ", err"
	}
	return call{argument: ", parameters " + parametersType, prepare: "\twire, err := parameters.wire()\n\tif err != nil {\n\t\treturn " + failure + "\n\t}\n"}
}

// dataType is the Go type a read answers and whether its method returns a pointer to it.
func dataType(file *goFile, read *readModel) (string, bool, error) {
	item, err := file.expression(read.item)
	if err != nil {
		return "", false, err
	}
	livewireAlias := file.use(livewirePath, "livewire")
	switch {
	case read.kind == livewire.ReadHistory:
		return livewireAlias + ".ListResponse[" + livewireAlias + ".Version[" + item + "]]", true, nil
	case read.list:
		return livewireAlias + ".ListResponse[" + item + "]", true, nil
	case read.kind == livewire.ReadDistinct:
		return "[]" + item, false, nil
	default:
		return item, returnsPointer(read.item), nil
	}
}

// returnsPointer reports whether a method answers a value of the type through a pointer:
// everything but the types that are references already.
func returnsPointer(goType reflect.Type) bool {
	switch goType.Kind() {
	case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Interface:
		return false
	default:
		return true
	}
}

// result is the result type and the answer statements of a method whose response is
// decoded into a local of goType.
func result(expression string, pointer bool) (resultType, zero, answer string) {
	if pointer {
		return "*" + expression, "nil", "&response"
	}
	return expression, "nil", "response"
}

func (g *goClient) read(source *goSource, read *readModel) error {
	parameters := wireParameters(read.parameters, read.reserved)
	description := "are the parameters of " + goReadName("Get", read)
	if read.live {
		description += " and " + goReadName("Subscribe", read)
	}
	present, err := g.parametersStruct(source, read.queryType, description, parameters)
	if err != nil {
		return fmt.Errorf("%w (the read %s)", err, read.targetName())
	}
	if err := refuseUndecodable(read.item); err != nil {
		return fmt.Errorf("%w (the read %s)", err, read.targetName())
	}
	data, pointer, err := dataType(source.file, read)
	if err != nil {
		return fmt.Errorf("%w (the read %s)", err, read.targetName())
	}
	resultType, zero, answer := result(data, pointer)
	context, runtime := source.file.use("context", "context"), source.file.use(clientPath, "client")
	invocation := callOf(runtime, read.queryType, present, zero)
	fmt.Fprintf(&source.body, "\n// %s reads GET %s%s, the socket target %q.\nfunc (c *Client) %s(ctx %s.Context%s) (%s, error) {\n%s\tvar response %s\n\tif err := c.runtime.Read(ctx, %q, wire, &response); err != nil {\n\t\treturn %s, err\n\t}\n\treturn %s, nil\n}\n",
		goReadName("Get", read), g.api.basePath, read.path, read.targetName(), goReadName("Get", read), context, invocation.argument, resultType, invocation.prepare, data, read.path, zero, answer)
	if !read.live {
		return nil
	}
	fmt.Fprintf(&source.body, "\n// %s subscribes to the socket target %q (%s.Subscribe): its pushes\n// arrive on the channel until ctx ends or one of kind error.\nfunc (c *Client) %s(ctx %s.Context%s) (<-chan %s.Push[%s], error) {\n%s\treturn %s.Subscribe[%s](ctx, c.runtime, %q, wire)\n}\n",
		goReadName("Subscribe", read), read.targetName(), runtime, goReadName("Subscribe", read), context, invocation.argument, runtime, data, invocation.prepare, runtime, data, read.targetName())
	return nil
}

func (g *goClient) mutation(source *goSource, mutation *mutationModel) error {
	name := pascalCase(mutation.name)
	present, err := g.parametersStruct(source, mutation.parametersType, "are the parameters of "+name, wireParameters(mutation.parameters, reservedModel{}))
	if err != nil {
		return err
	}
	file := source.file
	context, runtime := file.use("context", "context"), file.use(clientPath, "client")
	bodyArgument, bodyField, bodyType := "", "", ""
	if mutation.requestType != nil {
		if bodyType, err = file.expression(mutation.requestType); err != nil {
			return fmt.Errorf("%w (the mutation %s)", err, mutation.name)
		}
		bodyArgument, bodyField = ", body "+bodyType, ", Body: body"
	}
	resultType, zero, answer, decode, responseType := "error", "", "", "nil", ""
	if mutation.responseType != nil {
		if err := refuseUndecodable(mutation.responseType); err != nil {
			return fmt.Errorf("%w (the mutation %s)", err, mutation.name)
		}
		if responseType, err = file.expression(mutation.responseType); err != nil {
			return fmt.Errorf("%w (the mutation %s)", err, mutation.name)
		}
		var pointerResult string
		pointerResult, zero, answer = result(responseType, returnsPointer(mutation.responseType))
		resultType, decode = "("+pointerResult+", error)", "&response"
	}
	invocation := callOf(runtime, mutation.parametersType, present, zero)
	fill := ""
	if mutation.clientID != livewire.ClientIDNone {
		fill = "\tbody = with" + name + "ClientID(body)\n"
	}
	sendCall := fmt.Sprintf("c.runtime.Call(ctx, &%s.Call{Method: %q, Path: %q, Parameters: wire%s, IdempotencyKey: idempotencyKey(options)}, %s)", runtime, mutation.method, mutation.path, bodyField, decode)
	clientIDNote := ""
	if mutation.clientID != livewire.ClientIDNone {
		clientIDNote = fmt.Sprintf("\n// A body without an id gets a UUID v%d (D18); Prepare%s makes one to send again.", clientIDVersion(mutation.clientID), name)
	}
	fmt.Fprintf(&source.body, "\n// %s calls %s %s%s, answering %d, under the Idempotency-Key of\n// options (D16; a fresh one for nil).%s\nfunc (c *Client) %s(ctx %s.Context%s%s, options *%s.CallOptions) %s {\n%s%s",
		name, mutation.method, g.api.basePath, mutation.path, mutation.status, clientIDNote, name, context, invocation.argument, bodyArgument, runtime, resultType, invocation.prepare, fill)
	if mutation.responseType == nil {
		fmt.Fprintf(&source.body, "\treturn %s\n}\n", sendCall)
	} else {
		fmt.Fprintf(&source.body, "\tvar response %s\n\tif err := %s; err != nil {\n\t\treturn %s, err\n\t}\n\treturn %s, nil\n}\n", responseType, sendCall, zero, answer)
	}
	if err := g.batchCall(source, mutation, present, bodyType); err != nil {
		return err
	}
	if mutation.clientID == livewire.ClientIDNone {
		return nil
	}
	return g.clientID(source, mutation, bodyType)
}

func clientIDVersion(version livewire.ClientIDVersion) int {
	if version == livewire.UUIDv4 {
		return 4
	}
	return 7
}

// batchCall writes the function that makes a mutation one call of a batch.
func (g *goClient) batchCall(source *goSource, mutation *mutationModel, present bool, bodyType string) error {
	name := pascalCase(mutation.name)
	livewireAlias := source.file.use(livewirePath, "livewire")
	var arguments []string
	var body strings.Builder
	fmt.Fprintf(&body, "\tcall := %s.BatchCall{Target: %q}\n", livewireAlias, mutation.name)
	if present {
		arguments = append(arguments, "parameters "+mutation.parametersType)
		fmt.Fprintf(&body, "\twire, err := parameters.wire()\n\tif err != nil {\n\t\treturn call, err\n\t}\n\tcall.Parameters = wire\n")
	}
	if bodyType != "" {
		arguments = append(arguments, "body "+bodyType)
		if mutation.clientID != livewire.ClientIDNone {
			fmt.Fprintf(&body, "\tbody = with%sClientID(body)\n", name)
		}
		fmt.Fprintf(&body, "\tencoded, err := %s.Marshal(body)\n\tif err != nil {\n\t\treturn call, err\n\t}\n\tcall.Body = encoded\n", source.file.use("encoding/json", "json"))
	}
	fmt.Fprintf(&source.body, "\n// Batch%s is a %s call as one call of a batch (D47, Client.Batch).\nfunc Batch%s(%s) (%s.BatchCall, error) {\n%s\treturn call, nil\n}\n",
		name, name, name, strings.Join(arguments, ", "), livewireAlias, body.String())
	return nil
}

// clientID writes the Prepare function of a ClientID create and the fill-in of its id
// (D18): the body's JSON field "id" must be a uuid.UUID or of a string kind, or a pointer
// to one, reached without an embedded pointer.
func (g *goClient) clientID(source *goSource, mutation *mutationModel, bodyType string) error {
	name := pascalCase(mutation.name)
	field, selector, err := clientIDField(mutation.requestType)
	if err != nil {
		return fmt.Errorf("%w (the mutation %s)", err, mutation.name)
	}
	runtime := source.file.use(clientPath, "client")
	generator := runtime + ".NewUUIDv" + strconv.Itoa(clientIDVersion(mutation.clientID)) + "()"
	fieldType := field.StructField.Type
	pointer := fieldType.Kind() == reflect.Pointer
	if pointer {
		fieldType = fieldType.Elem()
	}
	value := generator
	if fieldType != uuidType {
		expression, err := source.file.expression(fieldType)
		if err != nil {
			return err
		}
		value = expression + "(" + generator + ".String())"
	}
	var fill string
	switch {
	case pointer:
		fill = fmt.Sprintf("\tif %s == nil {\n\t\tid := %s\n\t\t%s = &id\n\t}\n", selector, value, selector)
	case fieldType == uuidType:
		fill = fmt.Sprintf("\tif %s == %s.Nil {\n\t\t%s = %s\n\t}\n", selector, source.file.use(uuidPath, "uuid"), selector, value)
	default:
		fill = fmt.Sprintf("\tif %s == \"\" {\n\t\t%s = %s\n\t}\n", selector, selector, value)
	}
	fmt.Fprintf(&source.body, "\n// Prepare%s prepares a %s call to send again (D16, D18): the body with\n// its id (a fresh UUID v%d when it has none) and options with a fresh Idempotency-Key;\n// pass both to every attempt.\nfunc Prepare%s(body %s) (%s, %s.CallOptions) {\n\treturn with%sClientID(body), %s.CallOptions{IdempotencyKey: %s.NewIdempotencyKey()}\n}\n",
		name, name, clientIDVersion(mutation.clientID), name, bodyType, bodyType, runtime, name, runtime, runtime)
	fmt.Fprintf(&source.body, "\nfunc with%sClientID(body %s) %s {\n%s\treturn body\n}\n", name, bodyType, bodyType, fill)
	return nil
}

// clientIDField finds the field of a create's body that carries its JSON "id" and the
// selector that reaches it through every embedded struct on its way, so a Go field name two
// embedded structs share stays unambiguous.
func clientIDField(requestType reflect.Type) (jsonfield.Field, string, error) {
	var fields []jsonfield.Field
	if requestType.Kind() == reflect.Struct {
		fields = jsonfield.Of(requestType)
	}
	for _, field := range fields {
		fieldType := field.StructField.Type
		if fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		fits := fieldType == uuidType || (fieldType.Kind() == reflect.String && !marshalsItselfAnyway(fieldType))
		if field.Name != "id" || !fits || reachedThroughPointer(requestType, field.StructField.Index) {
			continue // a nil embedded pointer has no field to fill
		}
		if selector, reachable := fieldSelector(requestType, field.StructField.Index); reachable {
			return field, selector, nil
		}
	}
	return jsonfield.Field{}, "", fmt.Errorf("generate: the Go client fills in a client id (D18) only in a struct body whose JSON field id is a uuid.UUID or a string kind (or a pointer to one), reached through exported fields and no embedded pointer; %s has none", requestType)
}

// fieldSelector is the selector of a field by its index path, and whether another package
// can write it: "body.ID" when Go's own selector resolves the name to that path (promotion
// through unexported embedded structs included), else the full path "body.Outer.Inner.ID",
// every step exported. The path has no embedded pointer (reachedThroughPointer).
func fieldSelector(structType reflect.Type, index []int) (string, bool) {
	leaf := structType.FieldByIndex(index)
	if promoted, ok := structType.FieldByName(leaf.Name); ok && leaf.IsExported() && slices.Equal(promoted.Index, index) {
		return "body." + leaf.Name, true
	}
	selector, current := "body", structType
	for _, position := range index {
		field := current.Field(position)
		if !field.IsExported() || !token.IsIdentifier(field.Name) {
			return "", false
		}
		selector += "." + field.Name
		current = field.Type
	}
	return selector, true
}

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// decodesItself mirrors how encoding/json decodes a type with methods (its indirect): an
// UnmarshalJSON on the pointer decodes anything, so nothing below it matters; an UnmarshalText
// decodes a JSON string, which a MarshalText writes (and a MarshalJSON may), but a value
// encoding/json writes by its kind (an object, an array, a number) it refuses to read back
// into UnmarshalText; a MarshalText of a string kind is read back as the string. It reports
// whether the methods decide the type, and the refusal when they cannot decode it.
func decodesItself(goType reflect.Type, path string) (bool, error) {
	pointer := reflect.PointerTo(goType)
	textDecoder := pointer.Implements(textUnmarshalerType)
	jsonEncoder := implements(goType, jsonMarshalerType, false)
	switch {
	case pointer.Implements(jsonUnmarshalerType):
		return true, nil
	case marshalsItselfAnyway(goType) && (textDecoder || (!jsonEncoder && goType.Kind() == reflect.String)):
		return true, nil
	case jsonEncoder:
		return true, fmt.Errorf("generate: the Go client cannot decode %s: it has MarshalJSON but neither UnmarshalJSON nor UnmarshalText", path)
	case marshalsItselfAnyway(goType):
		return true, fmt.Errorf("generate: the Go client cannot decode %s: it has MarshalText but no UnmarshalText", path)
	case textDecoder && goType.Kind() != reflect.String:
		return true, fmt.Errorf("generate: the Go client cannot decode %s: encoding/json writes it as its %s and reads only a JSON string into its UnmarshalText", path, goType.Kind())
	default:
		return false, nil
	}
}

// refuseUndecodable refuses a type the Go client cannot decode what the server wrote into:
// one that marshals itself (MarshalJSON, MarshalText) without the matching unmarshaler on its
// pointer, a map key that is text without UnmarshalText, an interface with methods, at any
// depth of its JSON fields, elements and values.
func refuseUndecodable(goType reflect.Type) error {
	return undecodable(goType, goType.String(), map[reflect.Type]bool{})
}

func undecodable(goType reflect.Type, path string, visited map[reflect.Type]bool) error {
	if visited[goType] {
		return nil
	}
	visited[goType] = true
	if goType.Kind() == reflect.Pointer {
		return undecodable(goType.Elem(), path, visited) // encoding/json allocates and decodes the value
	}
	if decodes, err := decodesItself(goType, path); decodes || err != nil {
		return err
	}
	switch goType.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return undecodable(goType.Elem(), path+"[]", visited)
	case reflect.Map:
		key := goType.Key()
		textKey := key.Kind() != reflect.String && implements(key, textMarshalerType, false)
		if textKey && !reflect.PointerTo(key).Implements(textUnmarshalerType) {
			return fmt.Errorf("generate: the Go client cannot decode the keys of %s: %s has MarshalText but no UnmarshalText", path, key)
		}
		return undecodable(goType.Elem(), path+"[]", visited)
	case reflect.Interface:
		if goType.NumMethod() > 0 {
			return fmt.Errorf("generate: the Go client cannot decode %s: an interface with methods", path)
		}
	case reflect.Struct:
		for _, field := range jsonfield.Of(goType) {
			if err := undecodable(field.StructField.Type, path+"."+field.StructField.Name, visited); err != nil {
				return err
			}
		}
	default:
	}
	return nil
}

func (g *goClient) upload(source *goSource, upload *uploadModel) error {
	name := pascalCase(upload.name)
	present, err := g.parametersStruct(source, upload.parametersType, "are the parameters of "+name, wireParameters(upload.parameters, reservedModel{}))
	if err != nil {
		return err
	}
	file := source.file
	if err := refuseUndecodable(upload.responseType); err != nil {
		return fmt.Errorf("%w (the upload %s)", err, upload.name)
	}
	expression, err := file.expression(upload.responseType)
	if err != nil {
		return fmt.Errorf("%w (the upload %s)", err, upload.name)
	}
	resultType, zero, answer := result(expression, returnsPointer(upload.responseType))
	context, runtime, livewireAlias := file.use("context", "context"), file.use(clientPath, "client"), file.use(livewirePath, "livewire")
	invocation := callOf(runtime, upload.parametersType, present, zero)
	fmt.Fprintf(&source.body, "\n// %s uploads to POST %s%s (D17), answering %d:\n// file.Body is sent raw with file.Size as its Content-Length (at most %d bytes) and\n// file.ContentType, one of %s.\nfunc (c *Client) %s(ctx %s.Context%s, file %s.File, options *%s.CallOptions) (%s, error) {\n%s\tvar response %s\n\tif err := c.runtime.Upload(ctx, &%s.Upload{Path: %q, Parameters: wire, File: file, IdempotencyKey: idempotencyKey(options)}, &response); err != nil {\n\t\treturn %s, err\n\t}\n\treturn %s, nil\n}\n",
		name, g.api.basePath, upload.path, upload.status, upload.maximumBytes, strings.Join(upload.contentTypes, ", "),
		name, context, invocation.argument, livewireAlias, runtime, resultType, invocation.prepare, expression, runtime, upload.path, zero, answer)
	return nil
}

func (g *goClient) export(source *goSource, export *exportModel) error {
	name := "Export" + pascalCase(export.declaration)
	present, err := g.parametersStruct(source, export.queryType, "are the parameters of "+name, wireParameters(nil, export.reserved))
	if err != nil {
		return err
	}
	context, input, runtime := source.file.use("context", "context"), source.file.use("io", "io"), source.file.use(clientPath, "client")
	invocation := callOf(runtime, export.queryType, present, "0")
	fmt.Fprintf(&source.body, "\n// %s streams GET %s%s as CSV into writer (D35) and answers the\n// rows it wrote; a cut export fails (D60).\nfunc (c *Client) %s(ctx %s.Context%s, writer %s.Writer) (int, error) {\n%s\treturn c.runtime.Export(ctx, %q, wire, writer)\n}\n",
		name, g.api.basePath, export.path, name, context, invocation.argument, input, invocation.prepare, export.path)
	return nil
}

// filterFunction is one generated condition builder: a field and one of its operators.
type filterFunction struct {
	name     string
	resource string
	field    query.Field
	operator query.Operator
	operand  *shape
}

// goOperatorNames spell the operators as the query package's constants do.
var goOperatorNames = map[query.Operator]string{
	query.OperatorEqual: "Equal", query.OperatorNotEqual: "NotEqual", query.OperatorGreater: "Greater",
	query.OperatorGreaterOrEqual: "GreaterOrEqual", query.OperatorLess: "Less", query.OperatorLessOrEqual: "LessOrEqual",
	query.OperatorIn: "In", query.OperatorNotIn: "NotIn", query.OperatorContains: "Contains", query.OperatorStartsWith: "StartsWith",
	query.OperatorIsNull: "IsNull", query.OperatorHas: "Has", query.OperatorHasAny: "HasAny", query.OperatorHasAll: "HasAll",
	query.OperatorIsEmpty: "IsEmpty",
}

// filterFunctions are a resource's condition builders, one per filterable field and
// operator, from the conditions both TypeScript and OpenAPI render (D12).
func filterFunctions(resource *resourceModel) []filterFunction {
	var functions []filterFunction
	for _, member := range resource.conditions() {
		field, _ := resource.fields.Lookup(member.field)
		for _, operator := range member.operators {
			functions = append(functions, filterFunction{
				name: resource.name + goIdentifier(member.field) + goOperatorNames[operator], resource: resource.name, field: field, operator: operator, operand: member.operand,
			})
		}
	}
	return functions
}

func (g *goClient) filtersFile() ([]byte, error) {
	source := g.newSource()
	for _, resource := range g.filterResources() {
		g.resourceFilters(source, resource)
	}
	documentation := fmt.Sprintf("// The filter documents of %s (D12): per filterable read a field type, a document type and\n// one condition function per field and operator.\n\n", g.options.Package)
	return g.finish("filters.go", documentation, source)
}

func (g *goClient) resourceFilters(source *goSource, resource *resourceModel) {
	runtime := source.file.use(clientPath, "client")
	description := "the resource " + resource.target
	if resource.history {
		description = "every history read (HistoryFields)"
	}
	fieldType, node := resource.name+"Field", runtime+".Node["+resource.name+"Field]"
	fmt.Fprintf(&source.body, "\n// %s is a field of %s a filter may name.\ntype %s string\n", fieldType, description, fieldType)
	search := ""
	if resource.searchable() {
		search = "\n\tSearch string `json:\"search,omitempty\"`"
	}
	fmt.Fprintf(&source.body, "\n// %s is a filter document of %s: its conditions under Where, combined with\n// %s.And, Or and Not; the zero value matches every row.\ntype %s struct {\n\tWhere *%s `json:\"where,omitempty\"`%s\n}\n",
		resource.filterType(), description, runtime, resource.filterType(), node, search)
	for _, declared := range filterTypes(resource) {
		fmt.Fprintf(&source.body, "\n// %s is %s.\ntype %s string\n", declared.name, declared.description, declared.name)
		if len(declared.constants) == 0 {
			continue
		}
		source.body.WriteString("\n// The values of " + declared.name + ".\nconst (\n")
		for _, constant := range declared.constants {
			fmt.Fprintf(&source.body, "\t%s %s = %q\n", constant.name, declared.name, constant.value)
		}
		source.body.WriteString(")\n")
	}
	for _, function := range filterFunctions(resource) {
		operandType, argument, value := g.operand(source.file, function)
		fmt.Fprintf(&source.body, "\n// %s is the condition %s %s.\nfunc %s(%s %s) %s {\n\treturn %s.Condition(%s(%q), %s.Operator%s, %s)\n}\n",
			function.name, function.field.Name, function.operator, function.name, argument, operandType, node, runtime, fieldType, function.field.Name, source.file.use(queryPath, "query"), goOperatorNames[function.operator], value)
	}
}

// operand is the Go type of a condition's operand, its argument name and the value passed
// on: a list operator takes its values variadic, a flag operator a bool.
func (g *goClient) operand(file *goFile, function filterFunction) (operandType, argument, value string) {
	scalar := scalarOperand(file, function.field.Type)
	if function.field.Type == query.TypeEnum {
		scalar = function.resource + goIdentifier(function.field.Name)
	}
	if function.operand.kind == shapeArray {
		return "..." + scalar, "values", "values"
	}
	if function.operand.kind == shapeBoolean {
		return "bool", "value", "value"
	}
	return scalar, "value", "value"
}

// filterType is a string type the filter document of a resource names, with its values:
// an enum field's values, the sort terms, the distinct fields.
type filterType struct {
	name        string
	description string
	constants   []filterConstant
}

type filterConstant struct {
	name  string
	value string
}

// filterTypes are the typed strings of a resource's filter vocabulary, as filters.ts has
// them: one per enum field (<Resource><Field>), the sort terms (<Resource>SortTerm) and,
// except for history, the distinct fields (<Resource>DistinctField).
func filterTypes(resource *resourceModel) []filterType {
	taken := map[string]bool{resource.name + "Field": true, resource.filterType(): true}
	for _, function := range filterFunctions(resource) {
		taken[function.name] = true
	}
	for _, field := range resource.fields {
		if field.Filter && field.Type == query.TypeEnum {
			taken[resource.name+goIdentifier(field.Name)] = true
		}
	}
	taken[resource.name+"SortTerm"], taken[resource.name+"DistinctField"] = true, true
	// constant numbers a name another identifier of the vocabulary took ("nut" and "Nut",
	// "+1" and "1"), in declaration order, so every value keeps a constant.
	constant := func(base, value string) filterConstant {
		name := base
		for number := 2; taken[name]; number++ {
			name = base + strconv.Itoa(number)
		}
		taken[name] = true
		return filterConstant{name: name, value: value}
	}
	var declared []filterType
	for _, field := range resource.fields {
		if !field.Filter || field.Type != query.TypeEnum {
			continue
		}
		name := resource.name + goIdentifier(field.Name)
		enum := filterType{name: name, description: "a value of the enum field " + field.Name}
		for _, value := range field.Enum {
			enum.constants = append(enum.constants, constant(name+goIdentifier(value), value))
		}
		declared = append(declared, enum)
	}
	sort := filterType{name: resource.name + "SortTerm", description: "a sort term: a sortable field, with a leading - descending"}
	for _, field := range resource.sortFields() {
		sort.constants = append(sort.constants,
			constant(resource.name+"Sort"+goIdentifier(field), field), constant(resource.name+"Sort"+goIdentifier(field)+"Descending", "-"+field))
	}
	declared = append(declared, sort)
	if !resource.history {
		distinct := filterType{name: resource.name + "DistinctField", description: "a field a distinct read counts the values of (D15)"}
		for _, field := range resource.distinctFields() {
			distinct.constants = append(distinct.constants, constant(resource.name+"Distinct"+goIdentifier(field), field))
		}
		declared = append(declared, distinct)
	}
	return declared
}

func scalarOperand(file *goFile, fieldType query.FieldType) string {
	switch fieldType {
	case query.TypeInteger:
		return "int64"
	case query.TypeFloat:
		return "float64"
	case query.TypeBoolean:
		return "bool"
	case query.TypeTime:
		return file.use(clientPath, "client") + ".Time"
	case query.TypeUUID:
		return file.use(uuidPath, "uuid") + ".UUID"
	case query.TypeString, query.TypeText, query.TypeEnum, query.TypeStringArray, query.TypeJSON:
	}
	return "string"
}

// renderGo renders the Go client and the command-line client the options ask for.
func renderGo(api *model, options Options) ([]file, error) {
	var files []file
	if options.GoClient.Directory != "" {
		generated, err := renderGoClient(api, options.GoClient)
		if err != nil {
			return nil, err
		}
		for _, generatedFile := range generated {
			files = append(files, file{path: filepath.Join(options.GoClient.Directory, generatedFile.path), content: generatedFile.content})
		}
	}
	if options.CommandLine.Directory != "" {
		content, err := renderCommandLine(api, options.CommandLine)
		if err != nil {
			return nil, err
		}
		files = append(files, file{path: filepath.Join(options.CommandLine.Directory, "command.go"), content: content})
	}
	return files, nil
}

func resolveGoClient(options *GoClientOptions) error {
	if options.Directory == "" {
		return nil
	}
	path := options.ImportPath
	malformed := path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.ContainsAny(path, " \\\t\n") || strings.Contains(path, "//")
	if malformed {
		return fmt.Errorf("generate: GoClient.ImportPath %q is no import path; it names the package GoClient.Directory holds", path)
	}
	if options.Package == "" {
		options.Package = path[strings.LastIndex(path, "/")+1:]
	}
	return validatePackageName("GoClient.Package", options.Package)
}

func validatePackageName(option, name string) error {
	if !token.IsIdentifier(name) || name == "main" || strings.ToLower(name) != name {
		return fmt.Errorf("generate: %s %q is no lower-case package name (other than main); set it", option, name)
	}
	return nil
}
