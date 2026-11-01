package generate

import (
	"strings"

	"github.com/govengers/livewire"
)

// valueKind is how the Go client and the CLI carry a parameter's value; each kind has one
// wire form (client.Parameters).
type valueKind int

const (
	valueString valueKind = iota
	valueInteger
	valueBoolean
	valueUUID
	valueTime
	valueList
	valueFilter
)

// sortTerms are the terms a sort of the filter vocabulary may name: each sortable field,
// ascending and with a leading - descending.
func (r *resourceModel) sortTerms() []string {
	var terms []string
	for _, field := range r.sortFields() {
		terms = append(terms, field, "-"+field)
	}
	return terms
}

// wireParameter is one parameter of a read, mutation, upload or export as the Go client and
// the CLI send it: a declaration's own or a reserved one (D11-D15, D38), in the order both
// list them.
type wireParameter struct {
	name     string
	kind     valueKind
	required bool
	path     bool
	pointer  bool
	values   []string
	filters  *resourceModel
	page     bool
	usage    string
}

// wireParameters lists a read's own parameters, then its reserved ones.
func wireParameters(parameters []parameterModel, reserved reservedModel) []wireParameter {
	var listed []wireParameter
	for _, parameter := range parameters {
		kind, values := kindOf(parameter.shape)
		listed = append(listed, wireParameter{
			name: parameter.name, kind: kind, required: parameter.required, path: parameter.in == livewire.InPath,
			pointer: parameter.pointer, values: values, usage: parameterUsage(parameter.in, values),
		})
	}
	if reserved.filters != nil {
		listed = append(listed,
			wireParameter{name: livewire.ParameterFilter, kind: valueFilter, filters: reserved.filters, usage: "an inline filter document (JSON, D12)"},
			wireParameter{name: livewire.ParameterFilterID, kind: valueString, usage: "the id of a stored filter (D13)"})
	}
	if reserved.field {
		fields := reserved.filters.distinctFields()
		listed = append(listed, wireParameter{name: livewire.ParameterField, kind: valueString, required: true, values: fields, filters: reserved.filters, usage: "the field whose values are counted (D15), one of " + strings.Join(fields, ", ")})
	}
	if reserved.sort {
		listed = append(listed, wireParameter{name: livewire.ParameterSort, kind: valueList, values: reserved.filters.sortTerms(), filters: reserved.filters, usage: "sort terms, a leading - descending (" + strings.Join(reserved.filters.sortFields(), ", ") + ")"})
	}
	if reserved.page {
		listed = append(listed,
			wireParameter{name: livewire.ParameterLimit, kind: valueInteger, page: true, usage: "the page size, clamped to the maximum (D14)"},
			wireParameter{name: livewire.ParameterOffset, kind: valueInteger, page: true, usage: "the rows to skip"})
	}
	if reserved.after {
		listed = append(listed, wireParameter{name: livewire.ParameterAfter, kind: valueString, usage: "the next of the previous page (cursor mode, D38)"})
	}
	return listed
}

func kindOf(value *shape) (valueKind, []string) {
	switch value.kind {
	case shapeArray:
		_, values := kindOf(value.element)
		return valueList, values
	case shapeBoolean:
		return valueBoolean, nil
	case shapeInteger:
		return valueInteger, nil
	case shapeEnum:
		return valueString, value.values
	default:
	}
	switch value.format {
	case "uuid":
		return valueUUID, nil
	case "date-time":
		return valueTime, nil
	default:
		return valueString, nil
	}
}

func parameterUsage(location livewire.ParameterIn, values []string) string {
	usage := "query parameter"
	if location == livewire.InPath {
		usage = "path parameter"
	}
	if len(values) > 0 {
		usage += ", one of " + strings.Join(values, ", ")
	}
	return usage
}
