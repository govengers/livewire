package query

import (
	"fmt"
	"strings"
)

// SortTerm is one term of a sort: `-createdAt` is {Field: "createdAt", Descending: true}.
type SortTerm struct {
	Field      string
	Descending bool
}

func (t SortTerm) String() string {
	if t.Descending {
		return "-" + t.Field
	}
	return t.Field
}

// ParseSort reads a comma-separated list of fields, each optionally prefixed with "-" for
// descending: "-createdAt,name". An empty raw value returns no terms, so the caller
// applies the resource's default sort. Only fields declared Sort may appear, each at most
// once; the id tiebreaker is added when the SQL is built, not here. Every refusal is an
// *Error with CodeInvalidSort.
func ParseSort(fields Fields, raw string) ([]SortTerm, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var terms []SortTerm
	seen := map[string]bool{}
	for index, part := range strings.Split(raw, ",") {
		path := fmt.Sprintf("sort[%d]", index)
		name, descending := strings.CutPrefix(strings.TrimSpace(part), "-")
		field, ok := fields.Lookup(name)
		if !ok || !field.Sort {
			return nil, newError(CodeInvalidSort, path, "unknown or unsortable field %q", name)
		}
		if seen[name] {
			return nil, newError(CodeInvalidSort, path, "field %q appears twice", name)
		}
		seen[name] = true
		terms = append(terms, SortTerm{Field: name, Descending: descending})
	}
	return terms, nil
}

func formatSort(terms []SortTerm) string {
	parts := make([]string, len(terms))
	for index, term := range terms {
		parts[index] = term.String()
	}
	return strings.Join(parts, ",")
}
