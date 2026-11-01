package livewire_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

type part struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Price     *float64   `json:"price"`
	Secret    string     `json:"secret"`
	CreatedAt time.Time  `json:"createdAt"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
	Version   int        `json:"version"`
	Notes     string     `json:"notes"`
	Tags      []string   `json:"tags"`
}

var partTypes = []string{"bolt", "nut"}

// partFields: secret and notes are guarded (admin only); notes is the guarded searchable
// field, tags the array field.
func partFields() query.Fields {
	return query.Fields{
		{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeText, SQL: "parts.name", Filter: true, Sort: true, Search: true},
		{Name: "type", Type: query.TypeEnum, SQL: "parts.type", Enum: slices.Clone(partTypes), Filter: true, Sort: true},
		{Name: "price", Type: query.TypeFloat, SQL: "parts.price", Filter: true, Sort: true},
		{Name: "secret", Type: query.TypeString, SQL: "parts.secret", Filter: true, Sort: true, Read: authorization.IsAdmin()},
		{Name: "createdAt", Type: query.TypeTime, SQL: "parts.created_at", Filter: true, Sort: true},
		{Name: "deletedAt", Type: query.TypeTime, SQL: "parts.deleted_at", Filter: true, Sort: true},
		{Name: "notes", Type: query.TypeText, SQL: "parts.notes", Filter: true, Search: true, Read: authorization.IsAdmin()},
		{Name: "tags", Type: query.TypeStringArray, SQL: "parts.tags", Filter: true},
	}
}

func partOptions() *livewire.ResourceOptions[part] {
	return &livewire.ResourceOptions[part]{
		Tables:      []string{"parts"},
		Read:        authorization.IsUser(),
		Fields:      partFields(),
		DefaultSort: []query.SortTerm{{Field: "createdAt", Descending: true}},
		Item:        &livewire.ItemKey{Parameter: "id", Field: "id"},
		List: func(context.Context, *query.ListQuery) (*livewire.Page[part], error) {
			return &livewire.Page[part]{}, nil
		},
		Get: func(context.Context, *query.ListQuery) (*part, error) {
			return &part{Name: "bolt"}, nil
		},
		Distinct: func(context.Context, string, *query.ListQuery) ([]livewire.DistinctValue, error) {
			return nil, nil
		},
	}
}

func newParts(t *testing.T, options *livewire.ResourceOptions[part]) *livewire.Resource {
	t.Helper()
	resource := livewire.NewResource("parts", "/parts", options)
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(resource))
	return resource
}

func userContext() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{Provider: "password", Subject: "user", Active: true})
}

func adminContext() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{Provider: "password", Subject: "admin", Active: true, Roles: []string{auth.RoleAdmin}})
}

func roleContext(roles ...string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{Provider: "password", Subject: "r", Active: true, Roles: roles})
}

func requireError(t *testing.T, err error, status int, code livewire.Code, field string) map[string]string {
	t.Helper()
	require.Error(t, err)
	gotStatus, body := livewire.ResponseFor(err)
	require.Equal(t, status, gotStatus, "status of %v", err)
	require.Equal(t, code, body.Code, "code of %v", err)
	if field != "" {
		require.Contains(t, body.Fields, field, "fields of %v", err)
	}
	return body.Fields
}

func addError(t *testing.T, declarations ...livewire.Declaration) error {
	t.Helper()
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	return registry.Add(declarations...)
}
