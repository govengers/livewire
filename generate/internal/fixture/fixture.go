// Package fixture holds the DTOs of the generator's test registry: every mapping case of
// encoding/json the generator follows. Only the tests import it.
package fixture

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kind is a named string type the test registry lists in Options.Enums.
type Kind string

// The kinds.
const (
	KindBolt Kind = "bolt"
	KindNut  Kind = "nut"
)

// Color is a named string type without listed values: an alias of string.
type Color string

// Cents is a named number.
type Cents int64

// Tags is a named slice: nullable where it is used.
type Tags []string

// Audit is embedded by value and flattened.
type Audit struct {
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// Owner is embedded by pointer and flattened.
type Owner struct {
	OwnerID uuid.UUID `json:"ownerId"`
}

// Dimensions is embedded under a JSON name, so it is one field.
type Dimensions struct {
	Width  float64 `json:"width"`
	Height float32 `json:"height"`
}

type hidden struct {
	Promoted string `json:"promoted"` // promoted through an unexported embedding
}

// Raw marshals itself: nothing is known about its JSON.
type Raw struct{}

// MarshalJSON writes an empty object.
func (Raw) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

// UnmarshalJSON reads what MarshalJSON wrote: a type that marshals itself must unmarshal
// itself, or the Go client refuses it (D67).
func (*Raw) UnmarshalJSON([]byte) error { return nil }

// TextOnly marshals itself as text but cannot read it back: the Go client refuses it.
type TextOnly struct{ value string }

// MarshalText writes the value.
func (t TextOnly) MarshalText() ([]byte, error) { return []byte(t.value), nil }

// The cases of the Go client's decodability check (D67), as encoding/json decodes them.
type (
	// JSONOnly marshals itself but reads nothing back: refused.
	JSONOnly struct{}
	// JSONWithText writes a JSON string and reads it back through UnmarshalText: decodes.
	JSONWithText struct{ value string }
	// TextString is a string kind with MarshalText: encoding/json reads the text as the string.
	TextString string
	// DecodesItself has UnmarshalJSON, so the field it holds is its own business: decodes.
	DecodesItself struct {
		Hook fmt.Stringer `json:"hook"`
	}
	// EmbedsDecoder decodes through the UnmarshalJSON it promotes.
	EmbedsDecoder struct {
		DecodesItself
	}
	// PointerCodec marshals and unmarshals through pointer receivers: decodes.
	PointerCodec struct{ value string }
	// TextReader is a struct with UnmarshalText only: written as an object, which
	// UnmarshalText cannot read; refused.
	TextReader struct {
		Name string `json:"name"`
	}
	// TextBytes is a byte slice with UnmarshalText only: encoding/json writes it as a base64
	// string and reads that string back through UnmarshalText, so it decodes.
	TextBytes []byte
	// HoldsTextBytes has a TextBytes field.
	HoldsTextBytes struct {
		Data TextBytes `json:"data"`
	}
	// TextKey is a map key that marshals itself as text only: refused as a key.
	TextKey struct{ value string }
	// HoldsTextKeys has a map of TextKey keys.
	HoldsTextKeys struct {
		Counts map[TextKey]int `json:"counts"`
	}
	// HoldsStringer has an interface field with methods, which encoding/json cannot fill.
	HoldsStringer struct {
		Hook fmt.Stringer `json:"hook"`
	}
)

// MarshalJSON writes a string the struct cannot be read from.
func (JSONOnly) MarshalJSON() ([]byte, error) { return []byte(`"opaque"`), nil }

// MarshalJSON writes the value as a JSON string.
func (j JSONWithText) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(j.value)), nil }

// UnmarshalText reads the string.
func (j *JSONWithText) UnmarshalText(text []byte) error {
	j.value = string(text)
	return nil
}

// MarshalText writes the string upper-cased.
func (t TextString) MarshalText() ([]byte, error) { return []byte(strings.ToUpper(string(t))), nil }

// UnmarshalJSON reads anything.
func (*DecodesItself) UnmarshalJSON([]byte) error { return nil }

// MarshalJSON writes the value.
func (p *PointerCodec) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(p.value)), nil }

// UnmarshalJSON reads the value.
func (p *PointerCodec) UnmarshalJSON(data []byte) error { return json.Unmarshal(data, &p.value) }

// UnmarshalText keeps the text it is given, the base64 encoding/json wrote.
func (b *TextBytes) UnmarshalText(text []byte) error {
	*b = append((*b)[:0], text...)
	return nil
}

// UnmarshalText reads a name.
func (t *TextReader) UnmarshalText(text []byte) error {
	t.Name = string(text)
	return nil
}

// MarshalText writes the key.
func (k TextKey) MarshalText() ([]byte, error) { return []byte(k.value), nil }

