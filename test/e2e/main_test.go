//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	authginadapter "github.com/govengers/auth/ginadapter"
	"github.com/govengers/auth/password"
	"github.com/govengers/auth/session"
	"github.com/govengers/postgrestest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
	"github.com/govengers/livewire/ginadapter"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

// The suite runs two engines, each with its own pool, Listener and gin server, on one
// embedded PostgreSQL, as two replicas of one application (D24): the notes of each user,
// a versioned resource with history, export and presence, created, renamed and deleted by
// mutations with a client id and optimistic effects.

const schemaSQL = `CREATE TABLE notes (
	id uuid PRIMARY KEY,
	owner text NOT NULL,
	title text NOT NULL,
	version integer NOT NULL DEFAULT 1
)`

// The users and their API keys; a key the suite revokes is gone (4401).
var keys = map[string]*auth.Identity{
	"alice-key": {Provider: "apikey", Subject: "alice", Name: "Alice", Active: true},
	"bob-key":   {Provider: "apikey", Subject: "bob", Name: "Bob", Active: true},
}

// unnamedFiltersPerOwner is the FilterStore's cap, small so a test evicts (D54).
const unnamedFiltersPerOwner = 2

// postgres is the suite's one PostgreSQL (govengers/postgrestest): every cluster gets a database
// of its own, copied from the template the schema and livewire's SQL (D22) migrated.
var postgres *postgrestest.Server

func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	history, err := postgresadapter.HistorySQL("notes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: the template's SQL: %v\n", err)
		os.Exit(1)
	}
	statements := []string{
		schemaSQL, postgresadapter.TriggerFunctionSQL, postgresadapter.TriggerSQL("notes"), history,
		postgresadapter.FiltersSQL, postgresadapter.IdempotencySQL, postgresadapter.AuditSQL, postgresadapter.PresenceSQL,
	}
	postgres = postgrestest.New(&postgrestest.Options{
		Migrate:     postgrestest.Statements(statements...),
		Fingerprint: strings.Join(statements, "\n"),
	})
	goleak.VerifyTestMain(postgres.Main(m))
}

// withParameters is a database URL with the query parameters added (key=value pairs joined by &).
func withParameters(address, parameters string) string {
	if strings.Contains(address, "?") {
		return address + "&" + parameters
	}
	return address + "?" + parameters
}

// cluster is one database with two engines on it, each behind a server of its own.
type cluster struct {
	pool   *pgxpool.Pool // the test's own connection, for assertions
	first  *replica      // engine A
	second *replica      // engine B
}

// replica is one engine, its pool, its Listener and its server.
type replica struct {
	engine *livewire.Engine
	server *httptest.Server
	filter *postgresadapter.FilterStore
}

// baseURL is the replica's API root with the registry's BasePath.
func (r *replica) baseURL() string { return r.server.URL + "/api" }

