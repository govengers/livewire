package generate

import (
	"fmt"
	"strings"

	"github.com/govengers/livewire"
)

// apiNames renders types as api.ts refers to them: DTOs from types.ts, the runtime's generic
// types from runtime.ts.
var apiNames = typescriptNames{types: "types.", runtime: "runtime."}

// renderAPI writes api.ts: per read its parameters interface, its descriptor (in reads, and
// in live when the socket can subscribe to it) and a REST function; per mutation and upload
// the same with calls and uploads; per export a URL and a download function; and the
// library's own routes, the stored filters and the batch.
func renderAPI(api *model) ([]byte, error) {
	var builder strings.Builder
	builder.WriteString(typescriptHeader)
	builder.WriteString("import type * as filters from './filters'\nimport * as runtime from './runtime'\nimport type * as types from './types'\n")
	identifiers := newIdentifiers("filters", "runtime", "types", "reads", "live", "calls", "uploads", "downloads", "saveFilter", "listFilters", "deleteFilter", "batch")
	if err := renderReads(&builder, api, identifiers); err != nil {
		return nil, err
	}
	if err := renderMutations(&builder, api, identifiers); err != nil {
		return nil, err
	}
	if err := renderUploads(&builder, api, identifiers); err != nil {
		return nil, err
	}
	if err := renderExports(&builder, api, identifiers); err != nil {
		return nil, err
	}
	builder.WriteString(strings.ReplaceAll(libraryRoutes, "{base}", api.basePath))
	return []byte(builder.String()), nil
}

// libraryRoutes are the functions of the library's own routes; {base} is the BasePath.
const libraryRoutes = `
/** POST {base}/filters: stores a filter (D13); an unnamed one is a registration the client remembers (D54). */
export function saveFilter<Target extends keyof filters.FilterDocuments>(
  client: runtime.Client,
  request: runtime.FilterRequest<filters.FilterDocuments[Target]> & { resource: Target },
  options?: runtime.RequestOptions,
): Promise<runtime.FilterItem<filters.FilterDocuments[Target]>> {
  return client.saveFilter(request, options)
}

/** GET {base}/filters: the caller's named filters, of one target or of every one. */
export function listFilters(client: runtime.Client, resource?: keyof filters.FilterDocuments, options?: runtime.RequestOptions): Promise<runtime.ListResponse<runtime.FilterItem>> {
  return client.listFilters(resource, options)
}

/** DELETE {base}/filters/{id}: removes one of the caller's filters. */
export function deleteFilter(client: runtime.Client, id: string, options?: runtime.RequestOptions): Promise<void> {
  return client.deleteFilter(id, options)
}

/** POST {base}/batch: several mutations in one transaction, all or nothing (D33, D47); build each call with runtime.batchCall. */
export function batch(client: runtime.Client, batchCalls: readonly runtime.BatchCall[], options?: runtime.CallOptions): Promise<runtime.BatchResult[]> {
  return client.batch(batchCalls, options)
}
`

// member is one member of a parameters interface.
type member struct {
	text     string
	required bool
}

// parametersInterface writes the interface of a parameters set, or an alias of
// runtime.NoParameters without any, and reports whether a caller must pass it.
func parametersInterface(builder *strings.Builder, name string, parameters []parameterModel, reserved reservedModel) (required bool) {
	members := parameterMembers(parameters, reserved)
	if len(members) == 0 {
		fmt.Fprintf(builder, "\nexport type %s = runtime.NoParameters\n", name)
		return false
	}
	fmt.Fprintf(builder, "\nexport interface %s {\n", name)
	for _, entry := range members {
		builder.WriteString("  " + entry.text + "\n")
		required = required || entry.required
	}
	builder.WriteString("}\n")
	return required
}

func parameterMembers(parameters []parameterModel, reserved reservedModel) []member {
	var members []member
	add := func(name, expression string, required bool) {
		optional := "?"
		if required {
			optional = ""
		}
		members = append(members, member{text: propertyName(name) + optional + ": " + expression, required: required})
	}
	for _, parameter := range parameters {
		add(parameter.name, apiNames.operandExpression(parameter.shape), parameter.required)
	}
	if reserved.filters != nil {
		add(livewire.ParameterFilter, "filters."+reserved.filters.filterType(), false)
		add(livewire.ParameterFilterID, "string", false)
	}
	if reserved.field {
		add(livewire.ParameterField, "filters."+reserved.filters.name+"DistinctField", true)
	}
	if reserved.sort {
		add(livewire.ParameterSort, "readonly filters."+reserved.filters.name+"SortTerm[]", false)
	}
	if reserved.page {
		add(livewire.ParameterLimit, "number", false)
		add(livewire.ParameterOffset, "number", false)
	}
	if reserved.after {
		add(livewire.ParameterAfter, "string", false)
	}
	if reserved.format {
		add(livewire.ParameterFormat, quote(livewire.ExportCSV), false)
	}
	return members
}

