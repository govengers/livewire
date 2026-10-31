package query

import (
	"math"
	"strings"
)

// OrderTerm is one ORDER BY term: a field, its SQL expression and its direction. A
// Nullable term keeps its NULLs last on a descending sort as well.
type OrderTerm struct {
	Field      string
	SQL        string
	Descending bool
	Nullable   bool
}

// ListQuery is everything a list loader needs: one predicate (scope, filter and search), the
// order (the id tiebreaker is added by PageSQL), the page and the count cap. A nil Where
// matches every row.
type ListQuery struct {
	Where Predicate
	Order []OrderTerm
	Limit int
	// Offset is ignored when Cursor is set.
	Offset int
	// CountCap bounds the count: CountSQL reads at most CountCap + 1 rows, so a total above
	// CountCap means "more than CountCap" (D14). Zero counts every row.
	CountCap int
	// Cursor is the position after which PageSQL reads in cursor mode (D38).
	Cursor *Cursor
}

// ResourceSQL names where a resource's rows come from: the FROM clause (tables and joins),
// the id column and the derived columns the page query returns next to the id.
type ResourceSQL struct {
	From     string
	IDColumn string
	Derived  []DerivedColumn
}

// DerivedColumn is a value computed per row by the page query that the loader needs for
// hydration or the client for display, named by its API name.
type DerivedColumn struct {
	Name string
	SQL  string
}

// PageSQL is the page query: it selects the id column, the derived columns and then the
// value of every order term (so the loader can build the next cursor, NextCursor), under
// WHERE, ordered by the terms with the id column as the final tiebreaker, and bounded by
// LIMIT and OFFSET, or by the keyset after Cursor in cursor mode.
func (q *ListQuery) PageSQL(specification *ResourceSQL) (string, []any) {
	var builder Builder
	builder.WriteString("SELECT " + specification.IDColumn)
	for _, derived := range specification.Derived {
		builder.WriteString(", (" + derived.SQL + ")")
	}
	for _, term := range q.Order {
		builder.WriteString(", (" + term.SQL + ")")
	}
	builder.WriteString(" FROM " + specification.From)
	writeWhere(&builder, And(q.Where, q.keyset(specification.IDColumn)))
	builder.WriteString(" ORDER BY " + q.orderBy(specification.IDColumn))
	builder.WriteString(" LIMIT ").Argument(q.Limit)
	if q.Cursor == nil {
		builder.WriteString(" OFFSET ").Argument(q.Offset)
	}
	return builder.Query()
}

// CountSQL counts the rows under WHERE; the cursor does not narrow it. With a CountCap it
// reads at most CountCap + 1 rows, so the count over a large table stops early.
func (q *ListQuery) CountSQL(specification *ResourceSQL) (string, []any) {
	var builder Builder
	if q.CountCap > 0 {
		builder.WriteString("SELECT count(*) FROM (SELECT 1 FROM " + specification.From)
		writeWhere(&builder, q.Where)
		builder.WriteString(" LIMIT ").Argument(min(q.CountCap, math.MaxInt-1) + 1).WriteString(") AS capped")
		return builder.Query()
	}
	builder.WriteString("SELECT count(*) FROM " + specification.From)
	writeWhere(&builder, q.Where)
	return builder.Query()
}

// DistinctSQL is the distinct read of D15: the values of one filterable scalar field under
// WHERE with their row counts, most frequent first, at most maximum rows. A field that is
// not filterable, or an array, is refused with an *Error of CodeInvalidFilter at path
// "field"; the caller checks that the field is visible.
func (q *ListQuery) DistinctSQL(specification *ResourceSQL, field Field, maximum int) (string, []any, error) {
	if !field.Filter || field.Type == TypeStringArray {
		return "", nil, filterError("field", "distinct needs a filterable scalar field, %q is not one", field.Name)
	}
	var builder Builder
	builder.WriteString("SELECT (" + field.SQL + ") AS value, count(*) FROM " + specification.From)
	writeWhere(&builder, q.Where)
	builder.WriteString(" GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT ").Argument(maximum)
	text, arguments := builder.Query()
	return text, arguments, nil
}

