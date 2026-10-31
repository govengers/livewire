package livewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/query"
)

// ReadKind says which read of a declaration a request names. On the socket it is the
// target's suffix after a dot: "parts" is the main read, "parts.item" the item read,
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
// record (D37).
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
// (D39). The payload is the REST body (a ListResponse for lists, D14); it is answered with
// 200. A refusal or failure is the error, answered through ResponseFor. Read reports an
// OutcomeRequest to the registry's Observer.
func (e *Engine) Read(ctx context.Context, request ReadRequest) (json.RawMessage, error) {
	unit := startUnit(OutcomeRequest, "", request.Target)
	resolved, result, err := e.read(ctx, request)
	e.registry.Observe(ctx, unit.readOutcome(ctx, resolved, err))
	if err != nil {
		return nil, err
	}
	return result.payload, nil
}

// resolvedRead is a read past its static gate: the declaration, the decoded parameters and
// the tenant.
type resolvedRead struct {
	request        ReadRequest
	declaration    Declaration
	parameters     any             // a list's or object's decoded parameters
	readParameters *ReadParameters // a resource's reserved parameters, the stored filter resolved
	presenceKey    string
	tenant         string
}

// readResult is a read's answer: the REST body and, for a list, its items and total, which a
// subscription diffs.
type readResult struct {
	payload json.RawMessage
	list    bool
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

// resolve finds the declaration, runs its static check and decodes the parameters and the
// tenant (D8: before anything is resolved against the database).
func (e *Engine) resolve(ctx context.Context, request ReadRequest) (*resolvedRead, error) {
	declaration, err := e.readDeclaration(request)
	if err != nil {
		return nil, err
	}
	if err := declaration.Check()(ctx); err != nil {
		return nil, err
	}
	resolved := &resolvedRead{request: request, declaration: declaration}
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

// readDeclaration is the declaration a read names; anything else is 404.
func (e *Engine) readDeclaration(request ReadRequest) (Declaration, error) {
	declaration, ok := e.registry.Lookup(request.Target)
	if !ok {
		return nil, NotFound()
	}
	resource, isResource := declaration.(*Resource)
	readable := false
	switch request.Kind {
	case ReadMain:
		readable = isReadKind(declaration)
	case ReadItem:
		readable = isResource && resource.Item() != nil
	case ReadDistinct:
		readable = isResource && resource.DistinctPath() != ""
	case ReadPresence:
		readable = isReadKind(declaration) && e.options.PresenceStore != nil
	}
	if !readable {
		return nil, NotFound()
	}
	return declaration, nil
}

func isReadKind(declaration Declaration) bool {
	switch declaration.Kind() {
	case KindResource, KindList, KindObject:
		return true
	default:
		return false
	}
}

func (r *resolvedRead) decode() error {
	r.request.Source = sourceOrEmpty(r.request.Source)
	source := r.request.Source
	var err error
	switch {
	case r.request.Kind == ReadPresence:
		r.presenceKey, err = presenceKeyFrom(source)
	case r.declaration.Kind() == KindResource:
		r.readParameters, err = ReadParametersFrom(source)
	default:
		r.parameters, err = r.declaration.DecodeParameters(source)
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
		return err
	}
	if stored.Resource != resolved.declaration.Name() {
		return NotFound()
	}
	parameters.Filter = stored.Document
	return nil
}

// load runs the loader of a resolved read in a read-only engine transaction (D45).
func (e *Engine) load(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
	ctx = WithTenant(ctx, resolved.tenant)
	if resolved.request.Kind == ReadPresence {
		return e.loadPresence(ctx, resolved) // the presence store keeps its own rows, outside the app's transaction
	}
	transaction, err := e.options.Transactor.Begin(ctx, TransactionDetails{Tenant: resolved.tenant, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("livewire: begin the read transaction of %q: %w", resolved.request.Target, err)
	}
	var result *readResult
	err = recovering(func() error {
		var loadError error
		result, loadError = loadDeclaration(WithTransaction(ctx, transaction.Handle()), resolved)
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

func loadDeclaration(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
	switch declaration := resolved.declaration.(type) {
	case *Resource:
		return loadResource(ctx, declaration, resolved)
	case *List:
		items, err := declaration.Load(ctx, resolved.parameters)
		if err != nil {
			return nil, err
		}
		return listResult(ctx, declaration.Fields(), items)
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

func loadResource(ctx context.Context, resource *Resource, resolved *resolvedRead) (*readResult, error) {
	switch resolved.request.Kind {
	case ReadItem:
		listQuery, err := resource.ItemQuery(ctx, resolved.request.Source)
		if err != nil {
			return nil, err
		}
		item, err := resource.LoadItem(ctx, listQuery)
		if err != nil {
			return nil, err
		}
		return itemResult(ctx, resource.Fields(), item)
	case ReadDistinct:
		listQuery, field, err := resource.DistinctQuery(ctx, resolved.readParameters)
		if err != nil {
			return nil, err
		}
		values, err := resource.LoadDistinct(ctx, field.Name, listQuery)
		if err != nil {
			return nil, err
		}
		return marshaledResult(values)
	default:
		listQuery, err := resource.ListQuery(ctx, resolved.readParameters)
		if err != nil {
			return nil, err
		}
		items, total, err := resource.LoadList(ctx, listQuery)
		if err != nil {
			return nil, err
		}
		return pageResult(ctx, resource.Fields(), items, query.Page{Limit: listQuery.Limit, Offset: listQuery.Offset}, total)
	}
}

// listBody is ListResponse with the items already marshaled and redacted.
type listBody struct {
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
	Total  int             `json:"total"`
	Items  json.RawMessage `json:"items"`
}

// listResult answers every item a typed-parameters list loaded: one page of all of them.
func listResult(ctx context.Context, fields query.Fields, items any) (*readResult, error) {
	count := reflect.ValueOf(items).Len() // the loaders of a List return a slice
	return pageResult(ctx, fields, items, query.Page{Limit: count}, count)
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
	return &readResult{payload: payload, list: true, items: redacted, total: total}, nil
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

func marshaledResult(value any) (*readResult, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the answer: %w", err)
	}
	return &readResult{payload: payload}, nil
}

// loadPresence answers the presences on one record of the declaration (D37).
func (e *Engine) loadPresence(ctx context.Context, resolved *resolvedRead) (*readResult, error) {
	presences, err := e.options.PresenceStore.List(ctx, resolved.tenant, resolved.declaration.Name(), resolved.presenceKey)
	if err != nil {
		return nil, fmt.Errorf("livewire: list the presences on %s %q: %w", resolved.declaration.Name(), resolved.presenceKey, err)
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

// unitOfWork is one unit of work the engine reports to the Observer (D4, D30).
type unitOfWork struct {
	kind         OutcomeKind
	operation    Operation
	target       string
	subscription string
	started      time.Time
}

func startUnit(kind OutcomeKind, operation Operation, target string) unitOfWork {
	return unitOfWork{kind: kind, operation: operation, target: target, started: time.Now()}
}

// outcome reports the unit with the status the error answers, 200 without one.
func (u unitOfWork) outcome(ctx context.Context, err error) Outcome {
	outcome := Outcome{
		Kind: u.kind, Operation: u.operation, Target: u.target, Subscription: u.subscription,
		Identity: auth.IdentityFrom(ctx), Tenant: TenantFrom(ctx), Status: http.StatusOK, Err: err, Duration: time.Since(u.started),
	}
	if err != nil {
		outcome.Status, _ = ResponseFor(err)
	}
	return outcome
}

// readOutcome is outcome with what the read resolved; resolved is nil when it failed before.
func (u unitOfWork) readOutcome(ctx context.Context, resolved *resolvedRead, err error) Outcome {
	outcome := u.outcome(ctx, err)
	if resolved != nil {
		outcome.Tenant, outcome.Parameters = resolved.tenant, resolved.parameters
	}
	return outcome
}

// parseTarget splits a socket target into the declaration's name and the read kind.
func parseTarget(target string) (string, ReadKind) {
	name, kind, _ := strings.Cut(target, ".")
	return name, ReadKind(kind)
}
