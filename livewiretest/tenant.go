package livewiretest

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

// Tenant is one tenant of TenantIsolation: a name for the failure messages and the function
// that makes a request an identity of the tenant's (an API key header, a session cookie).
type Tenant struct {
	Name         string
	Authenticate func(request *http.Request)
}

// TenantIsolationOptions configures TenantIsolation: the application and two tenants, each
// with rows of every filterable resource its identity reads.
type TenantIsolationOptions struct {
	Application Application
	Tenants     [2]Tenant
}

// maximumIsolationPages bounds the pages TenantIsolation reads of one list; a list that has
// more fails the gate rather than leaving rows unchecked.
const maximumIsolationPages = 1000

// TenantIsolation reads every filterable resource of the registry as both tenants over REST
// (D42, DAT-017) and fails the test when anything of one tenant reaches the other. It lists
// every row of each tenant, paging until a page comes back short (past a CountCap), and keys
// each row by the resource's item key (its "id" without an item read); a row without a key
// fails, as nothing could be checked about it. Then, for each tenant, it fails on:
//   - a listed row whose key is one of the other tenant's;
//   - an item read of one of its own rows that answers another row, and an item read of any
//     row of the other tenant that answers anything but 404 or 403;
//   - a version in the history of one of its own rows that belongs to another row, and a
//     history read of any row of the other tenant that answers anything but 404 or 403;
//   - an export that writes no key column, or a row whose key is not one of its own;
//   - a distinct read of a filterable field whose values or counts differ from its own rows'.
//
// A resource of which a tenant reads no row fails too: the gate needs rows of both tenants.
func TenantIsolation(tb testing.TB, options *TenantIsolationOptions) {
	tb.Helper()
	if options == nil {
		tb.Fatalf("livewiretest: TenantIsolation needs options")
		return
	}
	first, second := options.Tenants[0], options.Tenants[1]
	if first.Authenticate == nil || second.Authenticate == nil {
		tb.Fatalf("livewiretest: TenantIsolation needs two tenants, each with Authenticate")
		return
	}
	server := startServer(tb, "TenantIsolation", &options.Application, first.Authenticate)
	defer server.close()
	ctx := context.Background()
	for _, declaration := range options.Application.Engine.Registry().Declarations() {
		if resource, filterable := declaration.(*livewire.Resource); filterable {
			for _, err := range isolateResource(ctx, server, resource, options.Tenants) {
				tb.Errorf("livewiretest: TenantIsolation: %s: %v", resource.Name(), err)
			}
		}
	}
}

// tenantRows are the rows one tenant lists of a resource, by key.
type tenantRows struct {
	tenant Tenant
	server *testServer
	keys   []string
	rows   map[string]map[string]json.RawMessage
}

// isolateResource checks one resource as both tenants.
func isolateResource(ctx context.Context, server *testServer, resource *livewire.Resource, tenants [2]Tenant) []error {
	var listed [2]*tenantRows
	var failures []error
	for index, tenant := range tenants {
		rows, err := listTenantRows(ctx, server.as(tenant.Authenticate), resource, tenant)
		if err != nil {
			failures = append(failures, fmt.Errorf("as %s: %w", tenant.Name, err))
			continue
		}
		if len(rows.keys) == 0 {
			failures = append(failures, fmt.Errorf("%s reads no row: give each tenant fixture rows of every filterable resource", tenant.Name))
		}
		listed[index] = rows
	}
	if listed[0] == nil || listed[1] == nil {
		return failures
	}
	for reader, owner := range []int{1, 0} {
		check := isolation{resource: resource, reader: listed[reader], owner: listed[owner]}
		failures = append(failures, check.run(ctx)...)
	}
	return failures
}

// keyField is the member a resource's rows are keyed by: the item key's field, else "id".
func keyField(resource *livewire.Resource) string {
	if key := resource.Item(); key != nil {
		return key.Field
	}
	return "id"
}

// listTenantRows reads every row of the resource's list as the tenant.
func listTenantRows(ctx context.Context, server *testServer, resource *livewire.Resource, tenant Tenant) (*tenantRows, error) {
	items, err := listAll(ctx, server, resource.Path(), nil)
	if err != nil {
		return nil, err
	}
	rows := &tenantRows{tenant: tenant, server: server, rows: map[string]map[string]json.RawMessage{}}
	for _, item := range items {
		members, key, err := keyedRow(item, keyField(resource))
		if err != nil {
			return nil, fmt.Errorf("a listed row cannot be checked: %w", err)
		}
		rows.keys = append(rows.keys, key)
		rows.rows[key] = members
	}
	return rows, nil
}