// HoldsTextOnly reaches TextOnly through a field.
type HoldsTextOnly struct {
	Inner []TextOnly `json:"inner"`
}

// Primary carries the JSON id of CreateEmbedded.
type Primary struct {
	ID uuid.UUID `json:"id"`
}

// Secondary has the Go field ID too, under another JSON name.
type Secondary struct {
	ID uuid.UUID `json:"otherId"`
}

type hiddenBase struct {
	ID uuid.UUID `json:"id"`
}

// CreateHidden promotes its JSON id from an unexported embedded struct: the client fills
// body.ID, which compiles from another package.
type CreateHidden struct {
	hiddenBase
	Name string `json:"name"`
}

// CreateThroughPointer promotes its id through an embedded pointer, which may be nil: the
// client refuses to fill it.
type CreateThroughPointer struct {
	*Primary
	Name string `json:"name"`
}

// CreateEmbedded is a create body whose JSON id lives in an embedded struct, beside another
// embedded field ID: the client fills body.Primary.ID.
type CreateEmbedded struct {
	Primary
	Secondary
	Name string `json:"name"`
}

// Code marshals itself as text.
type Code struct{ value string }

// MarshalText writes the code.
func (c Code) MarshalText() ([]byte, error) { return []byte(strings.ToUpper(c.value)), nil }

// UnmarshalText reads the code, so the Go client decodes what the server wrote.
func (c *Code) UnmarshalText(text []byte) error {
	c.value = strings.ToLower(string(text))
	return nil
}

// Part is the item of the test resource.
type Part struct {
	Audit
	*Owner
	hidden
	Dimensions  `json:"dimensions"`
	ID          uuid.UUID         `json:"id"`
	Name        string            `json:"name"`
	Kind        Kind              `json:"kind"`
	Color       Color             `json:"color"`
	Price       *float64          `json:"price"`
	Secret      string            `json:"secret"`
	Notes       string            `json:"notes,omitempty"`
	Previous    *Kind             `json:"previous,omitempty"`
	Tags        Tags              `json:"tags"`
	Labels      []string          `json:"labels,omitempty"`
	Sizes       []string          `json:"sizes" enum:"s,m,l"`
	Grade       string            `json:"grade" enum:"a,b"`
	Counts      map[string]int    `json:"counts"`
	Names       map[int]string    `json:"names,omitzero"`
	Children    []*Part           `json:"children"`
	Matrix      [3]int            `json:"matrix"`
	Checksum    []byte            `json:"checksum"`
	Total       Cents             `json:"total"`
	Large       int64             `json:"large,string"`
	Flag        *bool             `json:"flag,string"`
	Version     int               `json:"version"`
	Expires     time.Time         `json:"expires,omitzero"`
	Started     time.Time         `json:"started,omitempty"`
	Extra       json.RawMessage   `json:"extra"`
	Anything    any               `json:"anything"`
	Payload     Raw               `json:"payload"`
	Code        Code              `json:"code"`
	ContentType string            `json:"content-type"`
	Dash        string            `json:"-,"` //nolint:staticcheck // SA5008: the key "-", a mapping case under test
	Skipped     string            `json:"-"`
	Inline      struct{ A int }   `json:"inline"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	unexported  string            //nolint:unused // encoding/json skips it; so must the generator
}

// CreatePart is the body of the create mutation; its id is the client's (D18).
type CreatePart struct {
	ID   uuid.UUID `json:"id,omitzero"`
	Name string    `json:"name" validate:"required"`
	Kind Kind      `json:"kind"`
}

// CreateLabel is a create body whose client id is a named string.
type CreateLabel struct {
	ID   Color  `json:"id"`
	Name string `json:"name"`
}

// CreateNote is a create body whose client id is a pointer.
type CreateNote struct {
	ID   *uuid.UUID `json:"id,omitempty"`
	Text string     `json:"text"`
}

// RenamePart is the body of the rename mutation.
type RenamePart struct {
	Name string `json:"name"`
}

// Page is a generic DTO: its instantiation is named after its argument.
type Page[Item any] struct {
	Items []Item `json:"items"`
	Count int    `json:"count"`
}

// Node is recursive.
type Node struct {
	Name     string `json:"name"`
	Children []Node `json:"children"`
}

// Stat is the item of the test list; Hits is guarded.
type Stat struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Hits int    `json:"hits"`
}

// Dashboard is the item of the test object.
type Dashboard struct {
	Title string        `json:"title"`
	Pages []Page[Stat]  `json:"pages"`
	Tree  Node          `json:"tree"`
	Limit time.Duration `json:"limit"`
}

// Attachment answers the upload.
type Attachment struct {
	ID   uuid.UUID `json:"id"`
	Size int64     `json:"size"`
	Type string    `json:"type"`
}

// Note is the item of the resource that is not live.
type Note struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}
