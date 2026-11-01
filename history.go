package livewire

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/govengers/livewire/query"
)

// Version is one version of an item of a versioned resource as its history read answers it
// (D34), one row of postgresadapter.HistorySQL's `<table>_history`: ID is the version number
// (1, 2, ... per item), the key a live history is diffed by; Operation is what the write did;
// ValidFrom and ValidTo bound the time the version was current (ValidTo nil for the current
// one; a delete is a closed version of the last row); Actor, RealActor, Mutation and
// RequestID are what the engine's transaction recorded (D31; empty outside one); Data is the
// item as the version left it, in the item's JSON shape: the loader maps the stored row to
// it. Data passes the resource's column checks like every item (D39).
type Version[Item any] struct {
	ID        int             `json:"id"`
	Operation ChangeOperation `json:"operation"`
	ValidFrom time.Time       `json:"validFrom"`
	ValidTo   *time.Time      `json:"validTo"`
	Actor     string          `json:"actor"`
	RealActor string          `json:"realActor"`
	Mutation  string          `json:"mutation"`
	RequestID string          `json:"requestId"`
	Data      *Item           `json:"data"`
}

// HistoryFields are the fields a history read filters and sorts by (D34), named as Version
// marshals them and written over the columns of `<table>_history`: the loader's FROM is the
// history table alone. The default sort is by version, oldest first. A live item's history
// may hold delete versions: a partition move writes a delete and an insert (D56).
func HistoryFields() query.Fields {
	return query.Fields{
		{Name: "id", Type: query.TypeInteger, SQL: "version", Filter: true, Sort: true},
		{Name: "operation", Type: query.TypeEnum, SQL: "operation", Enum: []string{string(ChangeInsert), string(ChangeUpdate), string(ChangeDelete)}, Filter: true},
		{Name: "validFrom", Type: query.TypeTime, SQL: "valid_from", Filter: true, Sort: true},
		{Name: "validTo", Type: query.TypeTime, SQL: "valid_to", Nullable: true, Filter: true, Sort: true},
		{Name: "actor", Type: query.TypeString, SQL: "actor", Nullable: true, Filter: true},
		{Name: "realActor", Type: query.TypeString, SQL: "real_actor", Nullable: true, Filter: true},
		{Name: "mutation", Type: query.TypeString, SQL: "mutation", Nullable: true, Filter: true},
		{Name: "requestId", Type: query.TypeString, SQL: "request_id", Nullable: true, Filter: true},
	}
}

var historySort = []query.SortTerm{{Field: "id"}}

// HistoryPath is the route of the history read (`ItemPath/history`), or "" without a History
// loader.
func (r *Resource) HistoryPath() string {
	if r.history == nil {
		return ""
	}
	return r.ItemPath() + "/history"
}

// historyQuery is the loader's query of a history read (D34): the reserved filter, sort and
// page parameters over HistoryFields, the item key's value from source as the scope
// `id = key` over the history table and the resource's CountCap (D14). A history pages by
// offset; `after` is refused.
func (r *Resource) historyQuery(source ParameterSource, parameters *ReadParameters) (*query.ListQuery, error) {
	key, ok := source.Path(r.item.Parameter)
	if !ok || key == "" {
		return nil, invalidParameter(r.item.Parameter, "required")
	}
	if parameters.After != "" {
		return nil, invalidParameter(ParameterAfter, "a history does not page by cursor")
	}
	fields := HistoryFields()
	var document *query.Document
	if len(parameters.Filter) > 0 {
		var err error
		if document, err = query.Parse(fields, parameters.Filter); err != nil {
			return nil, err
		}
	}
	sort, err := query.ParseSort(fields, parameters.Sort)
	if err != nil {
		return nil, err
	}
	if len(sort) == 0 {
		sort = historySort
	}
	page, err := query.ParsePage(parameters.Limit, parameters.Offset, r.maximumLimit)
	if err != nil {
		return nil, err
	}
	scope := func(builder *query.Builder) { builder.WriteString("id = ").Argument(key) }
	listQuery, err := query.Compile(fields, document, sort, page, scope)
	if err != nil {
		return nil, err
	}
	listQuery.CountCap = r.countCap
	return listQuery, nil
}

// loadHistory answers a history read: the item read first, under the caller, so the scope
// and the loader's ownership decide who sees the history; then the versions, each one's data
// stripped of the fields the caller may not read (D39).
func loadHistory(ctx context.Context, resource *Resource, source ParameterSource, parameters *ReadParameters) (*readResult, error) {
	listQuery, err := resource.historyQuery(source, parameters)
	if err != nil {
		return nil, err
	}
	if _, err := loadItem(ctx, resource, source); err != nil {
		return nil, err
	}
	page, err := resource.history(ctx, listQuery)
	if err != nil {
		return nil, err
	}
	items, total, _ := page.parts()
	fields := resource.Fields()
	answer := listAnswer{items: items, limit: listQuery.Limit, offset: listQuery.Offset, total: total}
	answer.redact = func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		return redactVersions(ctx, fields, raw)
	}
	return pageResult(ctx, nil, answer)
}

// redactVersions strips the guarded fields the identity may not read from the data of every
// version of raw, a marshaled JSON array of versions; unchecked when nothing is guarded for
// the identity.
func redactVersions(ctx context.Context, fields query.Fields, raw json.RawMessage) (json.RawMessage, error) {
	if len(fields.Visible(ctx)) == len(fields) {
		return raw, nil
	}
	var versions []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &versions); err != nil {
		return nil, fmt.Errorf("livewire: the versions to redact: %w", err)
	}
	for _, version := range versions {
		data, ok := version["data"]
		if !ok || string(data) == "null" {
			continue
		}
		redacted, err := RedactItem(ctx, fields, data)
		if err != nil {
			return nil, err
		}
		version["data"] = redacted
	}
	redacted, err := json.Marshal(versions)
	if err != nil {
		return nil, fmt.Errorf("livewire: marshal the redacted versions: %w", err)
	}
	return redacted, nil
}
