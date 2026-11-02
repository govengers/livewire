package livewire_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"

	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// The documents (README, llms.txt, the wiki) quote their code from compiled and tested Go and
// TypeScript files: every code block follows a marker naming its source and region,
//
//	<!-- snippet: examples/todo/internal/controller/registry.go#resource -->
//
// and the source holds the region between `snippet:begin resource` and `snippet:end resource`
// comments. The tests read the documents as CommonMark does (goldmark), so a block nested in a
// list or a blockquote is a block too. Only a fenced block tagged `sh` or `text` (a command, an
// output) needs no marker; an indented code block, an unclosed fence and any HTML other than a
// marker are refused. A quote that drifts from its source, a code block without a source,
// a link or image that resolves to nothing and an em-dash fail these tests.

// module is the module's directory, which the documents and their sources are paths in.
var module fs.FS = os.DirFS(".")

// documents are the files the tests check, relative to the module root.
func documents(t *testing.T) []string {
	t.Helper()
	wiki, err := fs.Glob(module, "docs/wiki/*.md")
	require.NoError(t, err)
	require.NotEmpty(t, wiki)
	return append([]string{"README.md", "llms.txt"}, wiki...)
}

func TestDocumentSnippetsMatchTheirSources(t *testing.T) {
	for _, document := range documents(t) {
		content, err := fs.ReadFile(module, document)
		require.NoError(t, err)
		require.Empty(t, snippetFaults(module, document, content))
	}
}

func TestDocumentLinksResolve(t *testing.T) {
	for _, document := range documents(t) {
		content, err := fs.ReadFile(module, document)
		require.NoError(t, err)
		require.Empty(t, linkFaults(module, document, content))
	}
}

// TestDocumentGuardsHaveNoBypass pins every way a review found to slip a block or a link past
// the two tests above, and the prose they must not refuse.
func TestDocumentGuardsHaveNoBypass(t *testing.T) {
	files := fstest.MapFS{
		"docs/wiki/Page.md": {Data: []byte("# Page\n\n## A section\n")},
		"source.go":         {Data: []byte("package source\n\n// snippet:begin one\nvar one = 1\n\n// snippet:end one\n")},
		"notes.md":          {Data: []byte("// snippet:begin one\nvar one = 1\n// snippet:end one\n")},
	}
	quote := "<!-- snippet: source.go#one -->\n"
	for _, testCase := range []struct {
		name, content string
		faults        bool
	}{
		{"a quote", quote + "```go\nvar one = 1\n```\n", false},
		{"a quote whose fence has a trailing space", quote + "```go \nvar one = 1\n```\n", false},
		{"a quote in a blockquote", "> " + quote + "> ```go\n> var one = 1\n> ```\n", false},
		{"a drifted quote", quote + "```go\nvar one = 2\n```\n", true},
		{"a source that is not code", "<!-- snippet: notes.md#one -->\n```go\nvar one = 1\n```\n", true},
		{"output blocks", "```sh\ngo get github.com/govengers/livewire\n```\n\n```text\nok\n```\n", false},
		{"an untagged block", "```\nvar one = 1\n```\n", true},
		{"a golang block", "```golang\nvar one = 1\n```\n", true},
		{"a tsx block", "```tsx\nconst one = 1\n```\n", true},
		{"a tilde fence", "~~~go\nvar one = 1\n~~~\n", true},
		{"a fence with a trailing space", "```go \nvar one = 1\n```\n", true},
		{"an indented fence", "   ```go\nvar one = 1\n   ```\n", true},
		{"an output tag with more", "```sh title\nls\n```\n", true},
		{"an unclosed output block", "```sh\nls\n", true},
		{"a fence in a blockquote", "> ```go\n> var one = 1\n> ```\n", true},
		{"a fence in a list item", "- One:\n\n    ```go\n    var one = 1\n    ```\n", true},
		{"a fence right under a list item", "- One:\n  ```go\n  var one = 1\n  ```\n", true},
		{"an indented code block", "Text.\n\n    var one = 1\n", true},
		{"an indented code block under a heading", "## Heading\n    var one = 1\n", true},
		{"a pre block", "<pre><code>var one = 1</code></pre>\n", true},
		{"an HTML link", "Text <a href=\"Missing.md\">a</a>.\n", true},
		{"a list item's second paragraph", "- One.\n\n    Its second paragraph.\n- Two.\n    - Nested.\n", false},
		{"an em-dash", "One \u2014 two.\n", true},
	} {
		require.Equal(t, testCase.faults, len(snippetFaults(files, "docs/wiki/Pin.md", []byte(testCase.content))) > 0, testCase.name)
	}
	for _, testCase := range []struct {
		name, content string
		faults        bool
	}{
		{"links that resolve", "[a](Page.md) [b](Page.md#a-section \"title\") [c][r] [d](<Page.md>)\n\n[r]: Page.md#page\n", false},
		{"a missing page", "[a](Missing.md)\n", true},
		{"a missing anchor", "[a](Page.md#nowhere)\n", true},
		{"a link with a title", "[a](Missing.md \"title\")\n", true},
		{"an angle-bracket destination", "[a](<Missing page.md>)\n", true},
		{"a reference link", "[a][r]\n\n[r]: Missing.md\n", true},
		{"a reference link's anchor", "[a][r]\n\n[r]: Page.md#nowhere \"title\"\n", true},
		{"an image", "![a](Missing.png)\n", true},
		{"a link in a block", "```text\n[a](Missing.md)\n```\n", false},
		{"a link in inline code", "Write `[a](Missing.md)`.\n", false},
	} {
		require.Equal(t, testCase.faults, len(linkFaults(files, "docs/wiki/Pin.md", []byte(testCase.content))) > 0, testCase.name)
	}
}

