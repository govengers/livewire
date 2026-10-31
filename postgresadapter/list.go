package postgresadapter

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

// PageRow is one row of the page query: the id and the value of every derived column of the
// ResourceSQL by its name, as pgx returns it.
type PageRow struct {
	ID      uuid.UUID
	Derived map[string]any
}

// Hydrate reads the items of a page's rows on database, the list's transaction, keyed by id;
// a sqlc query `WHERE id = ANY(@ids::uuid[])` serves it. ListPage puts them in page order.
type Hydrate[Item any] func(ctx context.Context, database DBTX, rows []PageRow) (map[uuid.UUID]Item, error)

// Page is one page of a list (D14, D38): the items in page order (never nil), the total
// (at most CountCap + 1 under a cap) and, when the page is full, the encoded cursor of its
// last row (nil otherwise), which a resource in cursor mode answers as ListResponse.Next.
type Page[Item any] struct {
	Items []Item
	Total int
	Next  *string
}

// beginner is what opens a transaction: a pool or a connection.
type beginner interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// ListPage reads one page of a filterable resource (section 8): the page query, the capped
// count and the hydration of the page's rows, all on one snapshot. On the engine's handle
// (TransactionFrom) they run in its REPEATABLE READ transaction; on a pool or a connection
// ListPage opens a REPEATABLE READ READ ONLY transaction of its own. A cursor value the
// column refuses (SQLSTATE 22P02, a forged enum value) is a *query.Error with
// query.CodeInvalidCursor; an id the hydration did not return is an error.
func ListPage[Item any](ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery, hydrate Hydrate[Item]) (*Page[Item], error) {
	var page *Page[Item]
	err := inSnapshot(ctx, database, func(database DBTX) error {
		rows, next, err := readPage(ctx, database, specification, listQuery)
		if err != nil {
			return err
		}
		total, err := count(ctx, database, specification, listQuery)
		if err != nil {
			return err
		}
		items, err := hydrateInOrder(ctx, database, rows, hydrate)
		if err != nil {
			return err
		}
		page = &Page[Item]{Items: items, Total: total, Next: next}
		return nil
	})
	return page, err
}

// inSnapshot runs work on database when it is a transaction already, else in a REPEATABLE
// READ READ ONLY transaction of database.
func inSnapshot(ctx context.Context, database DBTX, work func(database DBTX) error) error {
	opener, opens := database.(beginner)
	if !opens {
		return work(database)
	}
	transaction, err := opener.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("postgresadapter: begin a read transaction: %w", err)
	}
	if err := work(transaction); err != nil {
		return errors.Join(err, transaction.Rollback(ctx))
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("postgresadapter: commit a read transaction: %w", err)
	}
	return nil
}

// readPage runs the page query and returns its rows and, for a full page, the next cursor.
func readPage(ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery) ([]PageRow, *string, error) {
	rows, orderValues, err := scanPage(ctx, database, specification, listQuery)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 || len(rows) < listQuery.Limit {
		return rows, nil, nil
	}
	next, err := listQuery.NextCursor(orderValues, rows[len(rows)-1].ID.String()).Encode()
	if err != nil {
		return nil, nil, fmt.Errorf("postgresadapter: encode the next cursor: %w", err)
	}
	return rows, &next, nil
}

// scanPage runs the page query; orderValues are the last row's, an infinite timestamp as
// its string (NextCursor).
func scanPage(ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery) (rows []PageRow, orderValues []any, err error) {
	text, arguments := listQuery.PageSQL(specification)
	result, err := database.Query(ctx, text, arguments...)
	if err != nil {
		return nil, nil, pageError(listQuery, err)
	}
	defer result.Close()
	for result.Next() {
		row := PageRow{Derived: make(map[string]any, len(specification.Derived))}
		values := make([]any, len(specification.Derived)+len(listQuery.Order))
		targets := append([]any{&row.ID}, pointersTo(values)...)
		if err := result.Scan(targets...); err != nil {
			return nil, nil, fmt.Errorf("postgresadapter: scan the page: %w", err)
		}
		for index, derived := range specification.Derived {
			row.Derived[derived.Name] = values[index]
		}
		orderValues = cursorValues(values[len(specification.Derived):])
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, nil, pageError(listQuery, err)
	}
	return rows, orderValues, nil
}

func pointersTo(values []any) []any {
	pointers := make([]any, len(values))
	for index := range values {
		pointers[index] = &values[index]
	}
	return pointers
}

// cursorValues hands an infinite timestamp to NextCursor as the string PostgreSQL spells it.
func cursorValues(values []any) []any {
	for index, value := range values {
		if modifier, infinite := value.(pgtype.InfinityModifier); infinite {
			values[index] = modifier.String()
		}
	}
	return values
}

// pageError maps SQLSTATE 22P02 in cursor mode to invalid_cursor: the cursor carried a
// value the column's type refuses.
func pageError(listQuery *query.ListQuery, err error) error {
	var postgresError *pgconn.PgError
	if listQuery.Cursor != nil && errors.As(err, &postgresError) && postgresError.Code == invalidTextRepresentation {
		return &query.Error{
			Code: query.CodeInvalidCursor, Message: query.CodeInvalidCursor.Message(),
			Fields: map[string]string{"after": "carries a value the column does not accept"},
		}
	}
	return fmt.Errorf("postgresadapter: read the page: %w", err)
}

