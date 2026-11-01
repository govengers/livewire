package postgresadapter

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
// (at most CountCap + 1 under a cap) and, when the page is full, the cursor after its last
// row (nil otherwise). A resource in cursor mode answers Next.Encode() as ListResponse.Next;
// an offset list ignores it, so a sort value without a cursor form (a time after the year
// 9999) fails only the reads that need the cursor.
type Page[Item any] struct {
	Items []Item
	Total int
	Next  *query.Cursor
}

// ListPage reads one page of a filterable resource (section 8, D53): the page query, the
// capped count and the hydration of the page's rows, all on one snapshot. Inside an engine
// unit of work (TransactionFrom(ctx) is not nil) they run in the engine's transaction,
// whatever database is, so the read sees the engine's snapshot and livewire.tenant; outside
// one, on a pool or a connection, ListPage opens a REPEATABLE READ READ ONLY transaction of
// its own. A cursor value the column refuses (SQLSTATE 22P02, a forged enum value) is a
// *query.Error with query.CodeInvalidCursor; an id the hydration did not return is an error.
func ListPage[Item any](ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery, hydrate Hydrate[Item]) (*Page[Item], error) {
	var page *Page[Item]
	err := inSnapshot(ctx, database, func(database DBTX) error {
		rows, orderValues, err := scanPage(ctx, database, specification, listQuery)
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
		page = &Page[Item]{Items: items, Total: total}
		if len(rows) > 0 && len(rows) >= listQuery.Limit {
			page.Next = listQuery.NextCursor(orderValues, rows[len(rows)-1].ID.String())
		}
		return nil
	})
	return page, err
}

// PageIDs is the ids of rows in page order, the argument of a hydration query.
func PageIDs(rows []PageRow) []uuid.UUID {
	ids := make([]uuid.UUID, len(rows))
	for index, row := range rows {
		ids[index] = row.ID
	}
	return ids
}

// Distinct is the distinct read of D15: the values of field under the list's WHERE with
// their counts, most frequent first, at most listQuery.Limit of them (the resource's
// DistinctMaximum, which the engine puts there). It runs in the engine's transaction inside
// an engine unit of work, on database outside one. Field must be one of the resource's
// filterable scalar fields, else it is a *query.Error.
func Distinct(ctx context.Context, database DBTX, specification *query.ResourceSQL, listQuery *query.ListQuery, field query.Field) ([]livewire.DistinctValue, error) {
	text, arguments, err := listQuery.DistinctSQL(specification, field, listQuery.Limit)
	if err != nil {
		return nil, err
	}
	if transaction := TransactionFrom(ctx); transaction != nil {
		database = transaction
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
// offset grows; Limit and Offset of the query are ignored. A column whose Read check refuses
// the identity in ctx is left out, header included (D39). It runs on one snapshot like
// ListPage, in the engine's transaction inside an engine unit of work, and returns how many
// rows it wrote. Values are written as text: times in RFC 3339 UTC, uuids hyphenated, NULL as
// an empty field, arrays and json as JSON. It does not neutralize spreadsheet formulas in
// values.
func WriteCSV(ctx context.Context, database DBTX, export *Export, writer io.Writer) (int, error) {
	chunkSize := export.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultExportChunkSize
	}
	columns := export.Columns.Visible(ctx)
	specification, header := exportProjection(export.Specification, columns)
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
			if err := writeRecords(output, columns, rows); err != nil {
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

// beginner is what opens a transaction: a pool or a connection.
type beginner interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// inSnapshot runs work on the engine's transaction in ctx when there is one; else on
// database when it cannot open a transaction (a transaction itself), else in a REPEATABLE
// READ READ ONLY transaction of database.
func inSnapshot(ctx context.Context, database DBTX, work func(database DBTX) error) error {
	if transaction := TransactionFrom(ctx); transaction != nil {
		return work(transaction)
	}
	opener, opens := database.(beginner)
	if !opens {
		return work(database)
	}
	transaction, err := opener.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("postgresadapter: begin a read transaction: %w", err)
	}
	// Deferred, so a panicking hydration or writer gives the connection back too; after the
	// commit it is pgx.ErrTxClosed, and a read-only transaction's rollback error adds nothing
	// to the failure of work.
	defer func() { _ = transaction.Rollback(ctx) }()
	if err := work(transaction); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("postgresadapter: commit a read transaction: %w", err)
	}
	return nil
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
		orderValues = values[len(specification.Derived):]
		spellInfinities(orderValues)
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

// spellInfinities replaces, in place, every infinite timestamp of values by the string
// PostgreSQL spells it with, the form NextCursor takes.
func spellInfinities(values []any) {
	for index, value := range values {
		if modifier, infinite := value.(pgtype.InfinityModifier); infinite {
			values[index] = modifier.String()
		}
	}
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

// jsonValue is a driver value as the JSON body writes it: a uuid hyphenated, an infinite
// timestamp and a non-finite float as PostgreSQL spells them (JSON has no NaN or Infinity).
func jsonValue(value any) any {
	switch typed := value.(type) {
	case [16]byte:
		return uuid.UUID(typed).String()
	case pgtype.InfinityModifier:
		return typed.String()
	case float32:
		return nonFinite(float64(typed), value)
	case float64:
		return nonFinite(typed, value)
	default:
		return value
	}
}

// nonFinite is the PostgreSQL spelling of a NaN or infinite number, value otherwise.
func nonFinite(number float64, value any) any {
	switch {
	case math.IsNaN(number):
		return "NaN"
	case math.IsInf(number, 1):
		return "Infinity"
	case math.IsInf(number, -1):
		return "-Infinity"
	default:
		return value
	}
}

// exportProjection is the page specification of an export, one derived column per written
// column, and the header naming them.
func exportProjection(resource *query.ResourceSQL, columns query.Fields) (*query.ResourceSQL, []string) {
	specification := &query.ResourceSQL{From: resource.From, IDColumn: resource.IDColumn}
	header := make([]string, len(columns))
	for index, column := range columns {
		header[index] = column.Name
		specification.Derived = append(specification.Derived, query.DerivedColumn{Name: column.Name, SQL: column.SQL})
	}
	return specification, header
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