// listAll reads every item of a list route, page by page with the page the server applied,
// until a page comes back short: the total may be capped (D14), so it does not end the walk.
func listAll(ctx context.Context, server *testServer, route string, parameters map[string]string) ([]json.RawMessage, error) {
	var items []json.RawMessage
	for range maximumIsolationPages {
		paged := map[string]string{livewire.ParameterOffset: strconv.Itoa(len(items))}
		for name, value := range parameters {
			paged[name] = value
		}
		body, err := server.read(ctx, resolvedRead{target: readTarget{route: route}, parameters: paged})
		if err != nil {
			return nil, err
		}
		var page listBody
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("%s answers no ListResponse: %w", route, err)
		}
		items = append(items, page.Items...)
		if len(page.Items) == 0 || len(page.Items) < page.Limit {
			return items, nil
		}
	}
	return nil, fmt.Errorf("%s has more than %d pages: the gate cannot check every row", route, maximumIsolationPages)
}

// keyedRow decodes a row and reads its key member as text.
func keyedRow(item json.RawMessage, field string) (map[string]json.RawMessage, string, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(item, &members); err != nil {
		return nil, "", fmt.Errorf("a row is no JSON object: %s", abbreviate(item))
	}
	key, err := keyText(members[field])
	if err != nil {
		return nil, "", fmt.Errorf("a row has no usable %q: %s", field, abbreviate(item))
	}
	return members, key, nil
}

// keyText is a key's value as a path parameter carries it.
func keyText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil && text != "" {
		return text, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil || number == "" {
		return "", errors.New("it is neither a string nor a number")
	}
	return number.String(), nil
}

// isolation checks what one tenant, the reader, reads, against the other's, the owner's, rows.
type isolation struct {
	resource *livewire.Resource
	reader   *tenantRows
	owner    *tenantRows
}

func (i isolation) run(ctx context.Context) []error {
	var failures []error
	for _, key := range i.reader.keys {
		if _, foreign := i.owner.rows[key]; foreign {
			failures = append(failures, fmt.Errorf("%s lists the row %s of %s", i.reader.tenant.Name, key, i.owner.tenant.Name))
		}
	}
	for _, route := range []string{i.resource.ItemPath(), i.resource.HistoryPath()} {
		if route != "" {
			failures = append(failures, i.ownReads(ctx, route)...)
			failures = append(failures, i.foreignReads(ctx, route)...)
		}
	}
	failures = append(failures, i.export(ctx)...)
	return append(failures, i.distinct(ctx)...)
}

// ownReads reads every row of the reader by key: an item read must answer that row, a history
// only versions of it.
func (i isolation) ownReads(ctx context.Context, route string) []error {
	parameter := i.resource.Item().Parameter
	field := keyField(i.resource)
	var failures []error
	for _, key := range i.reader.keys {
		parameters := map[string]string{parameter: key}
		var answered []json.RawMessage
		if route == i.resource.HistoryPath() {
			versions, err := listAll(ctx, i.reader.server, route, parameters)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s's history of %s: %w", i.reader.tenant.Name, key, err))
				continue
			}
			answered = versionData(versions)
		} else {
			body, err := i.reader.server.read(ctx, resolvedRead{target: readTarget{route: route}, parameters: parameters})
			if err != nil {
				failures = append(failures, fmt.Errorf("%s's item %s: %w", i.reader.tenant.Name, key, err))
				continue
			}
			answered = []json.RawMessage{body}
		}
		for _, row := range answered {
			_, answeredKey, err := keyedRow(row, field)
			switch {
			case err != nil:
				failures = append(failures, fmt.Errorf("%s of %s's row %s cannot be checked: %w", route, i.reader.tenant.Name, key, err))
			case answeredKey != key:
				failures = append(failures, fmt.Errorf("%s of %s's row %s answers the row %s%s", route, i.reader.tenant.Name, key, answeredKey, i.ownership(answeredKey)))
			default:
			}
		}
	}
	return failures
}

// versionData is the data of every version that has some (D34).
func versionData(versions []json.RawMessage) []json.RawMessage {
	var data []json.RawMessage
	for _, version := range versions {
		var decoded struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(version, &decoded) == nil && len(decoded.Data) > 0 && !bytes.Equal(decoded.Data, []byte("null")) {
			data = append(data, decoded.Data)
		}
	}
	return data
}

// ownership names the owner of a key in a failure message.
func (i isolation) ownership(key string) string {
	if _, foreign := i.owner.rows[key]; foreign {
		return " of " + i.owner.tenant.Name
	}
	return ""
}

