package query

import (
	"context"
	"fmt"
)

// FieldType is the type of a field as the API exposes it. It decides which operators a
// filter may use on the field (OperatorsOf) and how a value is decoded.
type FieldType string

// The field types. TypeFloat reads a real or double precision expression and binds its
// values as float8; a numeric column is declared as `t.amount::float8` and accepts float
// comparison semantics. TypeStringArray reads a text[] expression and is filterable but not
// sortable; TypeJSON is neither.
const (
	TypeString      FieldType = "string"
	TypeText        FieldType = "text"
	TypeInteger     FieldType = "int"
	TypeFloat       FieldType = "float"
	TypeBoolean     FieldType = "bool"
	TypeTime        FieldType = "time"
	TypeUUID        FieldType = "uuid"
	TypeEnum        FieldType = "enum"
	TypeStringArray FieldType = "stringArray"
	TypeJSON        FieldType = "json"
)

var knownFieldTypes = map[FieldType]bool{
	TypeString: true, TypeText: true, TypeInteger: true, TypeFloat: true, TypeBoolean: true,
	TypeTime: true, TypeUUID: true, TypeEnum: true, TypeStringArray: true, TypeJSON: true,
}

// Field is one field of a filterable resource: its API name (the JSON name of the item), its
// type and the SQL expression that reads it over the resource's FROM. Filter, Sort and
// Search say what a client may do with it. Read and Write are the column checks of D39:
// nil passes; this package stores them and evaluates Read in Fields.Visible only, the root
// package decides what a refusal means.
type Field struct {
	Name string
	Type FieldType
	// SQL is the expression over the resource's FROM, written into the statement as it is.
	SQL string
	// Nullable must be true whenever SQL can yield NULL. A nullable term keeps the rows
	// without a value last on a descending sort too, where PostgreSQL would put them first,
	// and cursor mode walks them. A term declared non-nullable over a column with NULLs is
	// wrong in cursor mode: ascending it skips the NULL rows, descending a page that ends
	// among the leading NULLs ends the walk and loses every row after them. Only a
	// non-nullable leading sort term gives a cursor page the index range bound; a Nullable
	// leading term walks correctly but without it.
	Nullable bool
	// Enum lists the values of a TypeEnum field; a filter refuses any other value.
	Enum   []string
	Filter bool
	Sort   bool
	Search bool
	Read   func(context.Context) error
	Write  func(context.Context) error
}

// Fields is the ordered field list of one resource.
type Fields []Field

// Lookup returns the field with the given API name.
func (f Fields) Lookup(name string) (Field, bool) {
	for _, field := range f {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}

// Validate checks the declaration: names present and unique, a known type, SQL present,
// enum values on exactly the enum fields, Search only on string and text, Filter never on
// json, Sort never on json or stringArray. The error names the first field that is wrong;
// it is a declaration mistake, not an *Error.
func (f Fields) Validate() error {
	seen := make(map[string]bool, len(f))
	for _, field := range f {
		if err := validateField(field); err != nil {
			return err
		}
		if seen[field.Name] {
			return fmt.Errorf("query: field %q is declared twice", field.Name)
		}
		seen[field.Name] = true
	}
	return nil
}

func validateField(field Field) error {
	switch {
	case field.Name == "":
		return fmt.Errorf("query: a field has no name")
	case !knownFieldTypes[field.Type]:
		return fmt.Errorf("query: field %q has unknown type %q", field.Name, field.Type)
	case field.SQL == "":
		return fmt.Errorf("query: field %q has no SQL expression", field.Name)
	case field.Type == TypeEnum && len(field.Enum) == 0:
		return fmt.Errorf("query: enum field %q has no values", field.Name)
	case field.Type != TypeEnum && len(field.Enum) > 0:
		return fmt.Errorf("query: field %q has enum values but is %s", field.Name, field.Type)
	case field.Search && field.Type != TypeString && field.Type != TypeText:
		return fmt.Errorf("query: field %q is searchable but is %s, not string or text", field.Name, field.Type)
	case field.Filter && field.Type == TypeJSON:
		return fmt.Errorf("query: json field %q cannot be filterable", field.Name)
	case field.Sort && (field.Type == TypeJSON || field.Type == TypeStringArray):
		return fmt.Errorf("query: %s field %q cannot be sortable", field.Type, field.Name)
	}
	return nil
}

// Visible returns the fields whose Read check passes in ctx; a nil check passes. The
// result is a new list, the receiver is unchanged.
func (f Fields) Visible(ctx context.Context) Fields {
	visible := make(Fields, 0, len(f))
	for _, field := range f {
		if field.Read == nil || field.Read(ctx) == nil {
			visible = append(visible, field)
		}
	}
	return visible
}
