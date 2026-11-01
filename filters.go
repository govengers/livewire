package livewire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/query"
)

// The routes the library serves itself, below the registry's BasePath; no declaration may
// take them (Registry.Add refuses one that does).
const (
	BatchPath   = "/batch"   // POST: a batch of mutations (D33, D47)
	FiltersPath = "/filters" // POST stores, GET lists the caller's stored filters; DELETE FiltersPath/{id} removes one (D13)
)

// The targets of the outcomes of the library's own routes (D59); both are reserved names.
// BatchTarget is also the prefix of the name a batch's Idempotency-Key is reserved under.
const (
	BatchTarget   = "batch"
	FiltersTarget = "filters"
)

// reservedRoutes are the library's own routes as route patterns.
var reservedRoutes = map[route]bool{
	{http.MethodPost, BatchPath}:             true,
	{http.MethodPost, FiltersPath}:           true,
	{http.MethodGet, FiltersPath}:            true,
	{http.MethodDelete, FiltersPath + "/{}"}: true,
}

// FilterRequest is the body of POST /filters (D13): the resource the filter is for (its name,
// or "name.history" for its history read, D56), the application's context of the dedup key ("" for none), a name ("" for an unnamed filter,
// which is swept after 30 days without use) and the filter document.
type FilterRequest struct {
	Resource string          `json:"resource"`
	Context  string          `json:"context,omitempty"`
	Name     string          `json:"name,omitempty"`
	Filter   json.RawMessage `json:"filter"`
}

// FilterItem is a stored filter as the filter routes answer it: Filter is the canonical
// document a filterId read applies.
type FilterItem struct {
	ID         string          `json:"id"`
	Resource   string          `json:"resource"`
	Context    string          `json:"context,omitempty"`
	Name       string          `json:"name,omitempty"`
	Filter     json.RawMessage `json:"filter"`
	CreatedAt  time.Time       `json:"createdAt"`
	LastUsedAt time.Time       `json:"lastUsedAt"`
}

// SaveFilter stores a filter for the caller (POST /filters, D13): body is a FilterRequest
// (unknown fields refused); the resource must be a registered filterable resource, or
// "name.history" for its history read over HistoryFields (D56), whose static Check passes, the document must parse against its fields and name only fields the
// caller may read (D39). The filter is the owner's under the tenant of a resource read
// (D42). The payload is the stored FilterItem, answered with 201; an unnamed filter the
// owner stored already for the resource and context is that filter. Without a FilterStore
// it is 404, without an identity 401. It reports an OutcomeRequest with the target
// "filters".
func (e *Engine) SaveFilter(ctx context.Context, body []byte) (json.RawMessage, error) {
	unit := startUnit(OutcomeRequest, "", FiltersTarget)
	payload, owner, err := e.saveFilter(ctx, body)
	e.observeFilters(ctx, unit, owner, err)
	return payload, err
}

func (e *Engine) saveFilter(ctx context.Context, body []byte) (json.RawMessage, Owner, error) {
	var request FilterRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || !atEnd(decoder) {
		return nil, Owner{}, invalidParameter("body", "must be one JSON object {resource, context, name, filter}")
	}
	resource, fields, err := e.filterTarget(request.Resource)
	if err != nil {
		return nil, Owner{}, err
	}
	if err := resource.Check()(ctx); err != nil {
		return nil, Owner{}, err
	}
	owner, store, err := e.filterOwner(ctx)
	if err != nil {
		return nil, owner, err
	}
	ctx = WithTenant(ctx, owner.Tenant)
	request.Resource = canonicalTarget(request.Resource)
	filter, err := storedFilter(ctx, owner, fields, &request)
	if err != nil {
		return nil, owner, err
	}
	var stored *StoredFilter
	err = recovering(func() error {
		var saveError error
		stored, saveError = store.Save(ctx, filter)
		return saveError
	})
	if err != nil {
		return nil, owner, fmt.Errorf("livewire: store a filter of %s: %w", resource.Name(), err)
	}
	payload, err := marshalFilter(filterItemOf(stored))
	return payload, owner, err
}

// filterTarget resolves the target a stored filter belongs to: a filterable resource's name,
// or "name.history" for the history read of one with a History loader (D56); it returns the
// resource and the fields the filter's document names.
func (e *Engine) filterTarget(target string) (*Resource, query.Fields, error) {
	name, kind := parseTarget(target)
	declaration, _ := e.registry.Lookup(name)
	resource, ok := declaration.(*Resource)
	switch {
	case ok && kind == ReadMain:
		return resource, resource.Fields(), nil
	case ok && kind == ReadHistory && resource.HistoryPath() != "":
		return resource, HistoryFields(), nil
	}
	return nil, nil, invalidParameter("resource", "is not a filterable resource or the history of one")
}

// canonicalTarget is the target a filter is stored and looked up under: the name for a main
// read ("documents." is "documents"), the name and ".history" for a history read.
func canonicalTarget(target string) string {
	name, kind := parseTarget(target)
	return ReadRequest{Target: name, Kind: kind}.target()
}

