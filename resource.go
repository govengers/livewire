package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// ReadParametersFrom reads the reserved parameters from source; a repeated one is an *Error
// of 400 invalid_request naming it.
func ReadParametersFrom(source ParameterSource) (*ReadParameters, error) {
	values := map[string]string{}
	for _, name := range []string{ParameterFilter, ParameterFilterID, ParameterSort, ParameterLimit, ParameterOffset, ParameterAfter, ParameterField} {
		read, err := readQueryValues(source, name, false)
		if err != nil {
			return nil, err
		}
		if read != nil {
			values[name] = read[0]
		}
	}
	parameters := &ReadParameters{
		FilterID: values[ParameterFilterID],
		Sort:     values[ParameterSort],
		Limit:    values[ParameterLimit],
		Offset:   values[ParameterOffset],
		After:    values[ParameterAfter],
		Field:    values[ParameterField],
	}
	if filter, ok := values[ParameterFilter]; ok {
		parameters.Filter = json.RawMessage(filter)
	}
	return parameters, nil
}

// ItemKey says how a resource's item read addresses one row: the path parameter of the
// item route (`{Parameter}` below the resource path) and the declared field it matches,
// which must be filterable and of type uuid, string, text, enum, int or time.
type ItemKey struct {
	Parameter string
	Field     string
}

// Page is one page a filterable resource's List loader returns (D14, D38, D55): the items in
// page order, the total under scope and filter (at most CountCap + 1 under a cap) and, when
// the page is full, the cursor after its last row (nil otherwise), unencoded. The engine
// answers Next encoded as ListResponse.next for a resource in cursor mode and ignores it
// otherwise, so a sort value without a cursor form fails only the reads that need the
// cursor. postgresadapter.ListPage returns one.
type Page[Item any] struct {
	Items []Item
	Total int
	Next  *query.Cursor
}

func (p *Page[Item]) parts() (items any, total int, next *query.Cursor) {
	return p.Items, p.Total, p.Next
}

