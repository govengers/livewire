package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"

	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire/query"
)

// The reserved parameters of a filterable resource's reads (D11, D13, D15, D38). The core
// handles them; a declaration's own parameters never use these names.
const (
	ParameterFilter   = "filter"   // an inline filter document (JSON)
	ParameterFilterID = "filterId" // a stored filter's id; the engine resolves it to the document
	ParameterSort     = "sort"     // comma-separated fields, "-" prefix for descending
	ParameterLimit    = "limit"
	ParameterOffset   = "offset"
	ParameterAfter    = "after" // the cursor of the previous page (cursor mode)
	ParameterField    = "field" // the field of a distinct read
)

// ReadParameters are the reserved parameters of one resource read as the client sent them.
// Filter is the inline document, or the stored one the engine resolved from FilterID.
type ReadParameters struct {
	Filter   json.RawMessage
	FilterID string
	Sort     string
	Limit    string
	Offset   string
	After    string
	Field    string
}

// ReadParametersFrom reads the reserved parameters from source.
func ReadParametersFrom(source ParameterSource) *ReadParameters {
	first := func(name string) string {
		values, ok := source.Query(name)
		if !ok || len(values) == 0 {
			return ""
		}
		return values[0]
	}
	parameters := &ReadParameters{
		FilterID: first(ParameterFilterID),
		Sort:     first(ParameterSort),
		Limit:    first(ParameterLimit),
		Offset:   first(ParameterOffset),
		After:    first(ParameterAfter),
		Field:    first(ParameterField),
	}
	if filter := first(ParameterFilter); filter != "" {
		parameters.Filter = json.RawMessage(filter)
	}
	return parameters
}

// ItemKey says how a resource's item read addresses one row: the path parameter of the
// item route (`{Parameter}` below the resource path) and the declared field it matches,
// which must be filterable.
type ItemKey struct {
	Parameter string
	Field     string
}

// Default limits of a filterable resource.
const (
	DefaultMaximumLimit    = 100
	DefaultDistinctMaximum = 1000
)

// ResourceOptions declares a filterable resource (D11). Read and List are required; Fields
// must validate (query.Fields.Validate) and a field left Nullable false is derived from the
// item type: a pointer field of the item is nullable. Get is required with Item and refused
// without it. Scope narrows every read and re-run before filter and count; nil is no scope.
// MaximumLimit clamps the page (default 100), CountCap caps the total (0 counts every row),
// DistinctMaximum caps a distinct read (default 1000). Cursor allows keyset pagination with
// the `after` parameter (D38). Versioned marks the item as carrying `version` and the
// history read (D34). ExportColumns names the fields an export writes; empty means all.
type ResourceOptions[Item any] struct {
	Tables          []string
	Read            authorization.Check
	Fields          query.Fields
	DefaultSort     []query.SortTerm
	Scope           func(ctx context.Context) (query.Predicate, error)
	MaximumLimit    int
	CountCap        int
	DistinctMaximum int
	Item            *ItemKey
	Versioned       bool
	ExportColumns   []string
	Cursor          bool
	List            func(ctx context.Context, listQuery *query.ListQuery) ([]Item, int, error)
	Get             func(ctx context.Context, listQuery *query.ListQuery) (*Item, error)
	Distinct        func(ctx context.Context, field string, listQuery *query.ListQuery) ([]DistinctValue, error)
}

// Resource is a filterable list of items with optional item and distinct reads. Its routes
// are GET Path (list), GET ItemPath (one item) and GET DistinctPath (distinct values); all
// take the reserved parameters.
type Resource struct {
	Signature
	itemType        reflect.Type
	fields          query.Fields
	defaultSort     []query.SortTerm
	scope           func(ctx context.Context) (query.Predicate, error)
	maximumLimit    int
	countCap        int
	distinctMaximum int
	item            *ItemKey
	versioned       bool
	exportColumns   []string
	cursor          bool
	list            func(ctx context.Context, listQuery *query.ListQuery) (any, int, error)
	get             func(ctx context.Context, listQuery *query.ListQuery) (any, error)
	distinct        func(ctx context.Context, field string, listQuery *query.ListQuery) ([]DistinctValue, error)
}

