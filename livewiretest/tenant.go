package livewiretest

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/internal/listkey"
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

// isolationProbes bounds how many rows of the other tenant TenantIsolation reads by key.
const isolationProbes = 10

// maximumIsolationPages bounds the list pages TenantIsolation reads per resource and tenant.
const maximumIsolationPages = 100

// TenantIsolation reads every filterable resource of the registry as both tenants over REST
// (D42, DAT-017) and fails the test when the tenants' rows cross: a row (by its "id") that
// both lists contain, a row of the other tenant that an item read or a history read answers
// with 200, or an id of the other tenant in an export that writes the id column. A resource of
// which a tenant reads no row fails too: the gate needs rows of both tenants to prove anything.
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
		resource, filterable := declaration.(*livewire.Resource)
		if !filterable {
			continue
		}
		rows := [2][]json.RawMessage{}
		for index, tenant := range options.Tenants {
			listed, err := listAll(ctx, server.as(tenant.Authenticate), resource)
			if err != nil {
				tb.Errorf("livewiretest: TenantIsolation: %s as %s: %v", resource.Name(), tenant.Name, err)
				continue
			}
			if len(listed) == 0 {
				tb.Errorf("livewiretest: TenantIsolation: %s reads no row of %s: give each tenant fixture rows of every filterable resource", tenant.Name, resource.Name())
			}
			rows[index] = listed
		}
		for reader, owner := range []int{1, 0} {
			isolated := isolation{server: server.as(options.Tenants[reader].Authenticate), resource: resource, reader: options.Tenants[reader].Name, owner: options.Tenants[owner].Name}
			for _, err := range isolated.check(ctx, rows[reader], rows[owner]) {
				tb.Errorf("livewiretest: TenantIsolation: %s: %v", resource.Name(), err)
			}
		}
	}
}

// listAll reads every row of a resource's list, page by page with the page the server applied.
func listAll(ctx context.Context, server *testServer, resource *livewire.Resource) ([]json.RawMessage, error) {
	var rows []json.RawMessage
	for range maximumIsolationPages {
		body, err := server.read(ctx, resolvedRead{
			target:     readTarget{declaration: resource, route: resource.Path()},
			parameters: map[string]string{livewire.ParameterOffset: strconv.Itoa(len(rows))},
		})
		if err != nil {
			return nil, err
		}
		var page listBody
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("the list is no ListResponse: %w", err)
		}
		rows = append(rows, page.Items...)
		if len(page.Items) == 0 || len(rows) >= page.Total {
			return rows, nil
		}
	}
	return rows, nil
}

// isolation checks what one tenant, the reader, reads of the other's, the owner's, rows.
type isolation struct {
	server   *testServer
	resource *livewire.Resource
	reader   string
	owner    string
}

func (i isolation) check(ctx context.Context, readerRows, ownerRows []json.RawMessage) []error {
	ownerIDs := map[string]bool{}
	for _, row := range ownerRows {
		if id, err := listkey.ItemID(row); err == nil {
			ownerIDs[id] = true
		}
	}
	var failures []error
	for _, row := range readerRows {
		if id, err := listkey.ItemID(row); err == nil && ownerIDs[id] {
			failures = append(failures, fmt.Errorf("%s lists the row %s of %s", i.reader, id, i.owner))
		}
	}
	for _, row := range ownerRows[:min(len(ownerRows), isolationProbes)] {
		failures = append(failures, i.readByKey(ctx, row)...)
	}
	if err := i.export(ctx, ownerIDs); err != nil {
		failures = append(failures, err)
	}
	return failures
}

// readByKey reads one row of the owner by its item key, on the item and the history route.
func (i isolation) readByKey(ctx context.Context, row json.RawMessage) []error {
	key := i.resource.Item()
	if key == nil {
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(row, &members); err != nil {
		return []error{fmt.Errorf("a row is no object: %w", err)}
	}
	value, err := keyText(members[key.Field])
	if err != nil {
		return []error{fmt.Errorf("the item key %s of a row: %w", key.Field, err)}
	}
	var failures []error
	for _, route := range []string{i.resource.ItemPath(), i.resource.HistoryPath()} {
		if route == "" {
			continue
		}
		status, _, err := i.server.get(ctx, i.server.restURL(route, map[string]string{key.Parameter: value}))
		switch {
		case err != nil:
			failures = append(failures, err)
		case status == http.StatusOK:
			failures = append(failures, fmt.Errorf("%s reads %s of %s's row %s", i.reader, route, i.owner, value))
		default:
		}
	}
	return failures
}

// keyText is an item key's value as a path parameter carries it.
func keyText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return "", errors.New("it is neither a string nor a number")
	}
	return number.String(), nil
}

// export reads the reader's export and fails on an id of the owner when it writes the id column.
func (i isolation) export(ctx context.Context, ownerIDs map[string]bool) error {
	route := i.resource.ExportPath()
	if route == "" {
		return nil
	}
	status, body, err := i.server.get(ctx, i.server.restURL(route, nil))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s's export answered %d %s", i.reader, status, abbreviate(body))
	}
	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s's export is no CSV: %w", i.reader, err)
	}
	if len(records) == 0 {
		return nil
	}
	column := slices.Index(records[0], "id")
	if column < 0 {
		return nil
	}
	for _, record := range records[1:] {
		if column < len(record) && ownerIDs[record[column]] {
			return fmt.Errorf("%s's export writes the row %s of %s", i.reader, record[column], i.owner)
		}
	}
	return nil
}