var (
	snippetMarker = regexp.MustCompile(`^<!-- snippet: (\S+)#([a-z0-9-]+) -->$`)
	closingFence  = regexp.MustCompile("^[\\s>]*(```|~~~)")
	// outputLanguages are the tags of blocks that show a command or its output: they need no
	// source. Every other block is code and quotes one.
	outputLanguages = map[string]bool{"sh": true, "text": true}
	// sourceExtensions are the files a quote may come from: compiled and tested ones.
	sourceExtensions = []string{".go", ".ts"}
)

// parse reads a document as CommonMark.
func parse(content []byte) ast.Node {
	return goldmark.New().Parser().Parse(text.NewReader(content))
}

// lines is the text of a block's lines.
func lines(node ast.Node, content []byte) string {
	var joined bytes.Buffer
	for index := range node.Lines().Len() {
		segment := node.Lines().At(index)
		joined.Write(segment.Value(content))
	}
	return joined.String()
}

// lineOf is the line number of a byte offset.
func lineOf(content []byte, offset int) int {
	return bytes.Count(content[:offset], []byte("\n")) + 1
}

// snippetFaults lists what is wrong with the code blocks and the HTML of one document.
func snippetFaults(files fs.FS, document string, content []byte) []string {
	var faults []string
	if bytes.Contains(content, []byte("\u2014")) {
		faults = append(faults, document+": an em-dash")
	}
	fault := func(node ast.Node, format string, arguments ...any) {
		line := 0
		if node.Lines().Len() > 0 {
			line = lineOf(content, node.Lines().At(0).Start)
		}
		faults = append(faults, fmt.Sprintf("%s:%d: ", document, line)+fmt.Sprintf(format, arguments...))
	}
	_ = ast.Walk(parse(content), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.CodeBlock:
			fault(node, "an indented code block; fence it and quote it from a source")
		case *ast.HTMLBlock:
			if !snippetMarker.MatchString(strings.TrimSpace(lines(node, content))) {
				fault(node, "HTML other than a snippet marker")
			}
		case *ast.RawHTML:
			faults = append(faults, document+": inline HTML")
		case *ast.FencedCodeBlock:
			faults = append(faults, fencedFaults(files, document, content, typed)...)
		default:
		}
		return ast.WalkContinue, nil
	})
	return faults
}

// fencedFaults checks one fenced block: closed, quoted from its marked source, or an output.
func fencedFaults(files fs.FS, document string, content []byte, block *ast.FencedCodeBlock) []string {
	info := ""
	if block.Info != nil {
		info = strings.TrimSpace(string(block.Info.Segment.Value(content)))
	}
	if block.Lines().Len() == 0 {
		return []string{fmt.Sprintf("%s: an empty %q block", document, info)}
	}
	first, last := block.Lines().At(0), block.Lines().At(block.Lines().Len()-1)
	location := fmt.Sprintf("%s:%d", document, lineOf(content, first.Start)-1)
	if !closingFence.Match(content[last.Stop:]) {
		return []string{location + ": a code block that does not end"}
	}
	var marker []string
	if previous, isHTML := block.PreviousSibling().(*ast.HTMLBlock); isHTML {
		marker = snippetMarker.FindStringSubmatch(strings.TrimSpace(lines(previous, content)))
	}
	switch {
	case marker != nil:
		source, err := region(files, marker[1], marker[2])
		switch {
		case err != nil:
			return []string{location + ": " + err.Error()}
		case source != strings.TrimSuffix(lines(block, content), "\n"):
			return []string{fmt.Sprintf("%s: the quote of %s#%s drifted from its source", location, marker[1], marker[2])}
		}
	case !outputLanguages[info]:
		return []string{fmt.Sprintf("%s: a %q block without a snippet marker; quote it from a compiled or tested source", location, info)}
	}
	return nil
}

