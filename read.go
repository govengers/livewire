package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/govengers/livewire/query"
)

// ReadKind says which read of a declaration a request names. On the socket it is the
// target's suffix after a dot (D46): "parts" is the main read, "parts.item" the item read,
// "parts.distinct" the distinct read, "parts.presence" the presences on one record.
type ReadKind string

// The read kinds.
const (
	ReadMain     ReadKind = ""         // a resource's or list's ListResponse, an object's item
	ReadItem     ReadKind = "item"     // a resource's item read, keyed by its ItemKey parameter
	ReadDistinct ReadKind = "distinct" // a resource's distinct read (D15), the `field` parameter names the field
	ReadPresence ReadKind = "presence" // the presences on the record named by the `key` parameter (D37)
)

// ParameterPresenceKey is the parameter of a presence read, join and leave that names the
// record (D37): the item key of a resource, any key within an object.
const ParameterPresenceKey = "key"

// PresenceTable is the table whose changes re-run presence subscriptions; the
// postgresadapter's presence trigger notifies on it (D37).
const PresenceTable = "livewire_presence"

// ReadRequest is one read as a transport hands it to Engine.Read: the declaration's name,
// the read kind and the request's parameters, path and query.
type ReadRequest struct {
	Target string
	Kind   ReadKind
	Source ParameterSource
}

// PresenceItem is one presence as a presence read answers it: the socket that joined, its
// display name and the state it sent.
type PresenceItem struct {
	ID      string          `json:"id"`
	Display string          `json:"display"`
	State   json.RawMessage `json:"state"`
}

// Read runs one read for the identity in ctx, the pipeline the socket's query, subscribe and
// re-runs run too (D23): the target's static Check (D8), the parameters (a resource's
// reserved ones, a stored filter resolved through the FilterStore, D13), the tenant (D42),
// then in a read-only engine transaction (D45) the scope, the loader and the field redaction
// (D39). A presence read first runs the record's own read under the caller (a resource's item
// read for the key, an object's read), so Scope and ownership decide who sees who is there.
// The payload is the REST body (a ListResponse for lists, D14); it is answered with 200. A
// refusal or failure is the error, answered through ResponseFor. Read reports an
// OutcomeRequest to the registry's Observer.
func (e *Engine) Read(ctx context.Context, request ReadRequest) (json.RawMessage, error) {
	unit := startUnit(OutcomeRequest, "", request.Target)
	resolved, result, err := e.read(ctx, request)
	e.observe(ctx, unit.readOutcome(ctx, resolved, err))
	if err != nil {
		return nil, err
	}
	return result.payload, nil
}

// readPlan is what a declaration and a read kind make of a read, decided in one place.
type readPlan struct {
	declaration Declaration
	kind        ReadKind
	resource    *Resource // the declaration when it is a resource
	// answersList: the read answers a ListResponse, diffed by id when subscribed.
	answersList bool
	// typedParameters: the declaration's own parameters are decoded (lists and objects).
	typedParameters bool
	// readParameters: a resource's reserved parameters are read (its list and distinct reads).
	readParameters bool
}

// planRead decides what the request reads; a name that is no declaration, or a kind the
// declaration does not have, is 404. Presence needs a PresenceStore and a record read: a
// resource with an item key, or an object.
func (e *Engine) planRead(request ReadRequest) (*readPlan, error) {
	declaration, ok := e.registry.Lookup(request.Target)
	if !ok {
		return nil, NotFound()
	}
	plan := &readPlan{declaration: declaration, kind: request.Kind}
	plan.resource, _ = declaration.(*Resource)
	switch declaration.Kind() {
	case KindResource:
		plan.answersList, plan.readParameters = true, true
	case KindList:
		plan.answersList, plan.typedParameters = true, true
	case KindObject:
		plan.typedParameters = true
	case KindMutation, KindUpload:
		return nil, NotFound()
	}
	readable := false
	switch request.Kind {
	case ReadMain:
		readable = true
	case ReadItem:
		readable = plan.resource != nil && plan.resource.Item() != nil
		plan.answersList, plan.readParameters = false, false
	case ReadDistinct:
		readable = plan.resource != nil && plan.resource.DistinctPath() != ""
		plan.answersList = false
	case ReadPresence:
		hasRecordRead := declaration.Kind() == KindObject || (plan.resource != nil && plan.resource.Item() != nil)
		readable = hasRecordRead && e.options.PresenceStore != nil
		plan.answersList, plan.readParameters = true, false
	}
	if !readable {
		return nil, NotFound()
	}
	return plan, nil
}

// resolvedRead is a read past its static gate: the plan, the decoded parameters and the
// tenant.
type resolvedRead struct {
	request        ReadRequest
	plan           *readPlan
	parameters     any             // a list's or object's decoded parameters
	readParameters *ReadParameters // a resource's reserved parameters, the stored filter resolved
	presenceKey    string
	tenant         string
}

