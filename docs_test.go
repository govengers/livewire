package livewire_test

import (
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
)

// The documents (README, llms.txt, the wiki) quote their code from compiled and tested Go and
// TypeScript files: every code block follows a marker naming its source and region,
//
//	<!-- snippet: examples/todo/internal/controller/registry.go#resource -->
//
// and the source holds the region between `snippet:begin resource` and `snippet:end resource`
// comments. Only a block tagged `sh` or `text` (a command, an output) needs none, and an
// indented code block is refused. A quote that drifts from its source, a code block without a
// source, an inline or reference link that resolves to nothing and an em-dash fail these tests.

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
		require.Empty(t, snippetFaults(module, document, string(content)))
	}
}

func TestDocumentLinksResolve(t *testing.T) {
	for _, document := range documents(t) {
		content, err := fs.ReadFile(module, document)
		require.NoError(t, err)
		require.Empty(t, linkFaults(module, document, string(content)))
	}
}

// TestDocumentGuardsHaveNoBypass pins every way a review found to slip a block or a link past
// the two tests above.
func TestDocumentGuardsHaveNoBypass(t *testing.T) {
	files := fstest.MapFS{
		"docs/wiki/Page.md": {Data: []byte("# Page\n\n## A section\n")},
		"source.go":         {Data: []byte("package source\n\n// snippet:begin one\nvar one = 1\n\n// snippet:end one\n")},
		"notes.md":          {Data: []byte("<!-- snippet:begin one -->\nvar one = 1\n<!-- snippet:end one -->\n")},
	}
	for _, testCase := range []struct {
		name, content string
		faults        bool
	}{
		{"a quote", "<!-- snippet: source.go#one -->\n```go\nvar one = 1\n```\n", false},
		{"a drifted quote", "<!-- snippet: source.go#one -->\n```go\nvar one = 2\n```\n", true},
		{"a source that is not code", "<!-- snippet: notes.md#one -->\n```go\nvar one = 1\n```\n", true},
		{"an output block", "```sh\ngo get github.com/govengers/livewire\n```\n\n```text\nok\n```\n", false},
		{"an untagged block", "```\nvar one = 1\n```\n", true},
		{"a golang block", "```golang\nvar one = 1\n```\n", true},
		{"a tsx block", "```tsx\nconst one = 1\n```\n", true},
		{"a tilde fence", "~~~go\nvar one = 1\n~~~\n", true},
		{"a fence with a trailing space", "```go \nvar one = 1\n```\n", true},
		{"an indented fence", "   ```go\nvar one = 1\n   ```\n", true},
		{"an output tag with more", "```sh title\nls\n```\n", true},
		{"an indented code block", "Text.\n\n    var one = 1\n", true},
		{"an em-dash", "One — two.\n", true},
	} {
		require.Equal(t, testCase.faults, len(snippetFaults(files, "docs/wiki/Pin.md", testCase.content)) > 0, testCase.name)
	}
	for _, testCase := range []struct {
		name, content string
		faults        bool
	}{
		{"links that resolve", "[a](Page.md) [b](Page.md#a-section \"title\") [c][r]\n\n[r]: Page.md#page\n", false},
		{"a missing page", "[a](Missing.md)\n", true},
		{"a missing anchor", "[a](Page.md#nowhere)\n", true},
		{"a link with a title", "[a](Missing.md \"title\")\n", true},
		{"a reference link", "[a][r]\n\n[r]: Missing.md\n", true},
		{"a reference link's anchor", "[a][r]\n\n[r]: Page.md#nowhere \"title\"\n", true},
		{"a link in code", "```text\n[a](Missing.md)\n```\n", false},
	} {
		require.Equal(t, testCase.faults, len(linkFaults(files, "docs/wiki/Pin.md", testCase.content)) > 0, testCase.name)
	}
}

var (
	snippetMarker = regexp.MustCompile(`^<!-- snippet: (\S+)#([a-z0-9-]+) -->$`)
	fenceOpening  = regexp.MustCompile("^( {0,3})(`{3,}|~{3,})(.*)$")
	indentedCode  = regexp.MustCompile(`^( {4}|\t)\s*\S`)
	// outputLanguages are the tags of blocks that show a command or its output: they need no
	// source. Every other block is code and quotes one.
	outputLanguages = map[string]bool{"sh": true, "text": true}
	// sourceExtensions are the files a quote may come from: compiled and tested ones.
	sourceExtensions = []string{".go", ".ts"}
)