func count(ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery) (int, error) {
	text, arguments := listQuery.CountSQL(specification)
	var total int
	if err := database.QueryRow(ctx, text, arguments...).Scan(&total); err != nil {
		return 0, fmt.Errorf("postgresadapter: count the list: %w", err)
	}
	return total, nil
}

func hydrateInOrder[Item any](ctx context.Context, database DBTX, rows []PageRow, hydrate Hydrate[Item]) ([]Item, error) {
	items := make([]Item, 0, len(rows))
	if len(rows) == 0 {
		return items, nil
	}
	byID, err := hydrate(ctx, database, rows)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: hydrate the page: %w", err)
	}
	for _, row := range rows {
		item, found := byID[row.ID]
		if !found {
			return nil, fmt.Errorf("postgresadapter: the hydration returned no item for id %s", row.ID)
		}
		items = append(items, item)
	}
	return items, nil
}

// PageIDs is the ids of rows in page order, the argument of a hydration query.
func PageIDs(rows []PageRow) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for index, row := range rows {
		ids[index] = row.ID
	}
	return ids
}

// Distinct is the distinct read of D15 on database: the values of field under the list's
// WHERE with their counts, most frequent first, at most listQuery.Limit of them (the
// resource's DistinctMaximum, which the engine puts there). Field must be one of the
// resource's filterable scalar fields, else it is a *query.Error.
func Distinct(ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery, field query.Field) ([]livewire.DistinctValue, error) {
	text, arguments, err := listQuery.DistinctSQL(specification, field, listQuery.Limit)
	if err != nil {
		return nil, err
	}
	rows, err := database.Query(ctx, text, arguments...)
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: distinct values of %s: %w", field.Name, err)
	}
	values, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (livewire.DistinctValue, error) {
		var value livewire.DistinctValue
		err := row.Scan(&value.Value, &value.Count)
		value.Value = jsonValue(value.Value)
		return value, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgresadapter: distinct values of %s: %w", field.Name, err)
	}
	return values, nil
}

// jsonValue is a driver value as the JSON body writes it: a uuid hyphenated, an infinite
// timestamp as its spelling.
func jsonValue(value any) any {
	switch typed := value.(type) {
	case [16]byte:
		return uuid.UUID(typed).String()
	case pgtype.InfinityModifier:
		return typed.String()
	default:
		return value
	}
}

// Export is one CSV export of D35: the columns are the fields it writes, by Name in the
// header and by SQL per row; ChunkSize (default 1000) is how many rows one keyset query
// reads.
type Export struct {
	Specification *query.ResourceSQL
	Query         *query.ListQuery
	Columns       query.Fields
	ChunkSize     int
}

const defaultExportChunkSize = 1000

// WriteCSV streams every row under export.Query's WHERE, in its order, as CSV to writer: a
// header of the column names, then one record per row, read in chunks by keyset so no
// offset grows; Limit and Offset of the query are ignored. It runs on one snapshot like
// ListPage and returns how many rows it wrote. Values are written as text: times in RFC
// 3339 UTC, uuids hyphenated, NULL as an empty field, arrays and json as JSON. It does not
// neutralize spreadsheet formulas in values.
func WriteCSV(ctx context.Context, database DBTX, export *Export, writer io.Writer) (int, error) {
	chunkSize := export.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultExportChunkSize
	}
	specification := &query.ResourceSQL{From: export.Specification.From, IDColumn: export.Specification.IDColumn}
	header := make([]string, len(export.Columns))
	for index, column := range export.Columns {
		header[index] = column.Name
		specification.Derived = append(specification.Derived, query.DerivedColumn{Name: column.Name, SQL: column.SQL})
	}
	output := csv.NewWriter(writer)
	if err := output.Write(header); err != nil {
		return 0, fmt.Errorf("postgresadapter: write the export header: %w", err)
	}
	written := 0
	err := inSnapshot(ctx, database, func(database DBTX) error {
		chunk := *export.Query
		chunk.Limit, chunk.Offset, chunk.Cursor = chunkSize, 0, nil
		for {
			rows, orderValues, err := scanPage(ctx, database, specification, &chunk)
			if err != nil {
				return err
			}
			if err := writeRecords(output, export.Columns, rows); err != nil {
				return err
			}
			written += len(rows)
			if len(rows) < chunkSize {
				return nil
			}
			chunk.Cursor = chunk.NextCursor(orderValues, rows[len(rows)-1].ID.String())
		}
	})
	return written, err
}

// writeRecords writes one chunk's rows and flushes them to the writer.
func writeRecords(output *csv.Writer, columns query.Fields, rows []PageRow) error {
	for _, row := range rows {
		record := make([]string, len(columns))
		for index, column := range columns {
			record[index] = csvValue(row.Derived[column.Name])
		}
		if err := output.Write(record); err != nil {
			return fmt.Errorf("postgresadapter: write the export: %w", err)
		}
	}
	output.Flush()
	if err := output.Error(); err != nil {
		return fmt.Errorf("postgresadapter: write the export: %w", err)
	}
	return nil
}

func csvValue(value any) string {
	switch typed := jsonValue(value).(type) {
	case nil:
		return ""
	case string:
		return typed
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case bool:
		return strconv.FormatBool(typed)
	case int16, int32, int64, float32, float64:
		return fmt.Sprint(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}
