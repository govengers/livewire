// Package query is the filter model of filterable resources: declared fields, the filter
// document (and, or, not, per-type operators, search, relative times), its validation,
// canonical form and hash, and the compilation to SQL text with arguments. It depends on
// the standard library only.
package query
