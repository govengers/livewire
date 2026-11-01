// Package authentication wires govengers/auth for the example: password login with sessions
// for the browser, API keys for the CLI and the Go client (D20), the Guard in front of the API,
// and the IdentitySource the engine re-validates every socket with (D6). Either way the user's
// workspace, the registry's tenant (D42), is read from the user's current membership on every
// request and every socket re-validation (AUTH-007): a moved member's next request acts in the
// new workspace and a removed one's is refused. A socket learns of it when it re-validates, on
// its next op or its tick (60 s), or at once when the change re-validates the user's sockets
// (Engine.RevalidateOwner), as the application's moveMember and removeMember do.
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

// apiKeyProvider is the provider of an API key's identity, which carries its membership
// already: the key's lookup joins it.
const apiKeyProvider = "apikey"

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

// New wires the password provider, the session Guard and the API keys; it fails without the
// storage, the AccountService or a valid key.
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
	if authentication.provider, err = password.New(&password.Options{Sessions: store, Storage: options.Storage, Observer: options.Observer}); err != nil {
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

// Middleware authenticates an API request by its API key or else by its session and puts the
// user's current workspace into its identity; it refuses a request without either with 401
// and one whose user is in no workspace with 403, as JSON.
func (a *Authentication) Middleware(next http.Handler) http.Handler {
	member := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identity, err := a.member(request.Context(), auth.IdentityFrom(request.Context()))
		if err != nil {
			auth.WriteError(writer, err)
			return
		}
		next.ServeHTTP(writer, request.WithContext(auth.WithIdentity(request.Context(), identity)))
	})
	return a.keys.Middleware()(a.guard.RequireAPI()(member))
}

// Identify re-validates a socket by its upgrade request (livewire.IdentitySource): the API key
// it carried, or else its session, with the user's current workspace. A revoked key, an ended
// session and a user in no workspace are gone (4401); a moved member is another identity, the
// workspace being part of it, whose subscriptions of the old tenant end (D7, D42).
func (a *Authentication) Identify(ctx context.Context, request *http.Request) (*auth.Identity, error) {
	if key := presentedKey(request); key != "" {
		return a.resolveKey(ctx, key)
	}
	identity, err := a.guard.Identify(ctx, request)
	if err != nil {
		return nil, err
	}
	return a.member(ctx, identity)
}

// member is the identity with the user's current workspace; an API key's carries it already.
func (a *Authentication) member(ctx context.Context, identity *auth.Identity) (*auth.Identity, error) {
	if identity.Provider == apiKeyProvider {
		return identity, nil
	}
	workspace, err := a.accounts.Workspace(ctx, identity.UserID)
	if errors.Is(err, model.ErrNoMember) {
		return nil, auth.ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	return actor.WithWorkspace(identity, workspace), nil
}

// resolveKey is the identity of an API key: its user in the user's current workspace.
func (a *Authentication) resolveKey(ctx context.Context, key string) (*auth.Identity, error) {
	member, err := a.accounts.MemberOfAPIKey(ctx, key)
	if errors.Is(err, model.ErrNotFound) {
		return nil, auth.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	identity := &auth.Identity{Provider: apiKeyProvider, Subject: member.UserID.String(), UserID: member.UserID, Username: member.Username, Active: true}
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

// SeededUserID is the id SeedUser gives the user of that name.
func SeededUserID(username string) uuid.UUID { return uuid.NewSHA1(usersNamespace, []byte(username)) }

// Seed is a user SeedUser creates: the name, the password and the workspace.
type Seed struct {
	Username  string
	Password  string
	Workspace string
}

// SeedUser creates a user that is missing, and its membership when it has none, so a restart
// never moves a member back (INT-007). It answers the user's id.
func SeedUser(ctx context.Context, storage password.Storage, accounts *service.AccountService, seed Seed) (uuid.UUID, error) {
	id := SeededUserID(seed.Username)
	if err := accounts.JoinUnlessMember(ctx, id, seed.Workspace); err != nil {
		return uuid.Nil, err
	}
	if _, err := storage.GetUserByID(ctx, id); err == nil {
		return id, nil
	}
	if err := (password.Policy{}).Validate(seed.Password); err != nil {
		return uuid.Nil, err
	}
	hash, err := password.Hash(seed.Password, password.DefaultParameters())
	if err != nil {
		return uuid.Nil, err
	}
	return id, storage.CreateUser(ctx, &password.User{ID: id, Username: &seed.Username, PasswordHash: hash})
}