// NewResource declares a filterable resource of Item under path (no placeholders). A
// mistake in the declaration is reported by Registry.Add, never by a panic.
func NewResource[Item any](name, path string, options *ResourceOptions[Item]) *Resource {
	if options == nil {
		options = &ResourceOptions[Item]{}
	}
	resource := &Resource{
		Signature:       newSignature(name, http.MethodGet, path, options.Tables, options.Read),
		itemType:        reflect.TypeFor[Item](),
		fields:          slices.Clone(options.Fields),
		defaultSort:     slices.Clone(options.DefaultSort),
		scope:           options.Scope,
		maximumLimit:    options.MaximumLimit,
		countCap:        options.CountCap,
		distinctMaximum: options.DistinctMaximum,
		versioned:       options.Versioned,
		exportColumns:   slices.Clone(options.ExportColumns),
		cursor:          options.Cursor,
	}
	if options.Item != nil {
		resource.item = &ItemKey{Parameter: options.Item.Parameter, Field: options.Item.Field}
	}
	if options.List != nil {
		resource.list = func(ctx context.Context, listQuery *query.ListQuery) (any, int, error) {
			items, total, err := options.List(ctx, listQuery)
			if items == nil {
				items = []Item{}
			}
			return items, total, err
		}
	}
	if options.Get != nil {
		resource.get = func(ctx context.Context, listQuery *query.ListQuery) (any, error) {
			return options.Get(ctx, listQuery)
		}
	}
	resource.distinct = options.Distinct
	resource.compileParameters(reflect.TypeFor[NoParameters]())
	resource.validate(options.List == nil, options.Get == nil)
	resource.deriveNullable()
	return resource
}

func (r *Resource) validate(listMissing, getMissing bool) {
	switch {
	case listMissing:
		r.fail("List is required")
	case len(r.fields) == 0:
		r.fail("Fields is required: a resource is filterable")
	case r.maximumLimit < 0 || r.countCap < 0 || r.distinctMaximum < 0:
		r.fail("MaximumLimit, CountCap and DistinctMaximum must not be negative")
	case r.item == nil && !getMissing:
		r.fail("Get needs Item: the item route is declared through it")
	}
	if r.err != nil {
		return
	}
	if r.maximumLimit == 0 {
		r.maximumLimit = DefaultMaximumLimit
	}
	if r.distinctMaximum == 0 {
		r.distinctMaximum = DefaultDistinctMaximum
	}
	if err := r.fields.Validate(); err != nil {
		r.failWith(err)
		return
	}
	for _, term := range r.defaultSort {
		field, ok := r.fields.Lookup(term.Field)
		switch {
		case !ok || !field.Sort:
			r.fail("DefaultSort names %q, which is not a sortable field", term.Field)
		case field.Read != nil:
			r.fail("DefaultSort names the guarded field %q; a default sort must be readable by everyone", term.Field)
		}
	}
	for _, column := range r.exportColumns {
		if _, ok := r.fields.Lookup(column); !ok {
			r.fail("ExportColumns names %q, which is not a field", column)
		}
	}
	r.validateItem(getMissing)
	if len(r.tables) > 0 {
		r.failWith(requireJSONField(r.itemType, keyFieldJSON, liveListNeedsID))
	}
	if r.versioned {
		r.failWith(requireJSONField(r.itemType, "version", "a versioned resource exposes it, D34"))
	}
}

func (r *Resource) validateItem(getMissing bool) {
	if r.item == nil {
		return
	}
	field, ok := r.fields.Lookup(r.item.Field)
	switch {
	case r.item.Parameter == "" || r.item.Field == "":
		r.fail("Item needs Parameter and Field")
	case !ok || !field.Filter:
		r.fail("Item.Field %q is not a filterable field", r.item.Field)
	case getMissing:
		r.fail("Get is required with Item")
	default:
		r.failWith(requireJSONField(r.itemType, r.item.Field, "the item key reads it"))
	}
}

// deriveNullable sets Nullable on every field the item marshals from a pointer, which can
// be null; the query package needs it for sorting and cursor mode.
func (r *Resource) deriveNullable() {
	itemFields := jsonFieldsOf(r.itemType)
	for position := range r.fields {
		if r.fields[position].Nullable {
			continue
		}
		if itemField, ok := itemFields[r.fields[position].Name]; ok && itemField.Type.Kind() == reflect.Pointer {
			r.fields[position].Nullable = true
		}
	}
}

// Kind is KindResource.
func (r *Resource) Kind() Kind { return KindResource }

