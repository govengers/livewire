package livewire

import (
	"context"
	"fmt"
	"io"

	"github.com/govengers/livewire/query"
)

// ParameterFormat is the parameter of an export that names its format (D35); ExportCSV, the
// only one the core serves, is the default.
const ParameterFormat = "format"

// ExportCSV is the format of a CSV export.
const ExportCSV = "csv"

// ExportRequest is one export (D35) as the REST adapter hands it to Engine.Export: the
// resource's name, the request's parameters (the reserved ones of a list read and `format`)
// and the writer the export streams to.
type ExportRequest struct {
	Target string
	Source ParameterSource
	Writer io.Writer
}

// Export streams every row of a filterable resource under the identity in ctx (D35), REST
// only: authorized and resolved like a list read (the static Check, the reserved parameters
// with a stored filter resolved, the tenant), the format checked (csv, the default; another
// is 400 at "format"), then in a read-only engine transaction the scope, the filter and sort
// compiled like a list read's and the Export loader run with the export columns whose Read
// check passes (D39) and request.Writer. Nothing is written before the loader writes, so a
// refusal or a failure before it can still be answered as an error body. It returns the rows
// the loader wrote and reports an OutcomeExport carrying them.
func (e *Engine) Export(ctx context.Context, request ExportRequest) (rows int, err error) {
	unit := startUnit(OutcomeExport, "", request.Target)
	resolved, err := e.resolveExport(ctx, request)
	if err == nil {
		ctx = WithTenant(ctx, resolved.tenant)
		_, err = e.inReadTransaction(ctx, resolved, func(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
			var exportError error
			rows, exportError = exportRows(ctx, resolved.plan.resource, resolved.readParameters, request.Writer)
			return &readResult{}, exportError
		})
	}
	outcome := unit.readOutcome(ctx, resolved, err)
	outcome.Rows = rows
	e.observe(ctx, outcome)
	return rows, err
}

// resolveExport resolves an export like the list read of its resource: a declaration
// without an Export loader is 404.
func (e *Engine) resolveExport(ctx context.Context, request ExportRequest) (*resolvedRead, error) {
	declaration, _ := e.registry.Lookup(request.Target)
	if resource, ok := declaration.(*Resource); !ok || resource.export == nil {
		return nil, NotFound()
	}
	request.Source = sourceOrEmpty(request.Source)
	resolved, err := e.resolve(ctx, ReadRequest{Target: request.Target, Kind: ReadMain, Source: request.Source})
	if err != nil {
		return nil, err
	}
	format, err := readQueryValues(request.Source, ParameterFormat, false)
	if err != nil {
		return nil, err
	}
	if format != nil && format[0] != ExportCSV {
		return nil, invalidParameter(ParameterFormat, "must be csv")
	}
	return resolved, nil
}

func exportRows(ctx context.Context, resource *Resource, parameters *ReadParameters, writer io.Writer) (int, error) {
	listQuery, err := resource.ListQuery(ctx, parameters)
	if err != nil {
		return 0, err
	}
	rows, err := resource.export(ctx, listQuery, resource.exportFields().Visible(ctx), writer)
	if err != nil {
		return rows, fmt.Errorf("livewire: export %q: %w", resource.Name(), err)
	}
	return rows, nil
}

// ExportPath is the route of the export (`Path/export`), or "" without an Export loader.
func (r *Resource) ExportPath() string {
	if r.export == nil {
		return ""
	}
	return r.path + "/export"
}

// exportFields are the fields an export writes, in order: ExportColumns, or every field.
func (r *Resource) exportFields() query.Fields {
	if len(r.exportColumns) == 0 {
		return r.Fields()
	}
	fields := r.Fields()
	columns := make(query.Fields, 0, len(r.exportColumns))
	for _, name := range r.exportColumns {
		field, _ := fields.Lookup(name)
		columns = append(columns, field)
	}
	return columns
}