// NextCursor builds the cursor of a row from the order values PageSQL selected after the
// id and derived columns (one per order term, nil for NULL) and the row's id. Values are
// what the driver returns (see Cursor.Encode); a timestamptz infinity has no time.Time, the
// loader hands it over as the string "infinity" or "-infinity".
func (q *ListQuery) NextCursor(orderValues []any, id string) *Cursor {
	sort := make([]SortTerm, len(q.Order))
	for index, term := range q.Order {
		sort[index] = SortTerm{Field: term.Field, Descending: term.Descending}
	}
	return &Cursor{Sort: sort, Values: orderValues, ID: id}
}

func writeWhere(builder *Builder, where Predicate) {
	if where != nil {
		builder.WriteString(" WHERE ")
		where(builder)
	}
}

func (q *ListQuery) orderBy(idColumn string) string {
	terms := make([]string, 0, len(q.Order)+1)
	for _, term := range q.Order {
		terms = append(terms, "("+term.SQL+")"+direction(term))
	}
	return strings.Join(append(terms, idColumn+" ASC"), ", ")
}

// direction keeps the NULLs of a nullable term last in both directions: PostgreSQL puts
// them first on DESC and last on ASC, so only DESC needs the clause.
func direction(term OrderTerm) string {
	if !term.Descending {
		return " ASC"
	}
	if term.Nullable {
		return " DESC NULLS LAST"
	}
	return " DESC"
}

// keyset is the condition "after the cursor's row" under the order with NULLs last: the
// rows that come after it in the first term, or tie it there and come after in the second,
// and so on down to the id. A NULL cursor value has no row after it in its term, so that
// branch is dropped. The NULL branches exist for Nullable terms only (Field.Nullable). A
// non-nullable leading term with a value gets the redundant bound `(term) >= v` in front,
// the range the planner reads from an index on the sort; the OR form alone is a filter.
func (q *ListQuery) keyset(idColumn string) Predicate {
	if q.Cursor == nil {
		return nil
	}
	var branches []Predicate
	for index, term := range q.Order {
		if q.Cursor.Values[index] == nil {
			continue
		}
		branches = append(branches, And(append(q.ties(index), after(term, q.Cursor.Values[index]))...))
	}
	lastRow := func(builder *Builder) { builder.WriteString(idColumn + " > ").Argument(q.Cursor.ID) }
	return And(q.leadingBound(), Or(append(branches, And(append(q.ties(len(q.Order)), lastRow)...))...))
}

func (q *ListQuery) leadingBound() Predicate {
	if len(q.Order) == 0 || q.Order[0].Nullable || q.Cursor.Values[0] == nil {
		return nil
	}
	term, value := q.Order[0], q.Cursor.Values[0]
	comparison := " >= "
	if term.Descending {
		comparison = " <= "
	}
	return func(builder *Builder) {
		builder.WriteString("(" + term.SQL + ")" + comparison)
		writeValue(builder, value)
	}
}

// ties are the equalities with the cursor on the order terms before index.
func (q *ListQuery) ties(index int) []Predicate {
	ties := make([]Predicate, 0, index)
	for position := range index {
		ties = append(ties, equals(q.Order[position], q.Cursor.Values[position]))
	}
	return ties
}

func equals(term OrderTerm, value any) Predicate {
	return func(builder *Builder) {
		if value == nil {
			builder.WriteString("(" + term.SQL + ") IS NULL")
			return
		}
		builder.WriteString("(" + term.SQL + ") = ")
		writeValue(builder, value)
	}
}

func after(term OrderTerm, value any) Predicate {
	comparison := " > "
	if term.Descending {
		comparison = " < "
	}
	beyond := func(builder *Builder) {
		builder.WriteString("(" + term.SQL + ")" + comparison)
		writeValue(builder, value)
	}
	if !term.Nullable {
		return beyond
	}
	return Or(beyond, func(builder *Builder) { builder.WriteString("(" + term.SQL + ") IS NULL") })
}
