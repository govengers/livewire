package query_test

import (
	"fmt"

	"github.com/govengers/livewire/query"
)

func ExampleCompile() {
	fields := query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeString, SQL: "parts.name", Filter: true, Sort: true, Search: true},
		{Name: "createdAt", Type: query.TypeTime, SQL: "parts.created_at", Filter: true, Sort: true},
	}
	document, err := query.Parse(fields, []byte(`{"where":{"field":"createdAt","op":"gt","value":{"relative":"-7d"}},"search":"bolt"}`))
	if err != nil {
		fmt.Println(err)
		return
	}
	sort, err := query.ParseSort(fields, "-createdAt")
	if err != nil {
		fmt.Println(err)
		return
	}
	page, err := query.ParsePage("20", "", 500)
	if err != nil {
		fmt.Println(err)
		return
	}
	scope := func(builder *query.Builder) { builder.WriteString("parts.tenant_id = ").Argument("tenant-1") }

	listQuery, err := query.Compile(fields, document, sort, page, scope)
	if err != nil {
		fmt.Println(err)
		return
	}
	text, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "parts", IDColumn: "parts.id"})
	fmt.Println(text)
	fmt.Println(arguments)
	// Output:
	// SELECT parts.id, (parts.created_at) FROM parts WHERE (parts.tenant_id = $1) AND (((parts.created_at) > (now() + make_interval(secs => $2))) AND ((parts.name) ILIKE $3 ESCAPE '\')) ORDER BY (parts.created_at) DESC, parts.id ASC LIMIT $4 OFFSET $5
	// [tenant-1 -604800 %bolt% 20 0]
}

// ExampleParse reads a filter document of the todo example's fields: conditions combined
// with and, or and not, a relative time, and a search over the searchable fields.
func ExampleParse() {
	fields := query.Fields{
		{Name: "title", Type: query.TypeText, SQL: "todos.title", Filter: true, Sort: true, Search: true},
		{Name: "priority", Type: query.TypeEnum, SQL: "todos.priority", Enum: []string{"low", "normal", "high"}, Filter: true},
		{Name: "done", Type: query.TypeBoolean, SQL: "todos.done", Filter: true},
		{Name: "dueAt", Type: query.TypeTime, SQL: "todos.due_at", Filter: true, Nullable: true},
	}
	// snippet:begin filter-document
	document, err := query.Parse(fields, []byte(`{
		"where": {"and": [
			{"field": "done", "op": "eq", "value": false},
			{"or": [
				{"field": "priority", "op": "in", "value": ["high", "normal"]},
				{"field": "dueAt", "op": "lt", "value": {"relative": "+2d"}}
			]},
			{"not": {"field": "title", "op": "startsWith", "value": "draft"}}
		]},
		"search": "milk"
	}`))
	// snippet:end filter-document
	if err != nil {
		fmt.Println(err)
		return
	}
	canonical, err := query.Canonical(document)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(canonical), document.IsTimeDependent())
	// Output: {"search":"milk","where":{"and":[{"field":"done","op":"eq","value":false},{"or":[{"field":"priority","op":"in","value":["high","normal"]},{"field":"dueAt","op":"lt","value":{"relative":"172800s"}}]},{"not":{"field":"title","op":"startsWith","value":"draft"}}]}} true
}
