package query

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// Canonical is the document's canonical JSON: keys sorted, no insignificant whitespace,
// numbers in their shortest form, times in UTC, uuids in lower case, relative times as
// seconds ("-7d" and "-1w" both become "-604800s"). Two documents that Parse into the same
// values have the same canonical form, so its Hash deduplicates stored filters (D13). A
// nil document is the empty document `{}`.
func Canonical(document *Document) ([]byte, error) {
	canonical := map[string]any{}
	if document != nil && document.where != nil {
		canonical["where"] = canonicalNode(document.where)
	}
	if document != nil && document.search != "" {
		canonical["search"] = document.search
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(canonical); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// Hash is the SHA-256 of the canonical form in lower-case hex.
func Hash(document *Document) (string, error) {
	canonical, err := Canonical(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalNode(current *node) map[string]any {
	switch {
	case current.and != nil:
		return map[string]any{"and": canonicalNodes(current.and)}
	case current.or != nil:
		return map[string]any{"or": canonicalNodes(current.or)}
	case current.not != nil:
		return map[string]any{"not": canonicalNode(current.not)}
	default:
		return map[string]any{"field": current.field, "op": string(current.operator), "value": canonicalValue(current.value)}
	}
}

func canonicalNodes(nodes []*node) []any {
	out := make([]any, len(nodes))
	for index, child := range nodes {
		out[index] = canonicalNode(child)
	}
	return out
}

func canonicalValue(value any) any {
	switch typed := value.(type) {
	case relative:
		return map[string]any{"relative": strconv.FormatInt(int64(typed.duration/time.Second), 10) + "s"}
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = canonicalValue(item)
		}
		return out
	default:
		return typed
	}
}
