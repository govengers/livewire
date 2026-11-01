package generate

import (
	"embed"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/govengers/livewire"
)

// The runtime sources the generator copies; they are tested where they live, under bun.
//
//go:embed runtime/runtime.ts runtime/vue.ts
var runtimeSources embed.FS

const typescriptHeader = "// " + header + "\n\n"

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	// reservedWords are the words of JavaScript and TypeScript no generated identifier may be.
	reservedWords = []string{
		"await", "break", "case", "catch", "class", "const", "continue", "debugger", "default", "delete", "do", "else",
		"enum", "export", "extends", "false", "finally", "for", "function", "if", "implements", "import", "in",
		"instanceof", "interface", "let", "new", "null", "package", "private", "protected", "public", "return",
		"static", "super", "switch", "this", "throw", "true", "try", "typeof", "var", "void", "while", "with", "yield",
	}
)

func renderTypeScript(api *model) ([]file, error) {
	filters, err := renderFilters(api)
	if err != nil {
		return nil, err
	}
	apiFile, err := renderAPI(api)
	if err != nil {
		return nil, err
	}
	files := []file{{path: "types.ts", content: renderTypes(api)}, {path: "filters.ts", content: filters}, {path: "api.ts", content: apiFile}}
	for _, name := range []string{"runtime.ts", "vue.ts"} {
		source, err := runtimeSources.ReadFile("runtime/" + name)
		if err != nil {
			return nil, fmt.Errorf("generate: %w", err)
		}
		files = append(files, file{path: name, content: append([]byte(typescriptHeader), source...)})
	}
	return files, nil
}

// typescriptNames renders shapes as TypeScript type expressions; types prefixes a named type
// ("types." in api.ts), runtime the runtime's generic types ("runtime." in api.ts); both are
// empty inside types.ts and filters.ts.
type typescriptNames struct {
	types   string
	runtime string
	// libraries prefixes a library type instead of types when set: "runtime." where the
	// runtime's own declaration is meant.
	libraries string
}

func (n typescriptNames) prefix(named *namedType) string {
	if named.library && n.libraries != "" {
		return n.libraries
	}
	return n.types
}

func (n typescriptNames) expression(value *shape) string {
	var expression string
	switch value.kind {
	case shapeString:
		expression = "string"
	case shapeNumber, shapeInteger:
		expression = "number"
	case shapeBoolean:
		expression = "boolean"
	case shapeUnknown:
		return "unknown"
	case shapeArray:
		expression = n.elementExpression(value.element) + "[]"
	case shapeMap:
		expression = "Record<string, " + n.expression(value.element) + ">"
	case shapeObject:
		expression = n.objectExpression(value.properties)
	case shapeReference:
		expression = n.prefix(value.reference) + value.reference.name
	case shapeEnum:
		expression = literalUnion(value.values)
	case shapeListResponse:
		expression = n.runtime + "ListResponse<" + n.expression(value.element) + ">"
	case shapeVersion:
		expression = n.runtime + "Version<" + n.expression(value.element) + ">"
	case shapeTimeValue:
		expression = "TimeValue"
	}
	if value.nullable {
		return expression + " | null"
	}
	return expression
}

// operandExpression is a parameter's or a condition's value: a list is a readonly array, so a
// caller may pass either kind.
func (n typescriptNames) operandExpression(value *shape) string {
	if value.kind == shapeArray && !value.nullable {
		return "readonly " + n.elementExpression(value.element) + "[]"
	}
	return n.expression(value)
}

func (n typescriptNames) elementExpression(element *shape) string {
	expression := n.expression(element)
	if element.nullable || (element.kind == shapeEnum && len(element.values) > 1) {
		return "(" + expression + ")"
	}
	return expression
}

func (n typescriptNames) objectExpression(properties []property) string {
	if len(properties) == 0 {
		return "Record<string, never>"
	}
	members := make([]string, 0, len(properties))
	for _, member := range properties {
		members = append(members, n.member(member))
	}
	return "{ " + strings.Join(members, "; ") + " }"
}

func (n typescriptNames) member(member property) string {
	optional := ""
	if member.optional {
		optional = "?"
	}
	return propertyName(member.name) + optional + ": " + n.expression(member.shape)
}

func propertyName(name string) string {
	if identifierPattern.MatchString(name) {
		return name
	}
	return strconv.Quote(name)
}

func literalUnion(values []string) string {
	if len(values) == 0 {
		return "never"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quote(value))
	}
	return strings.Join(quoted, " | ")
}

// quote writes a TypeScript string literal in single quotes.
func quote(text string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, " ", ` `, " ", ` `).Replace(text)
	return "'" + escaped + "'"
}

func renderTypes(api *model) []byte {
	var builder strings.Builder
	builder.WriteString(typescriptHeader)
	library := slices.Concat(libraryNames, libraryTypeNames())
	slices.Sort(library)
	builder.WriteString("import type { " + strings.Join(library, ", ") + " } from './runtime'\n\n")
	builder.WriteString("export type { " + strings.Join(library, ", ") + " }\n")
	names := typescriptNames{}
	declared := slices.DeleteFunc(slices.Clone(api.types.ordered), func(named *namedType) bool { return named.library })
	slices.SortFunc(declared, func(left, right *namedType) int { return strings.Compare(left.name, right.name) })
	for _, named := range declared {
		builder.WriteString("\n/** " + goTypeName(named) + " */\n")
		definition := named.definition
		if definition.kind == shapeObject && len(definition.properties) > 0 {
			builder.WriteString("export interface " + named.name + " {\n")
			for _, member := range definition.properties {
				builder.WriteString("  " + names.member(member) + "\n")
			}
			builder.WriteString("}\n")
			continue
		}
		builder.WriteString("export type " + named.name + " = " + names.expression(definition) + "\n")
	}
	return []byte(builder.String())
}