// codeBlock is one fenced block: the lines of its fences and its info string.
type codeBlock struct {
	opening, closing int
	indentation      string
	info             string
}

// codeBlocks finds the fenced blocks of lines, with backticks or tildes, as CommonMark closes
// them: a fence of the same character, at least as long; an unclosed block runs to the end.
func codeBlocks(lines []string) []codeBlock {
	var blocks []codeBlock
	for index := 0; index < len(lines); index++ {
		opening := fenceOpening.FindStringSubmatch(lines[index])
		if opening == nil {
			continue
		}
		closing := regexp.MustCompile("^ {0,3}" + regexp.QuoteMeta(opening[2][:1]) + "{" + strconv.Itoa(len(opening[2])) + ",}\\s*$")
		end := index + 1
		for end < len(lines) && !closing.MatchString(lines[end]) {
			end++
		}
		blocks = append(blocks, codeBlock{opening: index, closing: end, indentation: opening[1], info: opening[3]})
		index = end
	}
	return blocks
}

// snippetFaults lists what is wrong with the code blocks of one document.
func snippetFaults(files fs.FS, document, content string) []string {
	var faults []string
	if strings.Contains(content, "—") {
		faults = append(faults, document+": an em-dash")
	}
	lines := strings.Split(content, "\n")
	blocks := codeBlocks(lines)
	inside := func(index int) bool {
		return slices.ContainsFunc(blocks, func(block codeBlock) bool { return index >= block.opening && index <= block.closing })
	}
	for index, line := range lines {
		if indentedCode.MatchString(line) && !inside(index) && (index == 0 || strings.TrimSpace(lines[index-1]) == "") {
			faults = append(faults, fmt.Sprintf("%s:%d: an indented code block; fence it and quote it from a source", document, index+1))
		}
	}
	for _, block := range blocks {
		location := fmt.Sprintf("%s:%d", document, block.opening+1)
		if block.closing == len(lines) {
			faults = append(faults, location+": a code block that does not end")
			continue
		}
		var marker []string
		if block.opening > 0 {
			marker = snippetMarker.FindStringSubmatch(lines[block.opening-1])
		}
		switch {
		case block.indentation != "":
			faults = append(faults, location+": an indented fence")
		case marker != nil:
			source, err := region(files, marker[1], marker[2])
			switch {
			case err != nil:
				faults = append(faults, location+": "+err.Error())
			case source != strings.Join(lines[block.opening+1:block.closing], "\n"):
				faults = append(faults, fmt.Sprintf("%s: the quote of %s#%s drifted from its source", location, marker[1], marker[2]))
			}
		case !outputLanguages[block.info]:
			faults = append(faults, fmt.Sprintf("%s: a %q block without a snippet marker; quote it from a compiled or tested source", location, block.info))
		}
	}
	return faults
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

var (
	// inlineLink is a link's destination, with an optional title: [text](target "title").
	inlineLink = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*\)`)
	// referenceDefinition is the destination of a reference link: [label]: target "title".
	referenceDefinition = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?`)
	heading             = regexp.MustCompile(`^#{1,6} (.+)$`)
)

// linkFaults lists the inline and reference links of one document that resolve to nothing.
func linkFaults(files fs.FS, document, content string) []string {
	text := withoutCode(content)
	var targets []string
	for _, match := range inlineLink.FindAllStringSubmatch(text, -1) {
		targets = append(targets, match[1])
	}
	for _, match := range referenceDefinition.FindAllStringSubmatch(text, -1) {
		targets = append(targets, match[1])
	}
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

// withoutCode is a document without its fenced blocks, whose brackets are code, not links.
func withoutCode(content string) string {
	lines := strings.Split(content, "\n")
	for _, block := range slices.Backward(codeBlocks(lines)) {
		lines = slices.Delete(lines, block.opening, min(block.closing+1, len(lines)))
	}
	return strings.Join(lines, "\n")
}

// anchors are the heading anchors GitHub gives a Markdown file: lower case, punctuation but
// hyphens and underscores dropped, spaces as hyphens, a repeated one numbered.
func anchors(files fs.FS, path string) ([]string, error) {
	content, err := fs.ReadFile(files, path)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	var found []string
	for line := range strings.SplitSeq(withoutCode(string(content)), "\n") {
		match := heading.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		var slug strings.Builder
		for _, character := range strings.ToLower(strings.ReplaceAll(match[1], "`", "")) {
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
	}
	return found, nil
}