// parametersArgument is the parameters argument of a generated function and what it passes
// on: none for a set without parameters, a default {} for one without required parameters
// unless another required argument follows.
func parametersArgument(typeName string, members, required bool) (argument, passed string) {
	switch {
	case !members:
		return "", "{}"
	case required:
		return ", parameters: " + typeName, "parameters"
	default:
		return ", parameters: " + typeName + " = {}", "parameters"
	}
}

func hasMembers(parameters []parameterModel, reserved reservedModel) bool {
	return len(parameterMembers(parameters, reserved)) > 0
}

func renderReads(builder *strings.Builder, api *model, identifiers *identifiers) error {
	var descriptors, live strings.Builder
	for _, read := range api.reads {
		owner := "the read " + read.targetName()
		for _, name := range []string{read.queryType, read.functionName()} {
			if err := identifiers.add(name, owner); err != nil {
				return err
			}
		}
		required := parametersInterface(builder, read.queryType, read.parameters, read.reserved)
		argument, passed := parametersArgument(read.queryType, hasMembers(read.parameters, read.reserved), required)
		data := apiNames.expression(read.data)
		key := propertyName(read.targetName())
		access := "reads[" + quote(read.targetName()) + "]"
		if identifierPattern.MatchString(read.targetName()) {
			access = "reads." + read.targetName()
		}
		fmt.Fprintf(builder, "\n/** GET %s%s (socket target %s). */\nexport function %s(client: runtime.Client%s, options?: runtime.RequestOptions): Promise<%s> {\n  return client.read(%s, %s, options)\n}\n",
			api.basePath, read.path, quote(read.targetName()), read.functionName(), argument, data, access, passed)
		constructor := "read"
		if read.live {
			constructor = "liveRead"
			fmt.Fprintf(&live, "  %s: %s,\n", key, access)
		}
		fmt.Fprintf(&descriptors, "  %s: runtime.%s<%s, %s>({ target: %s, path: %s, pathParameters: %s, list: %t }),\n",
			key, constructor, read.queryType, data, quote(read.targetName()), quote(read.path), stringArray(pathParameters(read.parameters)), read.list)
	}
	builder.WriteString("\n/** Every read: its socket target (D46), REST route and kind. */\nexport const reads = {\n" + descriptors.String() + "} as const\n")
	builder.WriteString("\n/** The reads a socket can subscribe to: useLive(live.name) and LiveConnection.subscribe. */\nexport const live = {\n" + live.String() + "} as const\n")
	return nil
}

func renderMutations(builder *strings.Builder, api *model, identifiers *identifiers) error {
	var descriptors strings.Builder
	for _, mutation := range api.mutations {
		owner := "the mutation " + mutation.name
		for _, name := range []string{mutation.parametersType, mutation.name} {
			if err := identifiers.add(name, owner); err != nil {
				return err
			}
		}
		required := parametersInterface(builder, mutation.parametersType, mutation.parameters, reservedModel{})
		argument, passed := parametersArgument(mutation.parametersType, len(mutation.parameters) > 0, required || mutation.body != nil)
		body, bodyArgument, bodyPassed := "undefined", "", "undefined"
		if mutation.body != nil {
			body = apiNames.expression(mutation.body)
			bodyArgument, bodyPassed = ", body: "+body, "body"
		}
		response := "void"
		if mutation.response != nil {
			response = apiNames.expression(mutation.response)
		}
		fmt.Fprintf(builder, "\n/** %s %s%s, answering %d. */\nexport function %s(client: runtime.Client%s%s, options?: runtime.CallOptions): Promise<%s> {\n  return client.call(calls.%s, %s, %s, options)\n}\n",
			mutation.method, api.basePath, mutation.path, mutation.status, mutation.name, argument, bodyArgument, response, mutation.name, passed, bodyPassed)
		var extra string
		if mutation.effect.Kind != "" {
			extra += fmt.Sprintf(", effect: { kind: %s, target: %s }", quote(string(mutation.effect.Kind)), quote(mutation.effect.Target))
		}
		switch mutation.clientID {
		case livewire.UUIDv7:
			extra += ", clientID: 7"
		case livewire.UUIDv4:
			extra += ", clientID: 4"
		case livewire.ClientIDNone:
		}
		fmt.Fprintf(&descriptors, "  %s: runtime.mutation<%s, %s, %s>({ name: %s, method: %s, path: %s, pathParameters: %s, body: %t, content: %t%s }),\n",
			mutation.name, mutation.parametersType, body, response, quote(mutation.name), quote(mutation.method), quote(mutation.path),
			stringArray(pathParameters(mutation.parameters)), mutation.body != nil, mutation.response != nil, extra)
	}
	builder.WriteString("\n/** Every mutation, for LiveConnection.call (the socket, with its optimistic effect, D36) and runtime.batchCall. */\nexport const calls = {\n" + descriptors.String() + "} as const\n")
	return nil
}