// ItemType is the Go type of one item.
func (r *Resource) ItemType() reflect.Type { return r.itemType }

// Fields are the declared fields with Nullable derived from the item type; the result is
// the caller's copy.
func (r *Resource) Fields() query.Fields { return slices.Clone(r.fields) }

// DefaultSort is the sort of a read that sends none; the result is the caller's copy.
func (r *Resource) DefaultSort() []query.SortTerm { return slices.Clone(r.defaultSort) }

// Item is the item key, or nil when the resource has no item read.
func (r *Resource) Item() *ItemKey {
	if r.item == nil {
		return nil
	}
	return &ItemKey{Parameter: r.item.Parameter, Field: r.item.Field}
}

// ItemPath is the route of the item read (`Path/{Item.Parameter}`), or "" without one.
func (r *Resource) ItemPath() string {
	if r.item == nil {
		return ""
	}
	return r.path + "/{" + r.item.Parameter + "}"
}

// DistinctPath is the route of the distinct read (`Path/distinct`), or "" without a Distinct
// loader.
func (r *Resource) DistinctPath() string {
	if r.distinct == nil {
		return ""
	}
	return r.path + "/distinct"
}

// Versioned reports whether the item carries `version` and a history (D34).
func (r *Resource) Versioned() bool { return r.versioned }

// Cursor reports whether the resource pages by cursor on request (D38).
func (r *Resource) Cursor() bool { return r.cursor }

// ExportColumns are the fields an export writes, every field when empty (D35); the result
// is the caller's copy.
func (r *Resource) ExportColumns() []string { return slices.Clone(r.exportColumns) }

func (r *Resource) routes() []route {
	routes := []route{{http.MethodGet, r.path}}
	if r.item != nil {
		routes = append(routes, route{http.MethodGet, r.ItemPath()})
	}
	if r.distinct != nil {
		routes = append(routes, route{http.MethodGet, r.DistinctPath()})
	}
	return routes
}

// ListQuery turns the reserved parameters of a list read into the loader's query under the
// identity in ctx: the filter is parsed against every field, so an unknown field is 400
// (invalid_filter), then the fields it names and the sort names are checked against the
// column checks, so a guarded one is 403 (forbidden_field, D39); search runs over the visible
// searchable fields only. An empty sort is the default sort; the page is clamped to
// MaximumLimit; `after` is refused unless the resource pages by cursor. Scope runs last.
func (r *Resource) ListQuery(ctx context.Context, parameters *ReadParameters) (*query.ListQuery, error) {
	return r.compile(ctx, parameters, r.maximumLimit)
}

// DistinctQuery is ListQuery for a distinct read (D15): it also resolves the `field`
// parameter, which must name a visible filterable field, and caps the page at
// DistinctMaximum.
func (r *Resource) DistinctQuery(ctx context.Context, parameters *ReadParameters) (*query.ListQuery, query.Field, error) {
	field, err := r.distinctField(ctx, parameters.Field)
	if err != nil {
		return nil, query.Field{}, err
	}
	listQuery, err := r.compile(ctx, parameters, r.distinctMaximum)
	if err != nil {
		return nil, query.Field{}, err
	}
	return listQuery, field, nil
}

// ItemQuery is the loader's query of an item read: the item key's value from source as the
// one condition, under scope. A key of the wrong form is 400; a guarded key field is 403.
func (r *Resource) ItemQuery(ctx context.Context, source ParameterSource) (*query.ListQuery, error) {
	if r.item == nil {
		return nil, NotFound()
	}
	raw, ok := source.Path(r.item.Parameter)
	if !ok || raw == "" {
		return nil, invalidParameter(r.item.Parameter, "required")
	}
	field, _ := r.fields.Lookup(r.item.Field)
	value, err := itemValue(field, raw)
	if err != nil {
		return nil, invalidParameter(r.item.Parameter, err.Error())
	}
	condition, err := json.Marshal(map[string]any{"where": map[string]any{"field": field.Name, "op": query.OperatorEqual, "value": value}})
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the item condition: %w", err)
	}
	listQuery, err := r.compile(ctx, &ReadParameters{Filter: condition, Limit: "1"}, 1)
	var queryError *query.Error
	if errors.As(err, &queryError) {
		return nil, invalidParameter(r.item.Parameter, "must be a "+string(field.Type))
	}
	return listQuery, err
}

