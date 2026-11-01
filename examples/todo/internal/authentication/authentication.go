// Package authentication wires govengers/auth for the example: password login with sessions
// for the browser, API keys for the CLI and the Go client (D20), the Guard in front of the API,
// and the IdentitySource the engine re-validates every socket with (D6). Both ways carry the
// user's workspace in the identity, which the registry resolves as the tenant (D42).
//
// Users live in password.MemoryStorage and sessions in session.MemoryBackend to keep the
// example small: a service stores its users with a password.Storage on its own users table
// and its sessions in redisbackend before it runs more than one replica.
package authentication

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/apikey"
	"github.com/govengers/auth/password"
	"github.com/govengers/auth/session"

	"github.com/govengers/livewire/examples/todo/internal/actor"
	"github.com/govengers/livewire/examples/todo/internal/model"
	"github.com/govengers/livewire/examples/todo/internal/service"
)

// Authentication authenticates the API's requests and sockets.
type Authentication struct {
	provider *password.Provider
	guard    *session.Guard
	keys     *apikey.Authenticator
	accounts *service.AccountService
}

// Options configures Authentication. Storage, AccountService and the 32 bytes of
// EncryptionKey are required; InsecureCookie drops Secure for plain http in development;
// Observer receives every authentication outcome.
type Options struct {
	Storage        password.Storage
	AccountService *service.AccountService
	EncryptionKey  []byte
	InsecureCookie bool
	Observer       auth.Observer
}

// New wires the password provider, the session Guard and the API keys.
func New(options *Options) (*Authentication, error) {
	if options == nil || options.Storage == nil || options.AccountService == nil {
		return nil, errors.New("authentication: Options.Storage and AccountService are required")
	}
	authentication := &Authentication{accounts: options.AccountService}
	backend, err := session.NewMemoryBackend(nil)
	if err != nil {
		return nil, err
	}
	store, err := session.NewStore(&session.StoreOptions{Backend: backend, EncryptionKey: options.EncryptionKey, InsecureCookie: options.InsecureCookie})
	if err != nil {
		return nil, err
	}
	if authentication.provider, err = password.New(&password.Options{
		Sessions: store, Storage: options.Storage, OnLogin: authentication.joinWorkspace, Observer: options.Observer,
	}); err != nil {
		return nil, err
	}
	if authentication.guard, err = session.NewGuard(&session.GuardOptions{
		Store: store, Refreshers: []session.Refresher{authentication.provider}, Observer: options.Observer,
	}); err != nil {
		return nil, err
	}
	if authentication.keys, err = apikey.New(&apikey.Options{Resolver: authentication.resolveKey, Observer: options.Observer}); err != nil {
		return nil, err
	}
	return authentication, nil
}

// Routes are the password provider's routes: login, logout and the second factor.
func (a *Authentication) Routes() []auth.Route { return a.provider.Routes() }

// Middleware authenticates an API request by its API key or else by its session, and refuses
// it with 401 JSON without either.
func (a *Authentication) Middleware(next http.Handler) http.Handler {
	return a.keys.Middleware()(a.guard.RequireAPI()(next))
}

// Identify re-validates a socket by its upgrade request (livewire.IdentitySource): the API key
// it carried, or else its session. A revoked key or an ended session is gone (4401).
func (a *Authentication) Identify(ctx context.Context, request *http.Request) (*auth.Identity, error) {
	if key := presentedKey(request); key != "" {
		return a.resolveKey(ctx, key)
	}
	return a.guard.Identify(ctx, request)
}

// joinWorkspace is the login hook: the identity carries the user's workspace.
func (a *Authentication) joinWorkspace(ctx context.Context, identity *auth.Identity) (*auth.Identity, error) {
	workspace, err := a.accounts.Workspace(ctx, identity.UserID)
	if errors.Is(err, model.ErrNoMember) {
		return nil, auth.ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	return actor.WithWorkspace(identity, workspace), nil
}

// resolveKey is the identity of an API key: its user in the user's workspace.
func (a *Authentication) resolveKey(ctx context.Context, key string) (*auth.Identity, error) {
	member, err := a.accounts.MemberOfAPIKey(ctx, key)
	if errors.Is(err, model.ErrNotFound) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	identity := &auth.Identity{Provider: "apikey", Subject: member.UserID.String(), UserID: member.UserID, Username: member.Username, Active: true}
	return actor.WithWorkspace(identity, member.Workspace), nil
}

// presentedKey is the API key of a request, as apikey.Authenticator reads it: X-API-Key, else
// "Authorization: Bearer".
func presentedKey(request *http.Request) string {
	if key := strings.TrimSpace(request.Header.Get("X-API-Key")); key != "" {
		return key
	}
	if scheme, key, found := strings.Cut(request.Header.Get("Authorization"), " "); found && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(key)
	}
	return ""
}

// usersNamespace makes the id of a seeded user stable across restarts.
var usersNamespace = uuid.MustParse("6f1d7b3e-4f3a-4c4e-9a51-1d3f3c2b7a10")

// Seed is a user SeedUser creates: the name, the password and the workspace.
type Seed struct {
	Username  string
	Password  string
	Workspace string
}

// SeedUser creates a user with a password in a workspace unless the user exists (INT-007).
func SeedUser(ctx context.Context, storage password.Storage, accounts *service.AccountService, seed Seed) error {
	id := uuid.NewSHA1(usersNamespace, []byte(seed.Username))
	if err := accounts.Join(ctx, id, seed.Workspace); err != nil {
		return err
	}
	if _, err := storage.GetUserByID(ctx, id); err == nil {
		return nil
	}
	if err := (password.Policy{}).Validate(seed.Password); err != nil {
		return err
	}
	hash, err := password.Hash(seed.Password, password.DefaultParameters())
	if err != nil {
		return err
	}
	return storage.CreateUser(ctx, &password.User{ID: id, Username: &seed.Username, PasswordHash: hash})
}
