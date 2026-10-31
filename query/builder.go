package query

import (
	"strconv"
	"strings"
)

// Builder writes one PostgreSQL statement: its text and the numbered arguments the
// placeholders $1, $2, ... bind. The zero value is ready to use. Identifiers and
// expressions are written as they are; values go through Argument.
type Builder struct {
	text      strings.Builder
	arguments []any
}

// WriteString appends text as it is.
func (b *Builder) WriteString(text string) *Builder {
	b.text.WriteString(text)
	return b
}

// Argument binds value and writes its placeholder. A nil value writes NULL and binds
// nothing.
func (b *Builder) Argument(value any) *Builder {
	if value == nil {
		return b.WriteString("NULL")
	}
	b.arguments = append(b.arguments, value)
	return b.WriteString("$" + strconv.Itoa(len(b.arguments)))
}

// Query returns the statement text and its arguments in placeholder order.
func (b *Builder) Query() (string, []any) {
	return b.text.String(), b.arguments
}

// Predicate writes one boolean SQL expression into a builder. A nil Predicate is no
// condition: it matches every row. And, Or and Not parenthesize every operand, so a
// predicate may be any expression, a bare `a OR b` included. A predicate is meant for a
// WHERE clause: it is three-valued (a NULL input yields NULL, which drops the row like
// FALSE) except under Not, which reads NULL as FALSE before negating.
type Predicate func(*Builder)

// And joins the non-nil predicates with AND, each operand in parentheses. One predicate
// alone is returned as it is; none returns nil.
func And(predicates ...Predicate) Predicate {
	return join(" AND ", predicates)
}

// Or joins the non-nil predicates with OR, each operand in parentheses. One predicate alone
// is returned as it is; none returns nil.
func Or(predicates ...Predicate) Predicate {
	return join(" OR ", predicates)
}

// Not negates the predicate two-valued: `NOT COALESCE((p), FALSE)`, so a row the predicate
// cannot decide (NULL) is in the negation. A nil predicate matches every row, so its
// negation matches none.
func Not(predicate Predicate) Predicate {
	if predicate == nil {
		return func(builder *Builder) { builder.WriteString("FALSE") }
	}
	return func(builder *Builder) {
		builder.WriteString("NOT COALESCE(")
		parenthesized(predicate)(builder)
		builder.WriteString(", FALSE)")
	}
}

func parenthesized(predicate Predicate) Predicate {
	return func(builder *Builder) {
		builder.WriteString("(")
		predicate(builder)
		builder.WriteString(")")
	}
}

func join(joiner string, predicates []Predicate) Predicate {
	live := make([]Predicate, 0, len(predicates))
	for _, predicate := range predicates {
		if predicate != nil {
			live = append(live, predicate)
		}
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	}
	return func(builder *Builder) {
		for index, predicate := range live {
			if index > 0 {
				builder.WriteString(joiner)
			}
			parenthesized(predicate)(builder)
		}
	}
}