// itemValue types the item key's path value for the filter document.
func itemValue(field query.Field, raw string) (any, error) {
	if field.Type != query.TypeInteger {
		return raw, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, errors.New("must be an integer")
	}
	return value, nil
}

func (r *Resource) compile(ctx context.Context, parameters *ReadParameters, maximumLimit int) (*query.ListQuery, error) {
	hidden := hiddenFields(ctx, r.fields)
	document, err := r.parseFilter(hidden, parameters.Filter)
	if err != nil {
		return nil, err
	}
	sort, err := r.parseSort(hidden, parameters.Sort)
	if err != nil {
		return nil, err
	}
	page, err := r.parsePage(parameters, maximumLimit)
	if err != nil {
		return nil, err
	}
	var scope query.Predicate
	if r.scope != nil {
		if scope, err = r.scope(ctx); err != nil {
			return nil, err
		}
	}
	listQuery, err := query.Compile(visibleFields(r.fields, hidden), document, sort, page, scope)
	if err != nil {
		return nil, err
	}
	listQuery.CountCap = r.countCap
	return listQuery, nil
}

func (r *Resource) parseFilter(hidden map[string]bool, raw json.RawMessage) (*query.Document, error) {
	if len(raw) == 0 {
		return nil, nil //nolint:nilnil // no filter is a valid, empty document for query.Compile
	}
	document, err := query.Parse(r.fields, raw)
	if err != nil {
		return nil, err
	}
	for _, name := range document.FieldNames() {
		if hidden[name] {
			return nil, ForbiddenField(name)
		}
	}
	return document, nil
}

func (r *Resource) parseSort(hidden map[string]bool, raw string) ([]query.SortTerm, error) {
	terms, err := query.ParseSort(r.fields, raw)
	if err != nil {
		return nil, err
	}
	if len(terms) == 0 {
		return r.defaultSort, nil
	}
	for _, term := range terms {
		if hidden[term.Field] {
			return nil, ForbiddenField(term.Field)
		}
	}
	return terms, nil
}

func (r *Resource) parsePage(parameters *ReadParameters, maximumLimit int) (query.Page, error) {
	page, err := query.ParsePage(parameters.Limit, parameters.Offset, maximumLimit)
	if err != nil {
		return query.Page{}, err
	}
	if parameters.After == "" {
		return page, nil
	}
	if !r.cursor {
		return query.Page{}, invalidParameter(ParameterAfter, "this resource does not page by cursor")
	}
	page.After, err = query.ParseCursor(r.fields, parameters.After)
	if err != nil {
		return query.Page{}, err
	}
	return page, nil
}

func (r *Resource) distinctField(ctx context.Context, name string) (query.Field, error) {
	if name == "" {
		return query.Field{}, invalidParameter(ParameterField, "required")
	}
	field, ok := r.fields.Lookup(name)
	switch {
	case !ok || !field.Filter:
		return query.Field{}, invalidParameter(ParameterField, "must be a filterable field")
	case field.Read != nil && field.Read(ctx) != nil:
		return query.Field{}, ForbiddenField(name)
	}
	return field, nil
}

// LoadList runs the List loader; the items are a []Item, never nil.
func (r *Resource) LoadList(ctx context.Context, listQuery *query.ListQuery) (items any, total int, err error) {
	return r.list(ctx, listQuery)
}

// LoadItem runs the Get loader; the item is a *Item. A loader that finds no row returns
// NotFound.
func (r *Resource) LoadItem(ctx context.Context, listQuery *query.ListQuery) (any, error) {
	if r.get == nil {
		return nil, NotFound()
	}
	item, err := r.get(ctx, listQuery)
	if err != nil {
		return nil, err
	}
	if reflect.ValueOf(item).IsNil() {
		return nil, fmt.Errorf("livewire: the Get loader of %q returned no item and no error", r.name)
	}
	return item, nil
}

// LoadDistinct runs the Distinct loader for the field DistinctQuery resolved; the values are
// never nil.
func (r *Resource) LoadDistinct(ctx context.Context, field string, listQuery *query.ListQuery) ([]DistinctValue, error) {
	if r.distinct == nil {
		return nil, NotFound()
	}
	values, err := r.distinct(ctx, field, listQuery)
	if err != nil {
		return nil, err
	}
	if values == nil {
		values = []DistinctValue{}
	}
	return values, nil
}
