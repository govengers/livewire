package query

import (
	"fmt"
	"strings"
)

// Compile turns a parsed document, the sort terms, the page and the resource's scope into
// one ListQuery. A nil document is no filter, no sort terms means no order beyond the id,
// a nil scope matches every row. Conditions compile to plain comparisons the planner can
// serve from an index; a NULL input drops the row in a positive WHERE context, and `not`
// reads NULL as FALSE before negating, so `not` behaves classically: `ne` and `notIn`
// include the rows whose value is NULL, exactly like `not eq` and `not in`. A sort term
// must name a field in fields; a cursor must have been taken under the same sort. The
// caller sets CountCap.
func Compile(fields Fields, document *Document, sort []SortTerm, page Page, scope Predicate) (*ListQuery, error) {
	where, err := compileDocument(fields, document)
	if err != nil {
		return nil, err
	}
	order, err := compileOrder(fields, sort)
	if err != nil {
		return nil, err
	}
	if page.After != nil && !page.After.matches(sort) {
		return nil, newError(CodeInvalidCursor, "after", "was taken under sort %q, not %q", formatSort(page.After.Sort), formatSort(sort))
	}
	return &ListQuery{
		Where:  And(scope, where),
		Order:  order,
		Limit:  page.Limit,
		Offset: page.Offset,
		Cursor: page.After,
	}, nil
}

// compileDocument returns the filter of a document, nil when it narrows nothing.
func compileDocument(fields Fields, document *Document) (Predicate, error) {
	var where Predicate
	if document == nil {
		return where, nil
	}
	if document.where != nil {
		compiled, err := compileNode(fields, document.where)
		if err != nil {
			return nil, err
		}
		where = compiled
	}
	if document.search != "" {
		where = And(where, compileSearch(fields, document.search))
	}
	return where, nil
}

func compileOrder(fields Fields, sort []SortTerm) ([]OrderTerm, error) {
	order := make([]OrderTerm, 0, len(sort))
	for index, term := range sort {
		field, ok := fields.Lookup(term.Field)
		if !ok {
			return nil, newError(CodeInvalidSort, fmt.Sprintf("sort[%d]", index), "unknown field %q", term.Field)
		}
		order = append(order, OrderTerm{Field: field.Name, SQL: field.SQL, Descending: term.Descending, Nullable: field.Nullable})
	}
	return order, nil
}

func compileNode(fields Fields, current *node) (Predicate, error) {
	switch {
	case current.and != nil:
		return compileChildren(fields, current.and, And)
	case current.or != nil:
		return compileChildren(fields, current.or, Or)
	case current.not != nil:
		child, err := compileNode(fields, current.not)
		if err != nil {
			return nil, err
		}
		return Not(child), nil
	}
	field, ok := fields.Lookup(current.field)
	if !ok {
		return nil, filterError("where", "unknown field %q", current.field)
	}
	return compileCondition(field, current.operator, current.value), nil
}

func compileChildren(fields Fields, children []*node, combine func(...Predicate) Predicate) (Predicate, error) {
	predicates := make([]Predicate, 0, len(children))
	for _, child := range children {
		predicate, err := compileNode(fields, child)
		if err != nil {
			return nil, err
		}
		predicates = append(predicates, predicate)
	}
	return combine(predicates...), nil
}

// comparisons are the operators that compile to `expression <operator> value`.
var comparisons = map[Operator]string{
	OperatorEqual:          "=",
	OperatorGreater:        ">",
	OperatorGreaterOrEqual: ">=",
	OperatorLess:           "<",
	OperatorLessOrEqual:    "<=",
}