// foreignReads reads every row of the owner by key as the reader: only 404 and 403 pass.
func (i isolation) foreignReads(ctx context.Context, route string) []error {
	parameter := i.resource.Item().Parameter
	var failures []error
	for _, key := range i.owner.keys {
		status, body, err := i.reader.server.get(ctx, i.reader.server.restURL(route, map[string]string{parameter: key}))
		switch {
		case err != nil:
			failures = append(failures, err)
		case status == http.StatusOK:
			failures = append(failures, fmt.Errorf("%s reads %s of %s's row %s", i.reader.tenant.Name, route, i.owner.tenant.Name, key))
		case status != http.StatusNotFound && status != http.StatusForbidden:
			failures = append(failures, fmt.Errorf("%s of %s's row %s answers %s %d, not 404 or 403: %s", route, i.owner.tenant.Name, key, i.reader.tenant.Name, status, abbreviate(body)))
		default:
		}
	}
	return failures
}

// export reads the reader's export: it must write the key column and only the reader's rows.
func (i isolation) export(ctx context.Context) []error {
	route := i.resource.ExportPath()
	if route == "" {
		return nil
	}
	status, body, err := i.reader.server.get(ctx, i.reader.server.restURL(route, nil))
	if err != nil {
		return []error{err}
	}
	if status != http.StatusOK {
		return []error{fmt.Errorf("%s's export answered %d %s", i.reader.tenant.Name, status, abbreviate(body))}
	}
	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		return []error{fmt.Errorf("%s's export is no CSV: %w", i.reader.tenant.Name, err)}
	}
	if len(records) == 0 {
		return []error{fmt.Errorf("%s's export has no header", i.reader.tenant.Name)}
	}
	field := keyField(i.resource)
	column := slices.Index(records[0], field)
	if column < 0 {
		return []error{fmt.Errorf("%s's export writes no %q column, so its rows cannot be checked: export the key", i.reader.tenant.Name, field)}
	}
	var failures []error
	for _, record := range records[1:] {
		if column >= len(record) {
			failures = append(failures, fmt.Errorf("%s's export has a record without its %q", i.reader.tenant.Name, field))
			continue
		}
		key := record[column]
		_, foreign := i.owner.rows[key]
		_, own := i.reader.rows[key]
		switch {
		case foreign:
			failures = append(failures, fmt.Errorf("%s's export writes the row %s of %s", i.reader.tenant.Name, key, i.owner.tenant.Name))
		case !own:
			failures = append(failures, fmt.Errorf("%s's export writes the row %s, which it does not list", i.reader.tenant.Name, key))
		default:
		}
	}
	return failures
}

// distinct reads the distinct values of every filterable scalar field as the reader: each
// value and its count must be the reader's own rows'.
func (i isolation) distinct(ctx context.Context) []error {
	route := i.resource.DistinctPath()
	if route == "" {
		return nil
	}
	var failures []error
	for _, field := range i.resource.Fields() {
		if !field.Filter || field.Type == query.TypeStringArray || field.Type == query.TypeJSON {
			continue
		}
		status, body, err := i.reader.server.get(ctx, i.reader.server.restURL(route, map[string]string{livewire.ParameterField: field.Name}))
		switch {
		case err != nil:
			failures = append(failures, err)
			continue
		case status == http.StatusForbidden:
			continue // the field is guarded from the reader (D39): it reads none of its values
		case status != http.StatusOK:
			failures = append(failures, fmt.Errorf("%s's distinct %s answered %d %s", i.reader.tenant.Name, field.Name, status, abbreviate(body)))
			continue
		default:
		}
		failures = append(failures, i.distinctValues(field.Name, body)...)
	}
	return failures
}

func (i isolation) distinctValues(field string, body []byte) []error {
	own := map[string]int{}
	for _, row := range i.reader.rows {
		value, present := row[field]
		if !present {
			return []error{fmt.Errorf("%s's rows carry no member %q, so its distinct read cannot be checked", i.reader.tenant.Name, field)}
		}
		own[comparableValue(value)]++
	}
	var values []struct {
		Value json.RawMessage `json:"value"`
		Count int             `json:"count"`
	}
	if err := json.Unmarshal(body, &values); err != nil {
		return []error{fmt.Errorf("%s's distinct %s is no list of values: %w", i.reader.tenant.Name, field, err)}
	}
	var failures []error
	for _, value := range values {
		if counted := own[comparableValue(value.Value)]; counted != value.Count {
			failures = append(failures, fmt.Errorf("%s's distinct %s counts %s %d times, its own rows %d times", i.reader.tenant.Name, field, value.Value, value.Count, counted))
		}
	}
	return failures
}

// comparableValue is a JSON scalar as distinct and the rows can both spell it: a time as an
// instant, a number by its value.
func comparableValue(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return string(raw)
	}
	switch typed := value.(type) {
	case string:
		if instant, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return "time:" + instant.UTC().Format(time.RFC3339Nano)
		}
		return "text:" + typed
	case json.Number:
		if number, err := typed.Float64(); err == nil {
			return "number:" + strconv.FormatFloat(number, 'g', -1, 64)
		}
		return "number:" + typed.String()
	default:
		return fmt.Sprint(typed)
	}
}
