// Package actor reads who a request acts as from its context (AUTH-006): the identity
// govengers/auth put there, and the workspace the login or the API key carried in it.
package actor

import (
	"context"

	"github.com/govengers/auth"

	"github.com/govengers/livewire/examples/todo/internal/model"
)

// workspaceKey is the key of the workspace in auth.Identity.Data.
const workspaceKey = "workspace"

// WithWorkspace returns a copy of identity that carries the workspace.
func WithWorkspace(identity *auth.Identity, workspace string) *auth.Identity {
	carrying := identity.Clone()
	if carrying.Data == nil {
		carrying.Data = map[string]any{}
	}
	carrying.Data[workspaceKey] = workspace
	return carrying
}

// Member is who ctx acts as; without an identity or a workspace it is auth.ErrUnauthenticated.
func Member(ctx context.Context) (model.Member, error) {
	identity := auth.IdentityFrom(ctx)
	if identity == nil {
		return model.Member{}, auth.ErrUnauthenticated
	}
	workspace, carried := identity.Data[workspaceKey].(string)
	if !carried || workspace == "" {
		return model.Member{}, auth.ErrUnauthenticated
	}
	return model.Member{UserID: identity.UserID, Username: identity.Username, Workspace: workspace}, nil
}

// Workspace is the workspace ctx acts in, the registry's tenant (D42).
func Workspace(ctx context.Context, _ any) (string, error) {
	member, err := Member(ctx)
	return member.Workspace, err
}
