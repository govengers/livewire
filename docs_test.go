package livewire_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

// The documents (README, llms.txt, the wiki) quote their code from compiled and tested files:
// a fenced block of Go, TypeScript or SQL follows a marker naming its source and region,
//
//	<!-- snippet: examples/todo/internal/controller/registry.go#resource -->
//
// and the source holds the region between `snippet:begin resource` and `snippet:end resource`
// comments. A quote that drifts from its source, a code block without a source, a link that
// resolves to nothing and an em-dash fail these tests.

// module is the module's directory, which the documents and their sources are paths in.
var module = os.DirFS(".")

// documents are the files the tests check, relative to the module root.
func documents(t *testing.T) []string {
	t.Helper()
	wiki, err := fs.Glob(module, "docs/wiki/*.md")
	require.NoError(t, err)
	require.NotEmpty(t, wiki)
	return append([]string{"README.md", "llms.txt"}, wiki...)
}

var (
	snippetMarker = regexp.MustCompile(`^<!-- snippet: ([^#\s]+)#([a-z0-9-]+) -->$`)
	fence         = regexp.MustCompile("^```([a-z]*)$")
	// quotedLanguages are the languages whose blocks must be quoted from a source.
	quotedLanguages = map[string]bool{"go": true, "ts": true, "typescript": true, "sql": true}
)

func TestDocumentSnippetsMatchTheirSources(t *testing.T) {
	for _, document := range documents(t) {
		content, err := fs.ReadFile(module, document)
		require.NoError(t, err)
		require.NotContains(t, string(content), "\u2014", "%s: no em-dash", document)
		lines := strings.Split(string(content), "\n")
		for index := 0; index < len(lines); index++ {
			opening := fence.FindStringSubmatch(lines[index])
			if opening == nil {
				continue
			}
			end := index + 1
			for end < len(lines) && lines[end] != "```" {
				end++
			}
			require.Less(t, end, len(lines), "%s:%d: a code block that does not end", document, index+1)
			marker := []string(nil)
			if index > 0 {
				marker = snippetMarker.FindStringSubmatch(lines[index-1])
			}
			switch {
			case marker != nil:
				quoted := strings.Join(lines[index+1:end], "\n")
				require.Equal(t, region(t, marker[1], marker[2]), quoted, "%s:%d: the quote of %s#%s drifted from its source", document, index+1, marker[1], marker[2])
			case quotedLanguages[opening[1]]:
				t.Errorf("%s:%d: a %s block without a snippet marker; quote it from a compiled or tested source", document, index+1, opening[1])
			}
			index = end
		}
	}
}

// region is the text between the snippet markers of name in the source at path, dedented by
// its common indentation, a leading tab shown as four spaces.
func region(t *testing.T, path, name string) string {
	t.Helper()
	content, err := fs.ReadFile(module, path)
	require.NoError(t, err, "the source of a snippet")
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
	require.Equal(t, 1, found, "%s: exactly one region %q", path, name)
	require.False(t, inside, "%s: the region %q does not end", path, name)
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
	return strings.Trim(strings.Join(quoted, "\n"), "\n") // the blank lines that keep a marker off a doc comment
}

var (
	markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	heading      = regexp.MustCompile(`^#{1,6} (.+)$`)
)

func TestDocumentLinksResolve(t *testing.T) {
	for _, document := range documents(t) {
		content, err := fs.ReadFile(module, document)
		require.NoError(t, err)
		for _, match := range markdownLink.FindAllStringSubmatch(withoutCode(string(content)), -1) {
			target := match[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(target, "#")
			resolved := document
			if path != "" {
				resolved = filepath.ToSlash(filepath.Join(filepath.Dir(document), path))
				_, err := fs.Stat(module, resolved)
				require.NoError(t, err, "%s: the link %s", document, target)
			}
			if anchor != "" {
				require.Contains(t, anchors(t, resolved), anchor, "%s: the anchor of %s", document, target)
			}
		}
	}
}

// withoutCode is a document without its fenced blocks, whose brackets are code, not links.
func withoutCode(content string) string {
	var kept []string
	inside := false
	for line := range strings.SplitSeq(content, "\n") {
		if strings.HasPrefix(line, "```") {
			inside = !inside
			continue
		}
		if !inside {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// anchors are the heading anchors GitHub gives a Markdown file: lower case, punctuation but
// hyphens and underscores dropped, spaces as hyphens, a repeated one numbered.
func anchors(t *testing.T, path string) []string {
	t.Helper()
	content, err := fs.ReadFile(module, path)
	require.NoError(t, err)
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
	return found
}