// readResult is a read's answer: the REST body and, for a list, its items and total, which a
// subscription diffs.
type readResult struct {
	payload json.RawMessage
	items   json.RawMessage
	total   int
}

// read resolves and loads; resolved is nil when the resolution failed.
func (e *Engine) read(ctx context.Context, request ReadRequest) (*resolvedRead, *readResult, error) {
	resolved, err := e.resolve(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	result, err := e.load(ctx, resolved)
	return resolved, result, err
}

// resolve plans the read, runs its static check and decodes the parameters and the tenant
// (D8: before anything is resolved against the database), then a stored filter.
func (e *Engine) resolve(ctx context.Context, request ReadRequest) (*resolvedRead, error) {
	plan, err := e.planRead(request)
	if err != nil {
		return nil, err
	}
	if err := plan.declaration.Check()(ctx); err != nil {
		return nil, err
	}
	request.Source = sourceOrEmpty(request.Source)
	resolved := &resolvedRead{request: request, plan: plan}
	if err := resolved.decode(); err != nil {
		return nil, err
	}
	if resolved.tenant, err = e.registry.Tenant(ctx, resolved.parameters); err != nil {
		return nil, err
	}
	if err := e.resolveStoredFilter(WithTenant(ctx, resolved.tenant), resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func (r *resolvedRead) decode() error {
	source := r.request.Source
	var err error
	if r.plan.typedParameters {
		if r.parameters, err = r.plan.declaration.DecodeParameters(source); err != nil {
			return err
		}
	}
	if r.plan.readParameters {
		if r.readParameters, err = ReadParametersFrom(source); err != nil {
			return err
		}
	}
	if r.plan.kind == ReadPresence {
		r.presenceKey, err = presenceKeyFrom(source)
	}
	return err
}

// sourceOrEmpty is source, or no parameters for nil.
func sourceOrEmpty(source ParameterSource) ParameterSource {
	if source == nil {
		return MapSource{}
	}
	return source
}

// maximumPresenceKeyBytes bounds the key that names a presence record.
const maximumPresenceKeyBytes = 256

func presenceKeyFrom(source ParameterSource) (string, error) {
	values, ok := source.Query(ParameterPresenceKey)
	switch {
	case !ok || len(values) != 1 || values[0] == "":
		return "", invalidParameter(ParameterPresenceKey, "required, once")
	case len(values[0]) > maximumPresenceKeyBytes:
		return "", invalidParameter(ParameterPresenceKey, "at most 256 bytes")
	}
	return values[0], nil
}

// resolveStoredFilter replaces a filterId by the stored document of the caller (D13). A
// stored filter is the owner's only; without an identity, a FilterStore or a filter of this
// resource it is 404.
func (e *Engine) resolveStoredFilter(ctx context.Context, resolved *resolvedRead) error {
	parameters := resolved.readParameters
	if parameters == nil || parameters.FilterID == "" {
		return nil
	}
	if len(parameters.Filter) > 0 {
		return Invalid(map[string]string{ParameterFilter: "filter and filterId exclude each other"})
	}
	owner, ok := OwnerOf(ctx)
	if !ok || e.options.FilterStore == nil {
		return NotFound()
	}
	stored, err := e.options.FilterStore.Get(ctx, owner, parameters.FilterID)
	if err != nil {
		return fmt.Errorf("livewire: the stored filter %q of %s: %w", parameters.FilterID, resolved.request.Target, err)
	}
	if stored.Resource != resolved.request.Target {
		return NotFound()
	}
	parameters.Filter = stored.Document
	return nil
}

// load runs a resolved read in a read-only engine transaction (D45). A presence read runs
// its record read there, then lists the presences from the PresenceStore outside it (D50).
func (e *Engine) load(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
	ctx = WithTenant(ctx, resolved.tenant)
	result, err := e.inReadTransaction(ctx, resolved, loadDeclaration)
	if err != nil || resolved.plan.kind != ReadPresence {
		return result, err
	}
	return e.loadPresence(ctx, resolved)
}

// checkRecord runs the record read of a presence operation under the caller: a record the
// caller may not read is refused with the read's own error.
func (e *Engine) checkRecord(ctx context.Context, resolved *resolvedRead) error {
	_, err := e.inReadTransaction(WithTenant(ctx, resolved.tenant), resolved, loadDeclaration)
	return err
}

func (e *Engine) inReadTransaction(ctx context.Context, resolved *resolvedRead, run func(ctx context.Context, resolved *resolvedRead) (*readResult, error)) (*readResult, error) {
	transaction, err := e.options.Transactor.Begin(ctx, TransactionDetails{Tenant: resolved.tenant, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("livewire: begin the read transaction of %q: %w", resolved.request.Target, err)
	}
	var result *readResult
	err = recovering(func() error {
		var loadError error
		result, loadError = run(WithTransaction(ctx, transaction.Handle()), resolved)
		return loadError
	})
	if err != nil {
		return nil, errors.Join(err, transaction.Rollback(ctx))
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, fmt.Errorf("livewire: commit the read transaction of %q: %w", resolved.request.Target, err)
	}
	return result, nil
}

// loadDeclaration runs the loader the plan names; for a presence read that is the record's
// read, whose answer only gates the presences.
func loadDeclaration(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
	plan := resolved.plan
	switch declaration := plan.declaration.(type) {
	case *Resource:
		switch plan.kind {
		case ReadItem:
			return loadItem(ctx, declaration, resolved.request.Source)
		case ReadPresence:
			return loadItem(ctx, declaration, MapSource{declaration.Item().Parameter: resolved.presenceKey})
		case ReadDistinct:
			return loadDistinct(ctx, declaration, resolved.readParameters)
		case ReadMain:
		}
		return loadPage(ctx, declaration, resolved.readParameters)
	case *List:
		items, err := declaration.Load(ctx, resolved.parameters)
		if err != nil {
			return nil, err
		}
		count := reflect.ValueOf(items).Len() // the loaders of a List return a slice
		return pageResult(ctx, declaration.Fields(), items, query.Page{Limit: count}, count)
	case *Object:
		item, err := declaration.Load(ctx, resolved.parameters)
		if err != nil {
			return nil, err
		}
		return itemResult(ctx, declaration.Fields(), item)
	default:
		return nil, NotFound()
	}
}

func loadItem(ctx context.Context, resource *Resource, source ParameterSource) (*readResult, error) {
	listQuery, err := resource.ItemQuery(ctx, source)
	if err != nil {
		return nil, err
	}
	item, err := resource.LoadItem(ctx, listQuery)
	if err != nil {
		return nil, err
	}
	return itemResult(ctx, resource.Fields(), item)
}

func loadDistinct(ctx context.Context, resource *Resource, parameters *ReadParameters) (*readResult, error) {
	listQuery, field, err := resource.DistinctQuery(ctx, parameters)
	if err != nil {
		return nil, err
	}
	values, err := resource.LoadDistinct(ctx, field.Name, listQuery)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the distinct values: %w", err)
	}
	return &readResult{payload: payload}, nil
}

func loadPage(ctx context.Context, resource *Resource, parameters *ReadParameters) (*readResult, error) {
	listQuery, err := resource.ListQuery(ctx, parameters)
	if err != nil {
		return nil, err
	}
	items, total, err := resource.LoadList(ctx, listQuery)
	if err != nil {
		return nil, err
	}
	return pageResult(ctx, resource.Fields(), items, query.Page{Limit: listQuery.Limit, Offset: listQuery.Offset}, total)
}

// listBody is ListResponse with the items already marshaled and redacted.
type listBody struct {
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
	Total  int             `json:"total"`
	Items  json.RawMessage `json:"items"`
}

func pageResult(ctx context.Context, fields query.Fields, items any, page query.Page, total int) (*readResult, error) {
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the items: %w", err)
	}
	redacted, err := RedactItems(ctx, fields, raw)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(listBody{Limit: page.Limit, Offset: page.Offset, Total: total, Items: redacted})
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the list: %w", err)
	}
	return &readResult{payload: payload, items: redacted, total: total}, nil
}

func itemResult(ctx context.Context, fields query.Fields, item any) (*readResult, error) {
	raw, err := json.Marshal(item)
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the item: %w", err)
	}
	redacted, err := RedactItem(ctx, fields, raw)
	if err != nil {
		return nil, err
	}
	return &readResult{payload: redacted}, nil
}

// loadPresence answers the presences on one record of the declaration (D37).
func (e *Engine) loadPresence(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
	var presences []Presence
	err := recovering(func() error {
		var listError error
		presences, listError = e.options.PresenceStore.List(ctx, resolved.tenant, resolved.request.Target, resolved.presenceKey)
		return listError
	})
	if err != nil {
		return nil, fmt.Errorf("livewire: list the presences on %s %q: %w", resolved.request.Target, resolved.presenceKey, err)
	}
	items := make([]PresenceItem, 0, len(presences))
	for _, presence := range presences {
		state := presence.State
		if len(state) == 0 {
			state = json.RawMessage("null")
		}
		items = append(items, PresenceItem{ID: presence.Socket, Display: presence.Display, State: state})
	}
	return pageResult(ctx, nil, items, query.Page{Limit: len(items)}, len(items))
}

// parseTarget splits a socket target at its first dot: the declaration's name before it, the
// ReadKind after it (D46); an unknown kind plans as 404.
func parseTarget(target string) (string, ReadKind) {
	name, kind, _ := strings.Cut(target, ".")
	return name, ReadKind(kind)
}
