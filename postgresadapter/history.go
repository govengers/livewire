package postgresadapter

import (
	"context"
	"fmt"
	"time"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

// historyColumns are the columns of `<table>_history` a version is read from, after its
// version number, in the order HistoryPage scans them.
var historyColumns = []query.DerivedColumn{
	{Name: "operation", SQL: "operation"},
	{Name: "validFrom", SQL: "valid_from"},
	{Name: "validTo", SQL: "valid_to"},
	{Name: "actor", SQL: "coalesce(actor, '')"},
	{Name: "realActor", SQL: "coalesce(real_actor, '')"},
	{Name: "mutation", SQL: "coalesce(mutation, '')"},
	{Name: "requestId", SQL: "coalesce(request_id, '')"},
	{Name: "data", SQL: "data"},
}

// HistoryPage reads one page of an item's versions from table's `<table>_history`
// (HistorySQL) as a resource's History loader answers it (D34, D56): listQuery is the
// engine's history query (HistoryFields, scoped to the item's id), read with its page query
// and its capped count on one snapshot like ListPage, in the engine's transaction inside an
// engine unit of work. decode maps a version's stored row, the jsonb of the table's columns,
// to the item's JSON shape. The page has no cursor: a history pages by offset.
//
//	History: func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[part.Item]], error) {
//		return postgresadapter.HistoryPage(ctx, r.pool, "parts", listQuery, decodePartRow)
//	},
func HistoryPage[Item any](ctx context.Context, database DBTX, table string, listQuery *query.ListQuery, decode func(row []byte) (*Item, error)) (*livewire.Page[livewire.Version[Item]], error) {
	specification := &query.ResourceSQL{From: identifier(table + historySuffix), IDColumn: "version", Derived: historyColumns}
	page := &livewire.Page[livewire.Version[Item]]{Items: []livewire.Version[Item]{}}
	err := inSnapshot(ctx, database, func(database DBTX) error {
		var err error
		if page.Items, err = scanVersions(ctx, database, specification, listQuery, decode); err != nil {
			return err
		}
		page.Total, err = count(ctx, database, specification, listQuery)
		return err
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

func scanVersions[Item any](ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery, decode func(row []byte) (*Item, error)) ([]livewire.Version[Item], error) {
	text, arguments := listQuery.PageSQL(specification)
	rows, err := database.Query(ctx, text, arguments...)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: read the history: %w", err)
	}
	defer rows.Close()
	versions := []livewire.Version[Item]{}
	for rows.Next() {
		var version livewire.Version[Item]
		var operation string
		var validTo *time.Time
		var data []byte
		targets := []any{&version.ID, &operation, &version.ValidFrom, &validTo, &version.Actor, &version.RealActor, &version.Mutation, &version.RequestID, &data}
		targets = append(targets, pointersTo(make([]any, len(listQuery.Order)))...)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("postgresadapter: scan a version: %w", err)
		}
		version.Operation, version.ValidTo = livewire.ChangeOperation(operation), validTo
		if version.Data, err = decode(data); err != nil {
			return nil, fmt.Errorf("postgresadapter: decode version %d: %w", version.ID, err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresadapter: read the history: %w", err)
	}
	return versions, nil
}