// storedFilter is the filter a request stores: its document parsed and checked like a read's
// (D39), in canonical form with its hash.
func storedFilter(ctx context.Context, owner Owner, fields query.Fields, request *FilterRequest) (*StoredFilter, error) {
	if len(bytes.TrimSpace(request.Filter)) == 0 {
		return nil, invalidParameter(ParameterFilter, "required")
	}
	document, err := visibleFilter(ctx, fields, request.Filter)
	if err != nil {
		return nil, err
	}
	canonical, err := query.Canonical(document)
	if err != nil {
		return nil, fmt.Errorf("livewire: the canonical form of a filter: %w", err)
	}
	hash, err := query.Hash(document)
	if err != nil {
		return nil, fmt.Errorf("livewire: the hash of a filter: %w", err)
	}
	return &StoredFilter{Owner: owner, Resource: request.Resource, Context: request.Context, Name: request.Name, Document: canonical, Hash: hash}, nil
}

// observeFilters reports a filter route's outcome under the owner's tenant.
func (e *Engine) observeFilters(ctx context.Context, unit unitOfWork, owner Owner, err error) {
	outcome := unit.outcome(ctx, err)
	outcome.Tenant = owner.Tenant
	e.observe(ctx, outcome)
}

// ListFilters answers the caller's named filters of a resource (or "name.history"), of every
// one when resource is "" (GET /filters, D13), as a ListResponse of FilterItem; unnamed
// filters are registrations and not listed. Every filter is authorized again under the
// caller as SaveFilter does: one whose resource's static Check refuses the caller now, or
// whose document names a field the caller may no longer read (D39), is left out. A resource
// that is not a registered filterable resource is 400; without a FilterStore it is 404,
// without an identity 401.
func (e *Engine) ListFilters(ctx context.Context, resource string) (json.RawMessage, error) {
	unit := startUnit(OutcomeRequest, "", FiltersTarget)
	payload, owner, err := e.listFilters(ctx, resource)
	e.observeFilters(ctx, unit, owner, err)
	return payload, err
}

func (e *Engine) listFilters(ctx context.Context, resource string) (json.RawMessage, Owner, error) {
	if resource != "" {
		if _, _, err := e.filterTarget(resource); err != nil {
			return nil, Owner{}, err
		}
		resource = canonicalTarget(resource)
	}
	owner, store, err := e.filterOwner(ctx)
	if err != nil {
		return nil, owner, err
	}
	var stored []StoredFilter
	err = recovering(func() error {
		var listError error
		stored, listError = store.List(WithTenant(ctx, owner.Tenant), owner, resource)
		return listError
	})
	if err != nil {
		return nil, owner, fmt.Errorf("livewire: list the stored filters: %w", err)
	}
	items := make([]FilterItem, 0, len(stored))
	for index := range stored {
		if e.mayUseFilter(WithTenant(ctx, owner.Tenant), &stored[index]) {
			items = append(items, filterItemOf(&stored[index]))
		}
	}
	payload, err := marshalFilter(ListResponse[FilterItem]{Limit: len(items), Total: len(items), Items: items})
	return payload, owner, err
}

// DeleteFilter removes the caller's stored filter (DELETE /filters/{id}, D13); a filter that
// is not there, or another owner's, is no error. Without a FilterStore it is 404, without an
// identity 401.
func (e *Engine) DeleteFilter(ctx context.Context, id string) error {
	unit := startUnit(OutcomeRequest, "", FiltersTarget)
	owner, store, err := e.filterOwner(ctx)
	if err == nil {
		err = recovering(func() error { return store.Delete(WithTenant(ctx, owner.Tenant), owner, id) })
		if err != nil {
			err = fmt.Errorf("livewire: delete the stored filter %q: %w", id, err)
		}
	}
	e.observeFilters(ctx, unit, owner, err)
	return err
}

// filterOwner is the caller's owner under the tenant of a resource read (nil parameters,
// D42) and the store of a filter route.
func (e *Engine) filterOwner(ctx context.Context) (Owner, FilterStore, error) {
	store := e.options.FilterStore
	if store == nil {
		return Owner{}, nil, NotFound()
	}
	if auth.IdentityFrom(ctx) == nil {
		return Owner{}, nil, auth.ErrUnauthenticated
	}
	tenant, err := e.registry.Tenant(ctx, nil)
	if err != nil {
		return Owner{}, nil, err
	}
	owner, _ := OwnerOf(WithTenant(ctx, tenant))
	return owner, store, nil
}

func filterItemOf(stored *StoredFilter) FilterItem {
	return FilterItem{
		ID: stored.ID, Resource: stored.Resource, Context: stored.Context, Name: stored.Name,
		Filter: stored.Document, CreatedAt: stored.CreatedAt, LastUsedAt: stored.LastUsedAt,
	}
}

func marshalFilter(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the stored filters: %w", err)
	}
	return payload, nil
}

// mayUseFilter reports whether the caller in ctx may read with a stored filter now: its
// target is still registered, the resource's static Check passes and the document names
// only fields the caller may read.
func (e *Engine) mayUseFilter(ctx context.Context, stored *StoredFilter) bool {
	resource, fields, err := e.filterTarget(stored.Resource)
	if err != nil || recovering(func() error { return resource.Check()(ctx) }) != nil {
		return false
	}
	_, err = visibleFilter(ctx, fields, stored.Document)
	return err == nil
}

// visibleFilter parses a filter document against fields and refuses one that names a field
// the identity in ctx may not read (D39), as a read would.
func visibleFilter(ctx context.Context, fields query.Fields, raw json.RawMessage) (*query.Document, error) {
	document, err := query.Parse(fields, raw)
	if err != nil {
		return nil, err
	}
	visible := fields.Visible(ctx)
	for _, name := range document.FieldNames() {
		if _, ok := visible.Lookup(name); !ok {
			return nil, ForbiddenField(name)
		}
	}
	return document, nil
}
