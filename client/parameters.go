package client

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Parameters are the parameters of one request in their wire form, as both transports read
// them: one text per name, a list as one comma-separated text, a filter document as JSON.
// A route's `{name}` placeholders take their values from it, the rest is the query string
// (REST) or the frame's params (socket).
type Parameters map[string]string

// Set puts value under name unless it is the zero value of its type, which a server reads
// as an absent parameter anyway: a nil pointer, "", 0, false, the nil uuid, the zero time or
// an empty list. It takes string, bool, int, int64, uuid.UUID and time.Time (RFC 3339 with
// nanoseconds), each also as a pointer, and []string; another type panics, a programming
// error of the caller (the generated code passes these types only).
func (p Parameters) Set(name string, value any) {
	if text, present := wireText(value); present {
		p[name] = text
	}
}

// SetPath puts a path parameter under name, its zero value included; the route refuses an
// empty one.
func (p Parameters) SetPath(name string, value any) {
	text, _ := wireText(value)
	p[name] = text
}

// SetJSON puts document under name as JSON (an inline filter document).
func (p Parameters) SetJSON(name string, document any) error {
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("livewire: the parameter %s: %w", name, err)
	}
	p[name] = string(encoded)
	return nil
}

// wireText is the wire form of a value and whether it is present (not the zero value).
func wireText(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, typed != ""
	case bool:
		return strconv.FormatBool(typed), typed
	case int:
		return strconv.Itoa(typed), typed != 0
	case int64:
		return strconv.FormatInt(typed, 10), typed != 0
	case uuid.UUID:
		return typed.String(), typed != uuid.Nil
	case time.Time:
		return typed.Format(time.RFC3339Nano), !typed.IsZero()
	case []string:
		return strings.Join(typed, ","), len(typed) > 0
	case *string, *bool, *int, *int64, *uuid.UUID, *time.Time:
		return pointedText(typed)
	default:
		panic(fmt.Sprintf("livewire: a parameter of the type %T has no wire form", value))
	}
}

// pointedText is the wire form of the value a pointer points to, present unless nil.
func pointedText(pointer any) (string, bool) {
	var text string
	switch typed := pointer.(type) {
	case *string:
		if typed == nil {
			return "", false
		}
		text = *typed
	case *bool:
		if typed == nil {
			return "", false
		}
		text = strconv.FormatBool(*typed)
	case *int:
		if typed == nil {
			return "", false
		}
		text = strconv.Itoa(*typed)
	case *int64:
		if typed == nil {
			return "", false
		}
		text = strconv.FormatInt(*typed, 10)
	case *uuid.UUID:
		if typed == nil {
			return "", false
		}
		text = typed.String()
	case *time.Time:
		if typed == nil {
			return "", false
		}
		text = typed.Format(time.RFC3339Nano)
	}
	return text, true
}

// route fills the `{name}` placeholders of path from parameters (escaped) and appends the
// others as the query string, sorted by name. A placeholder without a value is an error.
func route(path string, parameters Parameters) (string, error) {
	rest := maps.Clone(parameters)
	var filled strings.Builder
	for segment := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		filled.WriteString("/")
		name, placeholder := strings.CutPrefix(segment, "{")
		name, closed := strings.CutSuffix(name, "}")
		if !placeholder || !closed {
			filled.WriteString(segment)
			continue
		}
		value := rest[name]
		if value == "" {
			return "", fmt.Errorf("livewire: %s: the path parameter %s is missing", path, name)
		}
		filled.WriteString(url.PathEscape(value))
		delete(rest, name)
	}
	if len(rest) == 0 {
		return filled.String(), nil
	}
	query := url.Values{}
	for name, value := range rest {
		query.Set(name, value)
	}
	return filled.String() + "?" + query.Encode(), nil
}
