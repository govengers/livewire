package generate

import (
	"fmt"
	"go/format"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// goFile collects the imports of one generated Go file and writes the type expressions of
// the application's types: every named type is imported from its package, under an alias
// no other import and no identifier of the file takes. A type the file cannot import is
// refused (D67): an unexported one, one of a main or test package, one behind an internal
// directory whose parent does not contain importPath, one of the generated package itself,
// and an unnamed struct, function or channel type.
type goFile struct {
	importPath string
	aliases    map[string]string // import path to alias
	explicit   map[string]bool   // the alias must be written: it may differ from the package's name
	taken      map[string]bool
}

// goLocalNames are the identifiers the generated function bodies use; no import takes them.
var goLocalNames = []string{"c", "ctx", "parameters", "wire", "data", "err", "body", "options", "file", "writer", "response", "encoded", "id", "values", "value"}

func newGoFile(importPath string, identifiers []string) *goFile {
	file := &goFile{importPath: importPath, aliases: map[string]string{}, explicit: map[string]bool{}, taken: map[string]bool{}}
	for _, name := range slices.Concat(goLocalNames, identifiers) {
		file.taken[name] = true
	}
	return file
}

// use imports path and answers its alias; name is the package's name when known ("" when
// only the path is, as for a type argument), the alias is then the path's last element.
func (f *goFile) use(path, name string) string {
	if alias, ok := f.aliases[path]; ok {
		if name != "" && name == alias {
			f.explicit[path] = false // the package's name, learned after a type argument named the path
		}
		return alias
	}
	alias := name
	if alias == "" {
		alias = guessPackageName(path)
	}
	base := alias
	for number := 2; f.taken[alias] || token.Lookup(alias).IsKeyword(); number++ {
		alias = base + strconv.Itoa(number)
	}
	f.aliases[path], f.taken[alias] = alias, true
	f.explicit[path] = name == "" || alias != name
	return alias
}

// guessPackageName is the identifier of a path's last element without a version suffix:
// "gopkg.in/yaml.v3" is yaml. The import names it explicitly, so a wrong guess is harmless.
func guessPackageName(path string) string {
	last := path[strings.LastIndex(path, "/")+1:]
	last, _, _ = strings.Cut(last, ".")
	var builder strings.Builder
	for _, character := range last {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(unicode.ToLower(character))
		}
	}
	if builder.Len() == 0 || !unicode.IsLetter(rune(builder.String()[0])) {
		return "dto" + builder.String()
	}
	return builder.String()
}

// importBlock writes the imports, the standard library first.
func (f *goFile) importBlock() string {
	var standard, others []string
	for path := range f.aliases {
		if strings.Contains(strings.Split(path, "/")[0], ".") {
			others = append(others, path)
		} else {
			standard = append(standard, path)
		}
	}
	slices.Sort(standard)
	slices.Sort(others)
	if len(standard)+len(others) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("import (\n")
	for _, path := range standard {
		builder.WriteString("\t" + f.importLine(path) + "\n")
	}
	if len(standard) > 0 && len(others) > 0 {
		builder.WriteString("\n")
	}
	for _, path := range others {
		builder.WriteString("\t" + f.importLine(path) + "\n")
	}
	builder.WriteString(")\n")
	return builder.String()
}

func (f *goFile) importLine(path string) string {
	if f.explicit[path] {
		return f.aliases[path] + " " + strconv.Quote(path)
	}
	return strconv.Quote(path)
}

// expression is the Go type expression of goType.
func (f *goFile) expression(goType reflect.Type) (string, error) {
	if goType.Name() != "" {
		return f.named(goType)
	}
	switch goType.Kind() {
	case reflect.Pointer:
		element, err := f.expression(goType.Elem())
		return "*" + element, err
	case reflect.Slice:
		element, err := f.expression(goType.Elem())
		return "[]" + element, err
	case reflect.Array:
		element, err := f.expression(goType.Elem())
		return "[" + strconv.Itoa(goType.Len()) + "]" + element, err
	case reflect.Map:
		key, err := f.expression(goType.Key())
		if err != nil {
			return "", err
		}
		value, err := f.expression(goType.Elem())
		return "map[" + key + "]" + value, err
	case reflect.Interface:
		if goType.NumMethod() == 0 {
			return "any", nil
		}
	default:
	}
	return "", fmt.Errorf("generate: the Go client cannot name the type %s: give it a name in an importable package", goType)
}

// named is the expression of a named type: a predeclared one as it is, another qualified
// with the alias of its package, a generic instance with its type arguments, which reflect
// knows only by their text.
func (f *goFile) named(goType reflect.Type) (string, error) {
	if goType.PkgPath() == "" {
		return goType.Name(), nil
	}
	if err := f.importable(goType.PkgPath(), goType.Name(), goType.String()); err != nil {
		return "", err
	}
	packageName, _, _ := strings.Cut(goType.String(), ".")
	alias := f.use(goType.PkgPath(), packageName)
	outer, arguments, generic := strings.Cut(goType.Name(), "[")
	if !generic {
		return alias + "." + outer, nil
	}
	rendered, err := f.typeArguments(strings.TrimSuffix(arguments, "]"))
	if err != nil {
		return "", fmt.Errorf("%w (in %s)", err, goType)
	}
	return alias + "." + outer + "[" + rendered + "]", nil
}