// region is the text between the snippet markers of name in the source at path, dedented by
// its common indentation, a leading tab shown as four spaces.
func region(files fs.FS, path, name string) (string, error) {
	if !slices.Contains(sourceExtensions, filepath.Ext(path)) {
		return "", fmt.Errorf("%s is no compiled or tested source (%v)", path, sourceExtensions)
	}
	content, err := fs.ReadFile(files, path)
	if err != nil {
		return "", err
	}
	var quoted []string
	inside, found := false, 0
	for line := range strings.SplitSeq(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasSuffix(trimmed, "snippet:begin "+name):
			inside = true
			found++
		case strings.HasSuffix(trimmed, "snippet:end "+name):
			inside = false
		case inside:
			quoted = append(quoted, line)
		}
	}
	if found != 1 || inside {
		return "", fmt.Errorf("%s: %d regions %q, the last one ended: %t", path, found, name, !inside)
	}
	indentation, measured := "", false
	for _, line := range quoted {
		if strings.TrimSpace(line) == "" {
			continue
		}
		leading := line[:len(line)-len(strings.TrimLeftFunc(line, unicode.IsSpace))]
		if !measured || len(leading) < len(indentation) {
			indentation, measured = leading, true
		}
	}
	for index, line := range quoted {
		line = strings.TrimPrefix(line, indentation)
		tabs := len(line) - len(strings.TrimLeft(line, "\t"))
		quoted[index] = strings.Repeat("    ", tabs) + line[tabs:]
	}
	return strings.Trim(strings.Join(quoted, "\n"), "\n"), nil // the blank lines that keep a marker off a doc comment
}

// linkFaults lists the links and images of one document, inline or by reference, that resolve
// to nothing.
func linkFaults(files fs.FS, document string, content []byte) []string {
	var targets []string
	_ = ast.Walk(parse(content), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.Link:
			targets = append(targets, string(typed.Destination))
		case *ast.Image:
			targets = append(targets, string(typed.Destination))
		default:
		}
		return ast.WalkContinue, nil
	})
	var faults []string
	for _, target := range targets {
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		path, anchor, _ := strings.Cut(target, "#")
		resolved := document
		if path != "" {
			resolved = filepath.ToSlash(filepath.Join(filepath.Dir(document), path))
			if _, err := fs.Stat(files, resolved); err != nil {
				faults = append(faults, fmt.Sprintf("%s: the link %s: %v", document, target, err))
				continue
			}
		}
		if anchor == "" {
			continue
		}
		found, err := anchors(files, resolved)
		if err != nil || !slices.Contains(found, anchor) {
			faults = append(faults, fmt.Sprintf("%s: the anchor of %s", document, target))
		}
	}
	return faults
}

// anchors are the heading anchors GitHub gives a Markdown file: the heading's text in lower
// case, punctuation but hyphens and underscores dropped, spaces as hyphens, a repeated one
// numbered.
func anchors(files fs.FS, path string) ([]string, error) {
	content, err := fs.ReadFile(files, path)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var found []string
	_ = ast.Walk(parse(content), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if _, isHeading := node.(*ast.Heading); !isHeading || !entering {
			return ast.WalkContinue, nil
		}
		var slug strings.Builder
		for _, character := range strings.ToLower(plainText(node, content)) {
			switch {
			case unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_':
				slug.WriteRune(character)
			case character == ' ':
				slug.WriteRune('-')
			default:
			}
		}
		anchor := slug.String()
		if count := seen[anchor]; count > 0 {
			seen[anchor]++
			anchor += "-" + strconv.Itoa(count)
		} else {
			seen[anchor] = 1
		}
		found = append(found, anchor)
		return ast.WalkSkipChildren, nil
	})
	return found, nil
}

// plainText is the text of an inline node and its children, code spans included.
func plainText(node ast.Node, content []byte) string {
	var joined strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if leaf, isText := child.(*ast.Text); isText {
			joined.Write(leaf.Segment.Value(content))
			continue
		}
		joined.WriteString(plainText(child, content))
	}
	return joined.String()
}