// newCluster starts two replicas on a copy of the template of the test's own.
func newCluster(t *testing.T) *cluster {
	t.Helper()
	address := postgres.Database(t)
	pool, err := pgxpool.New(t.Context(), address)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	logins := newLogins(t)
	started := &cluster{pool: pool, first: newReplica(t, address, logins), second: newReplica(t, address, logins)}
	require.Eventually(t, func() bool {
		var listening int
		err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'livewire_feed' AND state = 'idle' AND query LIKE 'LISTEN%'`).Scan(&listening)
		return err == nil && listening == 2
	}, 10*time.Second, 10*time.Millisecond, "both engines' feeds listen")
	return started
}

// newReplica starts one engine on the database at address with the postgresadapter's Listener,
// Transactor and stores, and serves it on gin.
func newReplica(t *testing.T, address string, logins *logins) *replica {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), address)
	require.NoError(t, err)
	registry := newRegistry(t, pool)
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: withParameters(address, "application_name=livewire_feed"), RetryInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	transactor, err := postgresadapter.NewTransactor(&postgresadapter.TransactorOptions{Pool: pool})
	require.NoError(t, err)
	filters, err := postgresadapter.NewFilterStore(&postgresadapter.FilterStoreOptions{Pool: pool, MaximumUnnamedPerOwner: unnamedFiltersPerOwner})
	require.NoError(t, err)
	idempotency, err := postgresadapter.NewIdempotencyStore(&postgresadapter.IdempotencyStoreOptions{Pool: pool})
	require.NoError(t, err)
	audit, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: pool})
	require.NoError(t, err)
	presences, err := postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: pool})
	require.NoError(t, err)
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, ChangeFeed: listener, IdentitySource: logins, Transactor: transactor,
		FilterStore: filters, IdempotencyStore: idempotency, AuditStore: audit, PresenceStore: presences,
		SameHostOnly: true, DeliveryBatchWait: 10 * time.Millisecond, RetryInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	router := gin.New()
	authginadapter.RegisterRoutes(router, logins.provider.Routes())
	api := router.Group("", authginadapter.Middleware(logins.middleware))
	require.NoError(t, ginadapter.RegisterRoutes(api, &ginadapter.Options{Engine: engine}))
	router.GET("/api/socket", ginadapter.SocketHandler(engine)) // outside the 401: Identify names the socket (D97)
	server := httptest.NewServer(router)
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan error, 1)
	go func() { running <- engine.Run(ctx) }()
	t.Cleanup(func() {
		closing, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		require.NoError(t, engine.Close(closing))
		server.Close()
		cancel()
		require.NoError(t, <-running)
		pool.Close()
	})
	return &replica{engine: engine, server: server, filter: filters}
}

// The password users of every cluster; their password hash is computed once.
var (
	users        = map[string]uuid.UUID{"alice": uuid.MustParse("0198c0de-0000-7000-8000-00000000a11c"), "bob": uuid.MustParse("0198c0de-0000-7000-8000-000000000b0b")}
	passwordHash = sync.OnceValues(func() (string, error) { return password.Hash(e2ePassword, password.DefaultParameters()) })
)

// e2ePassword is every password user's password.
const e2ePassword = "correct horse battery staple"

// logins are the password login, the sessions and the API keys both engines of a cluster
// share, as two replicas share a session backend.
type logins struct {
	provider *password.Provider
	guard    *session.Guard
}

func newLogins(t *testing.T) *logins {
	t.Helper()
	hash, err := passwordHash()
	require.NoError(t, err)
	storage := password.NewMemoryStorage()
	for name, id := range users {
		require.NoError(t, storage.CreateUser(t.Context(), &password.User{ID: id, Username: new(name), PasswordHash: hash}))
	}
	backend, err := session.NewMemoryBackend(nil)
	require.NoError(t, err)
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	store, err := session.NewStore(&session.StoreOptions{Backend: backend, EncryptionKey: key, InsecureCookie: true})
	require.NoError(t, err)
	provider, err := password.New(&password.Options{Sessions: store, Storage: storage})
	require.NoError(t, err)
	guard, err := session.NewGuard(&session.GuardOptions{Store: store})
	require.NoError(t, err)
	return &logins{provider: provider, guard: guard}
}

// middleware authenticates an API request by its API key, else by its session.
func (l *logins) middleware(next http.Handler) http.Handler {
	keyed := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if identity := identityOf(request); identity != nil {
			request = request.WithContext(auth.WithIdentity(request.Context(), identity))
		}
		l.guard.RequireAPI()(next).ServeHTTP(writer, request)
	})
	return keyed
}

// Identify re-validates a socket by the API key of its upgrade request, else by its session (D6).
func (l *logins) Identify(ctx context.Context, request *http.Request) (*auth.Identity, error) {
	if identity := identityOf(request); identity != nil {
		return identity, nil
	}
	if strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
		return nil, auth.ErrUnauthenticated
	}
	return l.guard.Identify(ctx, request)
}

func identityOf(request *http.Request) *auth.Identity {
	identity, known := keys[strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")]
	if !known {
		return nil
	}
	return identity.Clone()
}

// note is a note as the API answers it.
type note struct {
	ID      uuid.UUID `json:"id"`
	Owner   string    `json:"owner"`
	Title   string    `json:"title"`
	Version int       `json:"version"`
}

type (
	createNoteBody struct {
		ID    uuid.UUID `json:"id,omitzero"`
		Title string    `json:"title" validate:"required"`
	}
	renameNoteBody struct {
		Title string `json:"title" validate:"required"`
	}
	noteParameters struct {
		ID uuid.UUID `path:"id"`
	}
)

var noteSQL = &query.ResourceSQL{From: "notes", IDColumn: "notes.id"}

// newRegistry declares the notes on pool: every user reads and writes only its own.
func newRegistry(t *testing.T, pool *pgxpool.Pool) *livewire.Registry {
	t.Helper()
	notes := &noteRepository{pool: pool}
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{BasePath: "/api"})
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		livewire.NewResource("notes", "/notes", &livewire.ResourceOptions[note]{
			Tables: []string{"notes"},
			Read:   authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeUUID, SQL: "notes.id", Filter: true, Sort: true},
				{Name: "owner", Type: query.TypeString, SQL: "notes.owner"},
				{Name: "title", Type: query.TypeText, SQL: "notes.title", Filter: true, Sort: true, Search: true},
				{Name: "version", Type: query.TypeInteger, SQL: "notes.version"},
			},
			DefaultSort: []query.SortTerm{{Field: "title"}},
			Scope:       ownNotes,
			Item:        &livewire.ItemKey{Parameter: "id", Field: "id"},
			Versioned:   true,
			List:        notes.list,
			Get:         notes.get,
			History:     notes.history,
			Export:      notes.export,
		}),
		livewire.NewMutation("createNote", http.MethodPost, "/notes", &livewire.MutationOptions[livewire.NoParameters, createNoteBody, note]{
			Check: authorization.IsUser(), Status: http.StatusCreated, ClientID: livewire.UUIDv7, Effect: livewire.Upserts("notes"),
			Handle: notes.create,
		}),
		livewire.NewMutation("renameNote", http.MethodPatch, "/notes/{id}", &livewire.MutationOptions[noteParameters, renameNoteBody, note]{
			Check: authorization.IsUser(), Resource: "notes", Effect: livewire.Upserts("notes"),
			Handle: notes.rename,
		}),
		livewire.NewMutation("deleteNote", http.MethodDelete, "/notes/{id}", &livewire.MutationOptions[noteParameters, livewire.NoBody, livewire.NoContent]{
			Check: authorization.IsUser(), Effect: livewire.Removes("notes"),
			Handle: notes.delete,
		}),
	))
	return registry
}

// ownNotes scopes every read to the caller's notes.
func ownNotes(ctx context.Context) (query.Predicate, error) {
	owner := livewire.OwnerKey(auth.IdentityFrom(ctx))
	return func(builder *query.Builder) { builder.WriteString("notes.owner = ").Argument(owner) }, nil
}

// noteRepository reads and writes notes on the engine's transaction (D45, D53).
type noteRepository struct {
	pool *pgxpool.Pool
}

func (r *noteRepository) database(ctx context.Context) postgresadapter.DBTX {
	if transaction := postgresadapter.TransactionFrom(ctx); transaction != nil {
		return transaction
	}
	return r.pool
}

const noteColumns = "id, owner, title, version"

func scanNote(row pgx.Row) (*note, error) {
	var scanned note
	err := row.Scan(&scanned.ID, &scanned.Owner, &scanned.Title, &scanned.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, livewire.NotFound()
	}
	return &scanned, err
}

func (r *noteRepository) list(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[note], error) {
	return postgresadapter.ListPage(ctx, r.pool, noteSQL, listQuery, func(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]note, error) {
		found, err := database.Query(ctx, "SELECT "+noteColumns+" FROM notes WHERE id = ANY($1)", postgresadapter.PageIDs(rows))
		if err != nil {
			return nil, err
		}
		notes, err := pgx.CollectRows(found, func(row pgx.CollectableRow) (note, error) {
			scanned, err := scanNote(row)
			if err != nil {
				return note{}, err
			}
			return *scanned, nil
		})
		byID := map[uuid.UUID]note{}
		for _, found := range notes {
			byID[found.ID] = found
		}
		return byID, err
	})
}

func (r *noteRepository) get(ctx context.Context, listQuery *query.ListQuery) (*note, error) {
	page, err := r.list(ctx, listQuery)
	if err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, livewire.NotFound()
	}
	return &page.Items[0], nil
}

func (r *noteRepository) history(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[note]], error) {
	return postgresadapter.HistoryPage(ctx, r.pool, "notes", listQuery, func(row []byte) (*note, error) {
		var decoded note
		return &decoded, json.Unmarshal(row, &decoded)
	})
}

func (r *noteRepository) export(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error) {
	return postgresadapter.WriteCSV(ctx, r.pool, &postgresadapter.Export{Specification: noteSQL, Query: listQuery, Columns: columns, ChunkSize: 2}, writer)
}

func (r *noteRepository) create(ctx context.Context, _ *livewire.NoParameters, body *createNoteBody) (*note, error) {
	id := body.ID
	if id == uuid.Nil {
		id = client.NewUUIDv7()
	}
	return scanNote(r.database(ctx).QueryRow(ctx, "INSERT INTO notes (id, owner, title) VALUES ($1, $2, $3) RETURNING "+noteColumns,
		id, livewire.OwnerKey(auth.IdentityFrom(ctx)), body.Title))
}

func (r *noteRepository) rename(ctx context.Context, parameters *noteParameters, body *renameNoteBody) (*note, error) {
	return scanNote(r.database(ctx).QueryRow(ctx, "UPDATE notes SET title = $1, version = version + 1 WHERE id = $2 AND owner = $3 RETURNING "+noteColumns,
		body.Title, parameters.ID, livewire.OwnerKey(auth.IdentityFrom(ctx))))
}

func (r *noteRepository) delete(ctx context.Context, parameters *noteParameters, _ *livewire.NoBody) (*livewire.NoContent, error) {
	deleted, err := r.database(ctx).Exec(ctx, "DELETE FROM notes WHERE id = $1 AND owner = $2", parameters.ID, livewire.OwnerKey(auth.IdentityFrom(ctx)))
	if err != nil {
		return nil, err
	}
	if deleted.RowsAffected() == 0 {
		return nil, livewire.NotFound()
	}
	return &livewire.NoContent{}, nil
}

// clientOf is the Go client runtime (livewire/client) of a user on a replica.
func clientOf(t *testing.T, replica *replica, key string) *client.Client {
	t.Helper()
	runtime, err := client.New(&client.Options{BaseURL: replica.baseURL(), APIKey: key, ReconnectDelay: 10 * time.Millisecond, MaximumReconnectDelay: 100 * time.Millisecond})
	require.NoError(t, err)
	return runtime
}

// createNote creates a note through a replica's REST route and answers it.
func createNote(t *testing.T, runtime *client.Client, body createNoteBody, idempotencyKey string) *note {
	t.Helper()
	var created note
	require.NoError(t, runtime.Call(t.Context(), &client.Call{Method: http.MethodPost, Path: "/notes", Body: body, IdempotencyKey: idempotencyKey}, &created))
	return &created
}
