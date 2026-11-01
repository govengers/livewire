// Package fixture holds the DTOs of the generator's test registry: every mapping case of
// encoding/json the generator follows. Only the tests import it.
package fixture

import (
	"encoding/json"
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

// Code marshals itself as text.
type Code struct{ value string }

// MarshalText writes the code.
func (c Code) MarshalText() ([]byte, error) { return []byte(strings.ToUpper(c.value)), nil }

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