// likeEscaper makes a term literal inside a LIKE pattern with ESCAPE '\'.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func compileCondition(field Field, operator Operator, value any) Predicate {
	expression := "(" + field.SQL + ")"
	switch operator {
	case OperatorEqual, OperatorGreater, OperatorGreaterOrEqual, OperatorLess, OperatorLessOrEqual:
		return func(builder *Builder) {
			builder.WriteString(expression + " " + comparisons[operator] + " ")
			writeValue(builder, value)
		}
	case OperatorNotEqual:
		// IS DISTINCT FROM is two-valued on its own: NULL is distinct from every value.
		return func(builder *Builder) {
			builder.WriteString(expression + " IS DISTINCT FROM ")
			writeValue(builder, value)
		}
	case OperatorIn:
		return func(builder *Builder) { writeIn(builder, expression, value) }
	case OperatorNotIn:
		return Not(func(builder *Builder) { writeIn(builder, expression, value) })
	case OperatorContains:
		return compileLike(expression, "%"+likeEscaper.Replace(value.(string))+"%")
	case OperatorStartsWith:
		return compileLike(expression, likeEscaper.Replace(value.(string))+"%")
	case OperatorIsNull:
		return compileIsNull(expression, value.(bool))
	case OperatorHas:
		return func(builder *Builder) {
			builder.Argument(value).WriteString(" = ANY(" + expression + ")")
		}
	case OperatorHasAny:
		return func(builder *Builder) { writeArrayOverlap(builder, expression, " && ", value) }
	case OperatorHasAll:
		return func(builder *Builder) { writeArrayOverlap(builder, expression, " @> ", value) }
	case OperatorIsEmpty:
		return compileIsEmpty(expression, value.(bool))
	}
	return func(builder *Builder) { builder.WriteString("FALSE") }
}

func compileLike(expression, pattern string) Predicate {
	return func(builder *Builder) {
		builder.WriteString(expression + " ILIKE ").Argument(pattern).WriteString(` ESCAPE '\'`)
	}
}

func compileIsNull(expression string, isNull bool) Predicate {
	test := " IS NOT NULL"
	if isNull {
		test = " IS NULL"
	}
	return func(builder *Builder) { builder.WriteString(expression + test) }
}

// compileIsEmpty counts a NULL array as empty.
func compileIsEmpty(expression string, isEmpty bool) Predicate {
	comparison := " > 0"
	if isEmpty {
		comparison = " = 0"
	}
	return func(builder *Builder) {
		builder.WriteString("COALESCE(cardinality(" + expression + "), 0)" + comparison)
	}
}

func writeIn(builder *Builder, expression string, value any) {
	builder.WriteString(expression + " IN (")
	for index, item := range value.([]any) {
		if index > 0 {
			builder.WriteString(", ")
		}
		writeValue(builder, item)
	}
	builder.WriteString(")")
}

func writeArrayOverlap(builder *Builder, expression, operator string, value any) {
	builder.WriteString(expression + operator + "ARRAY[")
	for index, item := range value.([]any) {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.Argument(item)
	}
	builder.WriteString("]::text[]")
}

// writeValue binds one decoded value. A relative time becomes an expression over the
// transaction's now(), so the page and the count of one read see the same instant. An
// integer is cast to bigint, so a value outside an int4 or int2 column's range compares
// instead of failing to encode; a float is cast to float8, so a real column never makes the
// driver round the value to the parameter's narrower type.
func writeValue(builder *Builder, value any) {
	switch typed := value.(type) {
	case relative:
		builder.WriteString("(now() + make_interval(secs => ").Argument(typed.duration.Seconds()).WriteString("))")
	case int64:
		builder.Argument(typed).WriteString("::bigint")
	case float64:
		builder.Argument(typed).WriteString("::float8")
	default:
		builder.Argument(typed)
	}
}

// compileSearch matches when any searchable field contains the term, case-insensitively.
// Without a searchable field (every one guarded, D39) nothing matches.
func compileSearch(fields Fields, term string) Predicate {
	pattern := "%" + likeEscaper.Replace(term) + "%"
	var predicates []Predicate
	for _, field := range fields {
		if field.Search {
			predicates = append(predicates, compileLike("("+field.SQL+")", pattern))
		}
	}
	if len(predicates) == 0 {
		return func(builder *Builder) { builder.WriteString("FALSE") }
	}
	return Or(predicates...)
}