func renderUploads(builder *strings.Builder, api *model, identifiers *identifiers) error {
	var descriptors strings.Builder
	for _, upload := range api.uploads {
		owner := "the upload " + upload.name
		for _, name := range []string{upload.parametersType, upload.name} {
			if err := identifiers.add(name, owner); err != nil {
				return err
			}
		}
		parametersInterface(builder, upload.parametersType, upload.parameters, reservedModel{})
		argument, passed := parametersArgument(upload.parametersType, len(upload.parameters) > 0, true)
		contentTypes := literalUnion(upload.contentTypes)
		response := apiNames.expression(upload.response)
		fmt.Fprintf(builder, "\n/** POST %s%s: a raw body, or multipart with the part \"file\"; at most %d bytes, answering %d. */\nexport function %s(client: runtime.Client%s, file: Blob, options?: runtime.UploadOptions<%s>): Promise<%s> {\n  return client.upload(uploads.%s, %s, file, options)\n}\n",
			api.basePath, upload.path, upload.maximumBytes, upload.status, upload.name, argument, contentTypes, response, upload.name, passed)
		fmt.Fprintf(&descriptors, "  %s: runtime.upload<%s, %s, %s>({ name: %s, path: %s, pathParameters: %s, contentTypes: %s }),\n",
			upload.name, upload.parametersType, response, contentTypes, quote(upload.name), quote(upload.path), stringArray(pathParameters(upload.parameters)), stringArray(upload.contentTypes))
	}
	builder.WriteString("\n/** Every upload (D17), REST only. */\nexport const uploads = {\n" + descriptors.String() + "} as const\n")
	return nil
}

func renderExports(builder *strings.Builder, api *model, identifiers *identifiers) error {
	var descriptors strings.Builder
	for _, export := range api.exports {
		owner := "the export of " + export.declaration
		for _, name := range []string{export.queryType, export.urlFunction, export.downloadFunction} {
			if err := identifiers.add(name, owner); err != nil {
				return err
			}
		}
		parametersInterface(builder, export.queryType, nil, export.reserved)
		fmt.Fprintf(builder, "\n/** The URL of GET %s%s, for a link the browser downloads (D35). */\nexport function %s(client: runtime.Client, parameters: %s = {}): string {\n  return client.exportURL(downloads.%s, parameters)\n}\n",
			api.basePath, export.path, export.urlFunction, export.queryType, export.declaration)
		fmt.Fprintf(builder, "\n/** GET %s%s as a blob; a cut export fails (D60). */\nexport function %s(client: runtime.Client, parameters: %s = {}, options?: runtime.RequestOptions): Promise<runtime.ExportDownload> {\n  return client.download(downloads.%s, parameters, options)\n}\n",
			api.basePath, export.path, export.downloadFunction, export.queryType, export.declaration)
		fmt.Fprintf(&descriptors, "  %s: runtime.exportTarget<%s>({ target: %s, path: %s }),\n", export.declaration, export.queryType, quote(export.declaration), quote(export.path))
	}
	builder.WriteString("\n/** Every export (D35), REST only. */\nexport const downloads = {\n" + descriptors.String() + "} as const\n")
	return nil
}

func stringArray(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