// loadedPage is a *Page of any item type, as the engine reads it.
type loadedPage interface {
	parts() (items any, total int, next *query.Cursor)
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
// the `after` parameter and answers the Next of List's Page as ListResponse.next (D38, D55).
// Versioned marks the item as carrying `version` (D34); History, which needs Versioned and
// the Item key on the uuid field "id" (its SQL the table's id column), reads one item's versions for the history route (HistoryQuery
// says what it gets) and the subscription of it re-runs on the resource's Tables, which
// every version is written with. Export streams every row under scope, filter and sort to
// the writer for the export route (D35) and returns how many it wrote: the columns are
// ExportColumns, or every field, without those whose Read check refuses the caller, and a
// repository passes them to postgresadapter.WriteCSV. Partition names a subscription's
// partition (D25); nil means the tenant.
type ResourceOptions[Item any] struct {
	Tables          []string
	Read            authorization.Check
	Partition       PartitionResolver
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
	List            func(ctx context.Context, listQuery *query.ListQuery) (*Page[Item], error)
	Get             func(ctx context.Context, listQuery *query.ListQuery) (*Item, error)
	Distinct        func(ctx context.Context, field string, listQuery *query.ListQuery) ([]DistinctValue, error)
	History         func(ctx context.Context, listQuery *query.ListQuery) (*Page[Version[Item]], error)
	Export          func(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error)
}

// Resource is a filterable list of items with optional item, distinct and history reads and
// an export. Its routes are GET Path (list), GET ItemPath (one item), GET DistinctPath
// (distinct values), GET HistoryPath (one item's versions) and GET ExportPath (every row as
// CSV); all take the reserved parameters.
type Resource struct {
	signature
	liveness
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
	list            func(ctx context.Context, listQuery *query.ListQuery) (loadedPage, error)
	get             func(ctx context.Context, listQuery *query.ListQuery) (any, error)
	distinct        func(ctx context.Context, field string, listQuery *query.ListQuery) ([]DistinctValue, error)
	history         func(ctx context.Context, listQuery *query.ListQuery) (loadedPage, error)
	export          func(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error)
}

// NewResource declares a filterable resource of Item under path (no placeholders). The
// options are copied; changing them afterwards changes nothing. A mistake in the declaration
// is reported by Registry.Add, never by a panic (D43).
func NewResource[Item any](name, path string, options *ResourceOptions[Item]) *Resource {
	if options == nil {
		options = &ResourceOptions[Item]{}
	}
	resource := &Resource{
		signature:       newSignature(name, http.MethodGet, path, options.Tables, options.Read),
		itemType:        reflect.TypeFor[Item](),
		fields:          cloneFields(options.Fields),
		defaultSort:     slices.Clone(options.DefaultSort),
		scope:           options.Scope,
		maximumLimit:    intOrDefault(options.MaximumLimit, DefaultMaximumLimit),
		countCap:        options.CountCap,
		distinctMaximum: intOrDefault(options.DistinctMaximum, DefaultDistinctMaximum),
		versioned:       options.Versioned,
		exportColumns:   slices.Clone(options.ExportColumns),
		cursor:          options.Cursor,
		distinct:        options.Distinct,
		export:          options.Export,
	}
	resource.partition = options.Partition
	if options.Item != nil {
		resource.item = &ItemKey{Parameter: options.Item.Parameter, Field: options.Item.Field}
	}
	if list := options.List; list != nil {
		resource.list = func(ctx context.Context, listQuery *query.ListQuery) (loadedPage, error) {
			return loadPageOf(ctx, name, "List", list, listQuery)
		}
	}
	if history := options.History; history != nil {
		resource.history = func(ctx context.Context, listQuery *query.ListQuery) (loadedPage, error) {
			return loadPageOf(ctx, name, "History", history, listQuery)
		}
	}
	if get := options.Get; get != nil {
		resource.get = func(ctx context.Context, listQuery *query.ListQuery) (any, error) {
			return get(ctx, listQuery)
		}
	}
	resource.compileParameters(reflect.TypeFor[NoParameters]())
	resource.failWith(refusePointerLoops(resource.itemType))
	resource.failWith(resource.validate(options.MaximumLimit, options.DistinctMaximum))
	resource.deriveNullable()
	return resource
}

// loadPageOf runs a page loader of the resource name: a copy of its page with Items never
// nil, or a failure when it returned neither a page nor an error.
func loadPageOf[Item any](ctx context.Context, name, loader string, load func(context.Context, *query.ListQuery) (*Page[Item], error), listQuery *query.ListQuery) (loadedPage, error) {
	page, err := load(ctx, listQuery)
	switch {
	case err != nil:
		return nil, err
	case page == nil:
		return nil, fmt.Errorf("livewire: the %s loader of %q returned no page and no error", loader, name)
	}
	answered := *page
	if answered.Items == nil {
		answered.Items = []Item{}
	}
	return &answered, nil
}

// Kind is KindResource.
func (r *Resource) Kind() Kind { return KindResource }

// ItemType is the Go type of one item.
func (r *Resource) ItemType() reflect.Type { return r.itemType }

// Fields are the declared fields with Nullable derived from the item type; the result is
// the caller's copy.
func (r *Resource) Fields() query.Fields { return cloneFields(r.fields) }

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

// PresencePath is the route of the presence read (`Path/presence`, the record named by the
// `key` parameter, D37), or "" without an item read.
func (r *Resource) PresencePath() string {
	if r.item == nil {
		return ""
	}
	return r.path + "/presence"
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

// ListQuery turns the reserved parameters of a list read into the loader's query under the
// identity in ctx. The whole request is parsed first against every field, so an unknown
// field, a bad page or a bad cursor is 400 (invalid_*) whoever asks; then the fields the
// filter and the sort name are checked against the column checks, so a guarded one is 403
// (forbidden_field, D39); search runs over the visible searchable fields only. An empty sort
// is the default sort; the page is clamped to MaximumLimit; `after` is refused unless the
// resource pages by cursor. Scope runs last.
func (r *Resource) ListQuery(ctx context.Context, parameters *ReadParameters) (*query.ListQuery, error) {
	parsed, err := r.parse(parameters, r.maximumLimit)
	if err != nil {
		return nil, err
	}
	return r.compile(ctx, parsed)
}

// DistinctQuery is ListQuery for a distinct read (D15): it also resolves the `field`
// parameter, which must name a visible filterable scalar field, and caps the page at
// DistinctMaximum.
func (r *Resource) DistinctQuery(ctx context.Context, parameters *ReadParameters) (*query.ListQuery, query.Field, error) {
	parsed, err := r.parse(parameters, r.distinctMaximum)
	if err != nil {
		return nil, query.Field{}, err
	}
	field, err := r.distinctField(parameters.Field)
	if err != nil {
		return nil, query.Field{}, err
	}
	parsed.guarded = append(parsed.guarded, field.Name)
	listQuery, err := r.compile(ctx, parsed)
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
	parsed, err := r.parse(&ReadParameters{Filter: condition, Limit: "1"}, 1)
	var queryError *query.Error
	if errors.As(err, &queryError) {
		return nil, invalidParameter(r.item.Parameter, "must be a "+string(field.Type))
	}
	if err != nil {
		return nil, err
	}
	return r.compile(ctx, parsed)
}

// LoadList runs the List loader; the page is the loader's *Page[Item], a copy whose Items are
// never nil. A loader that returns no page and no error fails.
func (r *Resource) LoadList(ctx context.Context, listQuery *query.ListQuery) (any, error) {
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

func (r *Resource) declaration() Declaration { return r }

func (r *Resource) routes() []route {
	routes := []route{{http.MethodGet, r.path}}
	if r.item != nil {
		routes = append(routes, route{http.MethodGet, r.ItemPath()})
	}
	if r.distinct != nil {
		routes = append(routes, route{http.MethodGet, r.DistinctPath()})
	}
	if r.history != nil {
		routes = append(routes, route{http.MethodGet, r.HistoryPath()})
	}
	if r.export != nil {
		routes = append(routes, route{http.MethodGet, r.ExportPath()})
	}
	if r.item != nil {
		routes = append(routes, route{http.MethodGet, r.PresencePath()})
	}
	return routes
}

func (r *Resource) validate(maximumLimit, distinctMaximum int) error {
	negativeLimit := maximumLimit < 0 || r.countCap < 0 || distinctMaximum < 0
	switch {
	case r.list == nil:
		return errors.New("List is required")
	case len(r.fields) == 0:
		return errors.New("Fields is required: a resource is filterable")
	case negativeLimit:
		return errors.New("MaximumLimit, CountCap and DistinctMaximum must not be negative")
	case r.item == nil && r.get != nil:
		return errors.New("the Get loader needs Item: the item route is declared through it")
	}
	if err := r.fields.Validate(); err != nil {
		return err
	}
	if err := r.validateDefaultSort(); err != nil {
		return err
	}
	for _, column := range r.exportColumns {
		if _, ok := r.fields.Lookup(column); !ok {
			return fmt.Errorf("ExportColumns names %q, which is not a field", column)
		}
	}
	if err := r.validateItem(); err != nil {
		return err
	}
	if err := r.validateHistory(); err != nil {
		return err
	}
	if err := r.validateGuardedFields(); err != nil {
		return err
	}
	if r.live() {
		if err := requireJSONField(r.itemType, keyFieldJSON, liveListNeedsID); err != nil {
			return err
		}
	}
	if r.cursor {
		if err := r.validateCursorID(); err != nil {
			return err
		}
	}
	if r.versioned {
		return requireJSONField(r.itemType, "version", "a versioned resource exposes it, D34")
	}
	return nil
}

// validateGuardedFields checks that every field with a column check names a JSON key of the
// item, so redaction can strip it; a guard on a name the item never marshals would strip
// nothing (D39).
func (r *Resource) validateGuardedFields() error {
	for _, field := range r.fields {
		guarded := field.Read != nil || field.Write != nil
		if !guarded {
			continue
		}
		if err := requireJSONField(r.itemType, field.Name, "a guarded field is redacted by it"); err != nil {
			return err
		}
		guardedLiveID := r.live() && field.Name == keyFieldJSON && field.Read != nil
		if guardedLiveID {
			return errors.New(`the field "id" of a live resource cannot have a Read check: a list without ids cannot be diffed`)
		}
	}
	return nil
}

// validateCursorID checks the cursor contract of the query package: the cursor carries the
// row's id as a uuid, so a resource that pages by cursor declares a uuid field "id".
func (r *Resource) validateCursorID() error {
	field, ok := r.fields.Lookup(keyFieldJSON)
	uuidID := ok && field.Type == query.TypeUUID
	if !uuidID {
		return errors.New(`Cursor needs a field "id" of type uuid: the cursor carries the row's id as a uuid`)
	}
	return nil
}

func (r *Resource) validateDefaultSort() error {
	seen := map[string]bool{}
	for _, term := range r.defaultSort {
		field, ok := r.fields.Lookup(term.Field)
		sortable := ok && field.Sort
		switch {
		case !sortable:
			return fmt.Errorf("DefaultSort names %q, which is not a sortable field", term.Field)
		case field.Read != nil:
			return fmt.Errorf("DefaultSort names the guarded field %q; a default sort must be readable by everyone", term.Field)
		case seen[term.Field]:
			return fmt.Errorf("DefaultSort names %q twice", term.Field)
		}
		seen[term.Field] = true
	}
	return nil
}

var itemKeyTypes = []query.FieldType{query.TypeUUID, query.TypeString, query.TypeText, query.TypeEnum, query.TypeInteger, query.TypeTime}

func (r *Resource) validateItem() error {
	if r.item == nil {
		return nil
	}
	field, ok := r.fields.Lookup(r.item.Field)
	filterable := ok && field.Filter
	switch {
	case r.item.Parameter == "" || r.item.Field == "":
		return errors.New("Item needs Parameter and Field")
	case !filterable:
		return fmt.Errorf("Item.Field %q is not a filterable field", r.item.Field)
	case !slices.Contains(itemKeyTypes, field.Type):
		return fmt.Errorf("Item.Field %q is %s; an item key is uuid, string, text, enum, int or time", r.item.Field, field.Type)
	case r.get == nil:
		return errors.New("the Get loader is required with Item")
	}
	if !identifier.MatchString(r.item.Parameter) {
		return fmt.Errorf("Item.Parameter %q is not an identifier", r.item.Parameter)
	}
	return requireJSONField(r.itemType, r.item.Field, "the item key reads it")
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

// parsedRead is a read request parsed against every field, before any column check.
type parsedRead struct {
	document *query.Document
	sort     []query.SortTerm
	page     query.Page
	guarded  []string // the fields the request names, checked against the column checks
}

func (r *Resource) parse(parameters *ReadParameters, maximumLimit int) (*parsedRead, error) {
	parsed := &parsedRead{}
	var err error
	if parsed.document, err = r.parseFilter(parameters.Filter); err != nil {
		return nil, err
	}
	if parsed.sort, err = r.parseSort(parameters.Sort); err != nil {
		return nil, err
	}
	if parsed.page, err = r.parsePage(parameters, maximumLimit); err != nil {
		return nil, err
	}
	if err := cursorMatchesSort(parsed.page.After, parsed.sort); err != nil {
		return nil, err
	}
	if parsed.document != nil {
		parsed.guarded = append(parsed.guarded, parsed.document.FieldNames()...)
	}
	for _, term := range parsed.sort {
		parsed.guarded = append(parsed.guarded, term.Field)
	}
	return parsed, nil
}

func (r *Resource) parseFilter(raw json.RawMessage) (*query.Document, error) {
	if len(raw) == 0 {
		return nil, nil //nolint:nilnil // no filter is a valid, empty document for query.Compile
	}
	return query.Parse(r.fields, raw)
}

func (r *Resource) parseSort(raw string) ([]query.SortTerm, error) {
	terms, err := query.ParseSort(r.fields, raw)
	if err != nil {
		return nil, err
	}
	if len(terms) == 0 {
		return r.defaultSort, nil
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

func (r *Resource) distinctField(name string) (query.Field, error) {
	if name == "" {
		return query.Field{}, invalidParameter(ParameterField, "required")
	}
	field, ok := r.fields.Lookup(name)
	scalar := ok && field.Filter && field.Type != query.TypeStringArray
	if !scalar {
		return query.Field{}, invalidParameter(ParameterField, "must be a filterable scalar field")
	}
	return field, nil
}

// compile checks the named fields against the column checks (one rule: Fields.Visible),
// runs the scope and compiles over the visible fields.
func (r *Resource) compile(ctx context.Context, parsed *parsedRead) (*query.ListQuery, error) {
	visible := r.fields.Visible(ctx)
	for _, name := range parsed.guarded {
		if _, ok := visible.Lookup(name); !ok {
			return nil, ForbiddenField(name)
		}
	}
	var scope query.Predicate
	if r.scope != nil {
		var err error
		if scope, err = r.scope(ctx); err != nil {
			return nil, err
		}
	}
	listQuery, err := query.Compile(visible, parsed.document, parsed.sort, parsed.page, scope)
	if err != nil {
		return nil, err
	}
	listQuery.CountCap = r.countCap
	return listQuery, nil
}

// cursorMatchesSort refuses a cursor taken under another sort while the request is parsed,
// before any guard or scope runs; query.Compile would refuse it too, but later.
func cursorMatchesSort(cursor *query.Cursor, sort []query.SortTerm) error {
	if cursor == nil || slices.Equal(cursor.Sort, sort) {
		return nil
	}
	return &query.Error{Code: query.CodeInvalidCursor, Message: "the cursor is not valid for this sort", Fields: map[string]string{ParameterAfter: "taken under another sort"}}
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

// cloneFields copies the fields and their enum lists.
func cloneFields(fields query.Fields) query.Fields {
	cloned := slices.Clone(fields)
	for position := range cloned {
		cloned[position].Enum = slices.Clone(cloned[position].Enum)
	}
	return cloned
}

// validateHistory checks the history read's needs: a versioned resource whose item key is
// its uuid field "id", the column `<table>_history` keys its versions by. The history is
// scoped by the key's value, so a key on any other field would let an item the caller may
// read name the history of another row whose id equals that field's value.
func (r *Resource) validateHistory() error {
	if r.history == nil {
		return nil
	}
	if !r.versioned || r.item == nil {
		return errors.New("a History loader needs Versioned and Item: the history route is below the item route")
	}
	field, _ := r.fields.Lookup(r.item.Field)
	if field.Name != keyFieldJSON || field.Type != query.TypeUUID {
		return fmt.Errorf("a History loader needs the item key on the uuid field %q, the id the history table keys its versions by; Item.Field is %q (%s)", keyFieldJSON, r.item.Field, field.Type)
	}
	return nil
}