func libraryTypeNames() []string {
	names := make([]string, 0, len(libraryTypes))
	for _, library := range libraryTypes {
		names = append(names, library.name)
	}
	return names
}

func goTypeName(named *namedType) string {
	if named.goType.PkgPath() == "" {
		return named.goType.String()
	}
	return named.goType.PkgPath() + "." + named.goType.Name()
}

func renderFilters(api *model) ([]byte, error) {
	var builder strings.Builder
	builder.WriteString(typescriptHeader)
	builder.WriteString(`import type { RelativeTime } from './runtime'

/** A node of a filter document (D12): a condition, or and, or or not over nodes. */
export type FilterNode<Condition> =
  | Condition
  | { and: readonly FilterNode<Condition>[] }
  | { or: readonly FilterNode<Condition>[] }
  | { not: FilterNode<Condition> }

/** A filter document: its conditions under where; the empty document matches every row. */
export interface FilterDocument<Condition> {
  where?: FilterNode<Condition>
}

/** A filter document of a resource with a searchable field. */
export interface SearchableFilterDocument<Condition> extends FilterDocument<Condition> {
  search?: string
}

/** One sort term: the field, or "-field" for descending. */
export type SortTerm<Field extends string> = Field | ` + "`-${Field}`" + `

/** A time in a condition: RFC 3339, or relative to the database clock of the read. */
export type TimeValue = string | RelativeTime
`)
	identifiers := newIdentifiers("FilterNode", "FilterDocument", "SearchableFilterDocument", "SortTerm", "TimeValue", "RelativeTime", "FilterDocuments")
	resources := slices.Clone(api.resources)
	if api.history != nil {
		resources = append(resources, api.history)
	}
	for _, resource := range resources {
		if err := renderResourceFilters(&builder, resource, identifiers); err != nil {
			return nil, err
		}
	}
	builder.WriteString("\n/** The filter document of every target a stored filter may name (D13, D56). */\nexport interface FilterDocuments {\n")
	for _, read := range api.reads {
		if read.reserved.filters != nil && read.kind != livewire.ReadDistinct {
			builder.WriteString("  " + propertyName(read.targetName()) + ": " + read.reserved.filters.filterType() + "\n")
		}
	}
	builder.WriteString("}\n")
	return []byte(builder.String()), nil
}

func renderResourceFilters(builder *strings.Builder, resource *resourceModel, identifiers *identifiers) error {
	names := []string{resource.conditionType(), resource.filterType(), resource.name + "SortField", resource.name + "SortTerm"}
	if !resource.history {
		names = append(names, resource.name+"DistinctField")
	}
	for _, name := range names {
		if err := identifiers.add(name, "the filter types of "+resource.name); err != nil {
			return err
		}
	}
	operands := typescriptNames{}
	var conditions []string
	for _, member := range resource.conditions() {
		conditions = append(conditions, fmt.Sprintf("{ field: %s; op: %s; value: %s }", quote(member.field), literalUnion(stringsOf(member.operators)), operands.operandExpression(member.operand)))
	}
	description := "the resource " + resource.target
	if resource.history {
		description = "every history read (HistoryFields)"
	}
	fmt.Fprintf(builder, "\n/** The conditions of %s. */\nexport type %s =", description, resource.conditionType())
	if len(conditions) == 0 {
		builder.WriteString(" never\n")
	}
	for _, condition := range conditions {
		builder.WriteString("\n  | " + condition)
	}
	if len(conditions) > 0 {
		builder.WriteString("\n")
	}
	document := "FilterDocument"
	if resource.searchable() {
		document = "SearchableFilterDocument"
	}
	fmt.Fprintf(builder, "export type %s = %s<%s>\n", resource.filterType(), document, resource.conditionType())
	fmt.Fprintf(builder, "export type %sSortField = %s\n", resource.name, literalUnion(resource.sortFields()))
	fmt.Fprintf(builder, "export type %sSortTerm = SortTerm<%sSortField>\n", resource.name, resource.name)
	if !resource.history {
		fmt.Fprintf(builder, "export type %sDistinctField = %s\n", resource.name, literalUnion(resource.distinctFields()))
	}
	return nil
}

// identifiers keeps the names one generated module exports, so two never collide.
type identifiers struct {
	taken map[string]string
}

func newIdentifiers(fixed ...string) *identifiers {
	taken := map[string]string{}
	for _, name := range fixed {
		taken[name] = "the generated code"
	}
	return &identifiers{taken: taken}
}

func (i *identifiers) add(name, owner string) error {
	if slices.Contains(reservedWords, name) {
		return fmt.Errorf("generate: %s would be named %q, a reserved word of TypeScript; rename the declaration", owner, name)
	}
	if earlier, ok := i.taken[name]; ok {
		return fmt.Errorf("generate: %s and %s are both named %q in the TypeScript client; rename one of the declarations", earlier, owner, name)
	}
	i.taken[name] = owner
	return nil
}
