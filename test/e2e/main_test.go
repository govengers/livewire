//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
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

var postgres struct {
	port     int
	mutex    sync.Mutex
	sequence atomic.Int64
}

const templateDatabase = "e2e_template"

func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	os.Exit(run(m))
}

func run(m *testing.M) int {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fail("pick a port", err)
	}
	postgres.port = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return fail("free the port", err)
	}
	runtimePath, err := os.MkdirTemp("", "livewire-e2e-")
	if err != nil {
		return fail("create the runtime directory", err)
	}
	defer func() { _ = os.RemoveAll(runtimePath) }() // a temporary directory; a leftover is harmless
	server := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(postgres.port)). //nolint:gosec // G115: a TCP port fits a uint32
		RuntimePath(runtimePath).
		// ponytail: the binaries are extracted once per machine; a run killed mid-extraction
		// leaves a broken directory, which deleting it repairs.
		BinariesPath(filepath.Join(os.TempDir(), "livewire-e2e-postgres-binaries")).
		Logger(io.Discard))
	if err := server.Start(); err != nil {
		return fail("start embedded postgres", err)
	}
	defer func() { _ = server.Stop() }() // the result is decided; a failed stop changes nothing
	http.DefaultClient.CloseIdleConnections()
	if err := migrateTemplate(); err != nil {
		return fail("migrate the template database", err)
	}
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			return fail("goroutines leaked", err)
		}
	}
	return code
}

func fail(what string, err error) int {
	fmt.Fprintf(os.Stderr, "e2e: %s: %v\n", what, err)
	return 1
}

func dsn(database string) string {
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s", postgres.port, database)
}

// migrateTemplate applies the schema and livewire's SQL (D22) as an application's migrations.
func migrateTemplate() error {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn("postgres"))
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }() // the template exists or the error is returned
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDatabase); err != nil {
		return err
	}
	connection, err := pgx.Connect(ctx, dsn(templateDatabase))
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close(ctx) }() // as above
	history, err := postgresadapter.HistorySQL("notes")
	if err != nil {
		return err
	}
	for _, statement := range []string{
		schemaSQL, postgresadapter.TriggerFunctionSQL, postgresadapter.TriggerSQL("notes"), history,
		postgresadapter.FiltersSQL, postgresadapter.IdempotencySQL, postgresadapter.AuditSQL, postgresadapter.PresenceSQL,
	} {
		if _, err := connection.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%w\n%s", err, statement)
		}
	}
	return nil
}

// cluster is one database with two engines on it, each behind a server of its own.
type cluster struct {
	pool *pgxpool.Pool // the test's own connection, for assertions
	a, b *replica
}

// replica is one engine, its pool, its Listener and its server.
type replica struct {
	engine *livewire.Engine
	server *httptest.Server
	filter *postgresadapter.FilterStore
}

// baseURL is the replica's API root with the registry's BasePath.
func (r *replica) baseURL() string { return r.server.URL + "/api" }

// newCluster clones the template into a database of the test's own and starts two replicas.
func newCluster(t *testing.T) *cluster {
	t.Helper()
	name := "e2e_" + strconv.FormatInt(postgres.sequence.Add(1), 10)
	admin, err := pgx.Connect(t.Context(), dsn("postgres"))
	require.NoError(t, err)
	postgres.mutex.Lock()
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+name+" TEMPLATE "+templateDatabase)
	postgres.mutex.Unlock()
	require.NoError(t, admin.Close(t.Context()))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, dsn("postgres"))
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		require.NoError(t, err)
		require.NoError(t, admin.Close(ctx))
	})
	pool, err := pgxpool.New(t.Context(), dsn(name))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	started := &cluster{pool: pool, a: newReplica(t, name), b: newReplica(t, name)}
	require.Eventually(t, func() bool {
		var listening int
		err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'livewire_feed' AND state = 'idle' AND query LIKE 'LISTEN%'`).Scan(&listening)
		return err == nil && listening == 2
	}, 10*time.Second, 10*time.Millisecond, "both engines' feeds listen")
	return started
}

// newReplica starts one engine on the database with the postgresadapter's Listener,
// Transactor and stores, and serves it on gin.
func newReplica(t *testing.T, database string) *replica {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn(database))
	require.NoError(t, err)
	registry := newRegistry(t, pool)
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: dsn(database) + "?application_name=livewire_feed", RetryInterval: 10 * time.Millisecond})
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
		Registry: registry, ChangeFeed: listener, IdentitySource: apiKeys{}, Transactor: transactor,
		FilterStore: filters, IdempotencyStore: idempotency, AuditStore: audit, PresenceStore: presences,
		SameHostOnly: true, DeliveryBatchWait: 10 * time.Millisecond, RetryInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	router := gin.New()
	router.Use(func(ginContext *gin.Context) {
		if identity := identityOf(ginContext.Request); identity != nil {
			ginContext.Request = ginContext.Request.WithContext(auth.WithIdentity(ginContext.Request.Context(), identity))
		}
	})
	require.NoError(t, ginadapter.RegisterRoutes(router, &ginadapter.Options{Engine: engine}))
	router.GET("/api/socket", ginadapter.SocketHandler(engine))
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

// apiKeys re-validates a socket by the API key of its upgrade request (D6).
type apiKeys struct{}

func (apiKeys) Identify(_ context.Context, request *http.Request) (*auth.Identity, error) {
	if identity := identityOf(request); identity != nil {
		return identity, nil
	}
	return nil, auth.ErrUnauthenticated
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