// importable refuses a type the generated package cannot import.
func (f *goFile) importable(path, name, display string) error {
	switch {
	case !token.IsExported(name):
		return fmt.Errorf("generate: the Go client cannot import the type %s: it is unexported", display)
	case path == "main" || strings.HasSuffix(path, "_test"):
		return fmt.Errorf("generate: the Go client cannot import the type %s: a main or test package cannot be imported; move it to a package of its own", display)
	case path == f.importPath:
		return fmt.Errorf("generate: the type %s is declared in the generated package %s", display, f.importPath)
	}
	segments := strings.Split(path, "/")
	for index := len(segments) - 1; index >= 0; index-- {
		if segments[index] != "internal" {
			continue
		}
		parent := strings.Join(segments[:index], "/")
		if f.importPath != parent && !strings.HasPrefix(f.importPath, parent+"/") {
			return fmt.Errorf("generate: the Go client %s cannot import the type %s: it is internal to %s", f.importPath, display, parent)
		}
		return nil
	}
	return nil
}

// typeArguments renders the comma-separated type arguments of a generic name.
func (f *goFile) typeArguments(text string) (string, error) {
	var rendered []string
	for _, argument := range splitTopLevel(text) {
		expression, err := f.argument(strings.TrimSpace(argument))
		if err != nil {
			return "", err
		}
		rendered = append(rendered, expression)
	}
	return strings.Join(rendered, ", "), nil
}

// argument renders one type argument as reflect spells it: a predeclared name, a path
// qualified name (generic or not), a pointer, a slice, an array or a map of those.
func (f *goFile) argument(text string) (string, error) {
	switch {
	case strings.HasPrefix(text, "*"):
		element, err := f.argument(text[1:])
		return "*" + element, err
	case strings.HasPrefix(text, "[]"):
		element, err := f.argument(text[2:])
		return "[]" + element, err
	case strings.HasPrefix(text, "map["):
		key, value, ok := cutBracket(text[len("map"):])
		if !ok {
			break
		}
		keyExpression, err := f.argument(key)
		if err != nil {
			return "", err
		}
		valueExpression, err := f.argument(value)
		return "map[" + keyExpression + "]" + valueExpression, err
	case strings.HasPrefix(text, "["):
		length, element, ok := cutBracket(text)
		if _, err := strconv.Atoi(length); ok && err == nil {
			elementExpression, err := f.argument(element)
			return "[" + length + "]" + elementExpression, err
		}
	case text == "interface {}":
		return "any", nil
	default:
		return f.qualifiedArgument(text)
	}
	return "", fmt.Errorf("generate: the Go client cannot name the type argument %s", text)
}

// qualifiedArgument renders "path.Name", "path.Name[arguments]" or a predeclared name.
func (f *goFile) qualifiedArgument(text string) (string, error) {
	head, arguments, generic := strings.Cut(text, "[")
	dot := strings.LastIndex(head, ".")
	if dot < 0 {
		if !token.IsIdentifier(head) || generic {
			return "", fmt.Errorf("generate: the Go client cannot name the type argument %s", text)
		}
		return head, nil // a predeclared type
	}
	path, name := head[:dot], head[dot+1:]
	if err := f.importable(path, name, text); err != nil {
		return "", err
	}
	expression := f.use(path, "") + "." + name
	if !generic {
		return expression, nil
	}
	rendered, err := f.typeArguments(strings.TrimSuffix(arguments, "]"))
	return expression + "[" + rendered + "]", err
}

// cutBracket splits "[inner]rest" at the bracket that closes the first.
func cutBracket(text string) (inner, rest string, ok bool) {
	depth := 0
	for index, character := range text {
		switch character {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return text[1:index], text[index+1:], true
			}
		}
	}
	return "", "", false
}

// splitTopLevel splits text at the commas outside brackets.
func splitTopLevel(text string) []string {
	var parts []string
	depth, start := 0, 0
	for index, character := range text {
		switch character {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, text[start:index])
				start = index + 1
			}
		}
	}
	return append(parts, text[start:])
}

// formatGo formats a generated file; a file that does not parse is a bug of the generator.
func formatGo(name string, source string) ([]byte, error) {
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return nil, fmt.Errorf("generate: %s does not format: %w", name, err)
	}
	return formatted, nil
}

// goIdentifier is a name as a Go identifier: PascalCase with the initialisms of Go style
// ("filterId" is FilterID).
func goIdentifier(name string) string {
	initialisms := map[string]string{"Id": "ID", "Ids": "IDs", "Url": "URL", "Uuid": "UUID", "Api": "API", "Http": "HTTP", "Json": "JSON", "Ip": "IP", "Csv": "CSV", "Html": "HTML"}
	var builder strings.Builder
	for _, word := range camelWords(pascalCase(name)) {
		if initialism, ok := initialisms[word]; ok {
			word = initialism
		}
		builder.WriteString(word)
	}
	return builder.String()
}

// camelWords splits a PascalCase identifier before every upper-case letter that follows a
// lower-case letter or a digit.
func camelWords(identifier string) []string {
	var words []string
	runes := []rune(identifier)
	start := 0
	for index := 1; index < len(runes); index++ {
		if unicode.IsUpper(runes[index]) && !unicode.IsUpper(runes[index-1]) {
			words = append(words, string(runes[start:index]))
			start = index
		}
	}
	if start < len(runes) {
		words = append(words, string(runes[start:]))
	}
	return words
}

// kebabCase is a name as a command or flag name: "createPart" is create-part, "x_y" x-y.
func kebabCase(name string) string {
	var words []string
	for _, part := range identifierCharacter.Split(name, -1) {
		for _, word := range camelWords(pascalCase(part)) {
			words = append(words, strings.ToLower(word))
		}
	}
	return strings.Join(words, "-")
}
