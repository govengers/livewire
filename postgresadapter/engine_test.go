package postgresadapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/postgresadapter"
	"github.com/govengers/livewire/query"
)

// The engine on embedded Postgres (D48): the Listener, Transactor and stores of this
// package behind a real Engine, and a repository the way an application writes one.

// livePart is a part as the engine serves it; Tenant is livewire.tenant as the read saw it.
type livePart struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Tenant string    `json:"tenant"`
}

type createPartBody struct {
	Name string `json:"name"`
}

var (
	engineIdentity = &auth.Identity{Provider: "password", Subject: "alice", Name: "Alice", Active: true}
	livePartSQL    = &query.ResourceSQL{
		From: "parts", IDColumn: "parts.id",
		Derived: []query.DerivedColumn{{Name: "tenant", SQL: "current_setting('livewire.tenant', true)"}},
	}
)

// staticIdentity is the IdentitySource of the test: alice, always.
type staticIdentity struct{}

func (staticIdentity) Identify(context.Context, *http.Request) (*auth.Identity, error) {
	return engineIdentity.Clone(), nil
}

// liveEngine is an Engine on database, served behind alice's authentication.
type liveEngine struct {
	engine *livewire.Engine
	server *httptest.Server
}

func newLiveEngine(t *testing.T, database *testDatabase) *liveEngine {
	t.Helper()
	audit, err := postgresadapter.NewAuditStore(&postgresadapter.AuditStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{
		Tenant: func(context.Context, any) (string, error) { return "north", nil },
	})
	require.NoError(t, err)
	require.NoError(t, registry.Add(
		livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[livePart]{
			Tables: []string{"parts"},
			Read:   authorization.IsUser(),
			Fields: query.Fields{
				{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true},
				{Name: "name", Type: query.TypeText, SQL: "parts.name", Filter: true, Sort: true},
				{Name: "tenant", Type: query.TypeString, SQL: "current_setting('livewire.tenant', true)"},
			},
			DefaultSort: []query.SortTerm{{Field: "name"}},
			List: func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livePart], error) {
				return postgresadapter.ListPage(ctx, database.pool, livePartSQL, listQuery, hydrateLiveParts)
			},
			Export: func(ctx context.Context, listQuery *query.ListQuery, columns query.Fields, writer io.Writer) (int, error) {
				export := &postgresadapter.Export{Specification: livePartSQL, Query: listQuery, Columns: columns, ChunkSize: 1}
				return postgresadapter.WriteCSV(ctx, database.pool, export, writer)
			},
		}),
		versionedParts(database),
		livewire.NewMutation("restorePart", http.MethodPost, "/versioned-parts", &livewire.MutationOptions[livewire.NoParameters, restorePartBody, versionedPart]{
			Check: authorization.IsUser(), ClientID: livewire.UUIDv7,
			Handle: func(ctx context.Context, _ *livewire.NoParameters, body *restorePartBody) (*versionedPart, error) {
				_, err := postgresadapter.TransactionFrom(ctx).Exec(ctx, "INSERT INTO parts (id, name) VALUES ($1, $2)", body.ID, body.Name)
				return &versionedPart{ID: body.ID, Name: body.Name}, err
			},
		}),
		livewire.NewMutation("importPart", http.MethodPost, "/parts/import", &livewire.MutationOptions[livewire.NoParameters, createPartBody, livePart]{
			Check: authorization.IsUser(),
			Handle: func(ctx context.Context, _ *livewire.NoParameters, body *createPartBody) (*livePart, error) {
				imported := livePart{Name: body.Name}
				err := postgresadapter.TransactionFrom(ctx).QueryRow(ctx, "INSERT INTO parts (name) VALUES ($1) RETURNING id", body.Name).Scan(&imported.ID)
				return &imported, err
			},
		}),
		livewire.NewMutation("createPart", http.MethodPost, "/parts", &livewire.MutationOptions[livewire.NoParameters, createPartBody, livePart]{
			Check:  authorization.IsUser(),
			Status: http.StatusCreated,
			Handle: func(ctx context.Context, _ *livewire.NoParameters, body *createPartBody) (*livePart, error) {
				var created livePart
				err := postgresadapter.TransactionFrom(ctx).QueryRow(ctx, "INSERT INTO parts (name) VALUES ($1) RETURNING id, name, current_setting('livewire.tenant', true)",
					body.Name).Scan(&created.ID, &created.Name, &created.Tenant)
				if err != nil {
					return nil, err
				}
				if body.Name == "refused" {
					return nil, livewire.Conflict() // after the writes: the rollback takes them back
				}
				return &created, nil
			},
		}),
	))
	listener, err := postgresadapter.NewListener(&postgresadapter.ListenerOptions{DSN: database.dsn() + "?application_name=livewire_engine_feed", RetryInterval: 10 * time.Millisecond})
	require.NoError(t, err)
	transactor := newTransactor(t, database.pool)
	filters := newFilterStore(t, database, postgresadapter.FilterStoreOptions{})
	idempotency := newIdempotencyStore(t, database)
	presences, err := postgresadapter.NewPresenceStore(&postgresadapter.PresenceStoreOptions{Pool: database.pool})
	require.NoError(t, err)
	engine, err := livewire.NewEngine(&livewire.EngineOptions{
		Registry: registry, ChangeFeed: listener, IdentitySource: staticIdentity{}, Transactor: transactor,
		FilterStore: filters, IdempotencyStore: idempotency, AuditStore: audit, PresenceStore: presences,
		AllowedOrigins: []string{"https://app.example"}, DeliveryBatchWait: 10 * time.Millisecond, RetryInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		engine.Handler().ServeHTTP(writer, request.WithContext(auth.WithIdentity(request.Context(), engineIdentity.Clone())))
	}))
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { _ = engine.Run(ctx) }) // returns nil after Close
	t.Cleanup(func() {
		closing, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		require.NoError(t, engine.Close(closing))
		server.Close()
		cancel()
		running.Wait()
	})
	require.Eventually(t, func() bool {
		var listening bool
		err := database.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'livewire_engine_feed' AND state = 'idle' AND query LIKE 'LISTEN%')`).Scan(&listening)
		return err == nil && listening
	}, 5*time.Second, 10*time.Millisecond, "the engine's feed listens")
	return &liveEngine{engine: engine, server: server}
}

// restorePartBody creates a part under an id the client chose (D18).
type restorePartBody struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// versionedPart is a part as its history shows it.
type versionedPart struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Version int       `json:"version"`
}

// versionedParts reads the parts with their history (D56): the item read by id, the
// versions through HistoryPage.
func versionedParts(database *testDatabase) *livewire.Resource {
	specification := &query.ResourceSQL{From: "parts", IDColumn: "parts.id"}
	hydrate := func(ctx context.Context, handle postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]versionedPart, error) {
		result, err := handle.Query(ctx, "SELECT id, name, 0 FROM parts WHERE id = ANY($1)", postgresadapter.PageIDs(rows))
		if err != nil {
			return nil, err
		}
		loaded, err := pgx.CollectRows(result, pgx.RowToStructByPos[versionedPart])
		parts := map[uuid.UUID]versionedPart{}
		for _, item := range loaded {
			parts[item.ID] = item
		}
		return parts, err
	}
	return livewire.NewResource("versionedParts", "/versioned-parts", &livewire.ResourceOptions[versionedPart]{
		Tables: []string{"parts"}, Read: authorization.IsUser(), Versioned: true,
		Fields: query.Fields{{Name: "id", Type: query.TypeUUID, SQL: "parts.id", Filter: true, Sort: true}},
		Item:   &livewire.ItemKey{Parameter: "id", Field: "id"},
		List: func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[versionedPart], error) {
			return postgresadapter.ListPage(ctx, database.pool, specification, listQuery, hydrate)
		},
		Get: func(ctx context.Context, listQuery *query.ListQuery) (*versionedPart, error) {
			page, err := postgresadapter.ListPage(ctx, database.pool, specification, listQuery, hydrate)
			if err != nil || len(page.Items) == 0 {
				return nil, errors.Join(err, livewire.NotFound())
			}
			return &page.Items[0], nil
		},
		History: func(ctx context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[versionedPart]], error) {
			return postgresadapter.HistoryPage(ctx, database.pool, "parts", listQuery, func(row []byte) (*versionedPart, error) {
				var stored versionedPart
				return &stored, json.Unmarshal(row, &stored)
			})
		},
	})
}

func TestEngineReadsAHistoryFromPostgres(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	live := newLiveEngine(t, database)
	_, payload, err := live.createPart(t, "bolt", uuid.NewString())
	require.NoError(t, err)
	var created livePart
	require.NoError(t, json.Unmarshal(payload, &created))
	for _, name := range []string{"nut", "screw"} {
		_, err := database.pool.Exec(t.Context(), "UPDATE parts SET name = $1 WHERE id = $2", name, created.ID)
		require.NoError(t, err)
	}

	read := func(parameters livewire.MapSource) livewire.ListResponse[livewire.Version[versionedPart]] {
		t.Helper()
		parameters["id"] = created.ID.String()
		payload, err := live.engine.Read(asAlice(t), livewire.ReadRequest{Target: "versionedParts", Kind: livewire.ReadHistory, Source: parameters})
		require.NoError(t, err)
		var history livewire.ListResponse[livewire.Version[versionedPart]]
		require.NoError(t, json.Unmarshal(payload, &history))
		return history
	}
	all := read(livewire.MapSource{})
	require.Equal(t, 3, all.Total)
	require.Equal(t, []int{1, 2, 3}, []int{all.Items[0].ID, all.Items[1].ID, all.Items[2].ID})
	require.Equal(t, livewire.ChangeInsert, all.Items[0].Operation)
	require.Equal(t, []string{"password:alice", "createPart"}, []string{all.Items[0].Actor, all.Items[0].Mutation}, "the engine's actor")
	require.NotNil(t, all.Items[0].ValidTo)
	require.Nil(t, all.Items[2].ValidTo, "the current version is open")
	require.Equal(t, "bolt", all.Items[0].Data.Name)

	updates := read(livewire.MapSource{"filter": `{"where":{"field":"operation","op":"eq","value":"update"}}`, "sort": "-id", "offset": "1", "limit": "1"})
	require.Equal(t, 2, updates.Total, "the filter counts the updates")
	require.Len(t, updates.Items, 1)
	require.Equal(t, 2, updates.Items[0].ID, "the second update by version descending")
	require.Equal(t, "nut", updates.Items[0].Data.Name)
}

func TestEngineHistoryOfAReCreatedIDIsItsOwn(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	live := newLiveEngine(t, database)
	_, payload, err := live.createPart(t, "alices-secret", uuid.NewString())
	require.NoError(t, err)
	var deleted livePart
	require.NoError(t, json.Unmarshal(payload, &deleted))
	_, err = database.pool.Exec(t.Context(), "UPDATE parts SET name = 'alices-other-secret' WHERE id = $1", deleted.ID)
	require.NoError(t, err)
	_, err = database.pool.Exec(t.Context(), "DELETE FROM parts WHERE id = $1", deleted.ID)
	require.NoError(t, err)

	body := []byte(`{"id":"` + deleted.ID.String() + `","name":"mallorys"}`)
	_, _, err = live.engine.Call(asAlice(t), livewire.CallRequest{Target: "restorePart", Source: livewire.MapSource{}, Body: body})
	require.NoError(t, err, "a ClientID create re-uses the deleted id")
	read, err := live.engine.Read(asAlice(t), livewire.ReadRequest{Target: "versionedParts", Kind: livewire.ReadHistory, Source: livewire.MapSource{"id": deleted.ID.String()}})
	require.NoError(t, err)
	var history livewire.ListResponse[livewire.Version[versionedPart]]
	require.NoError(t, json.Unmarshal(read, &history))
	require.Equal(t, 1, history.Total, "the re-created item's history is its own life only")
	require.Len(t, history.Items, 1)
	require.Equal(t, "mallorys", history.Items[0].Data.Name)
	require.NotContains(t, string(read), "alices-")
	require.Equal(t, 4, history.Items[0].ID, "versions count on across lives; the life bounds the read")
}

func hydrateLiveParts(ctx context.Context, database postgresadapter.DBTX, rows []postgresadapter.PageRow) (map[uuid.UUID]livePart, error) {
	result, err := database.Query(ctx, "SELECT id, name, '' FROM parts WHERE id = ANY($1)", postgresadapter.PageIDs(rows))
	if err != nil {
		return nil, err
	}
	loaded, err := pgx.CollectRows(result, pgx.RowToStructByPos[livePart])
	if err != nil {
		return nil, err
	}
	tenants := map[uuid.UUID]string{}
	for _, row := range rows {
		tenants[row.ID], _ = row.Derived["tenant"].(string)
	}
	parts := map[uuid.UUID]livePart{}
	for _, item := range loaded {
		item.Tenant = tenants[item.ID]
		parts[item.ID] = item
	}
	return parts, nil
}

func asAlice(t *testing.T) context.Context {
	t.Helper()
	return auth.WithIdentity(t.Context(), engineIdentity.Clone())
}

func (e *liveEngine) createPart(t *testing.T, name, key string) (int, json.RawMessage, error) {
	t.Helper()
	return e.engine.Call(asAlice(t), livewire.CallRequest{
		Target: "createPart", Source: livewire.MapSource{}, Body: []byte(`{"name":"` + name + `"}`), IdempotencyKey: key, RequestID: key,
	})
}

func counts(t *testing.T, database *testDatabase, name, key string) []int {
	t.Helper()
	var parts, audits, records int
	require.NoError(t, database.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM parts WHERE name = $1),
		(SELECT count(*) FROM livewire_audit WHERE idempotency_key = $2::uuid AND tenant = 'north' AND actor = 'password:alice'),
		(SELECT count(*) FROM livewire_idempotency WHERE key = $2::uuid AND owner = 'password:alice' AND tenant = 'north' AND status = 201)`,
		name, key).Scan(&parts, &audits, &records))
	return []int{parts, audits, records}
}

func TestEngineCommitsAuditsAndReplaysOnPostgres(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	live := newLiveEngine(t, database)
	key := uuid.NewString()

	status, payload, err := live.createPart(t, "bolt", key)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	var created livePart
	require.NoError(t, json.Unmarshal(payload, &created))
	require.Equal(t, "north", created.Tenant, "the mutation ran under the tenant")
	require.Equal(t, []int{1, 1, 1}, counts(t, database, "bolt", key), "the part, its audit row and the idempotency record committed together")
	var actor, historyMutation string
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT actor, mutation FROM parts_history WHERE id = $1", created.ID).Scan(&actor, &historyMutation))
	require.Equal(t, []string{"password:alice", "createPart"}, []string{actor, historyMutation}, "the history trigger read livewire.actor")

	status, replayed, err := live.createPart(t, "bolt", "{"+strings.ToUpper(key)+"}")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	require.JSONEq(t, string(payload), string(replayed), "the key in another spelling replays the recorded answer")
	require.Equal(t, []int{1, 1, 1}, counts(t, database, "bolt", key), "a replay writes nothing")

	refusedKey := uuid.NewString()
	_, _, err = live.createPart(t, "refused", refusedKey)
	requireCode(t, err, livewire.CodeConflict)
	require.Equal(t, []int{0, 1, 0}, counts(t, database, "refused", refusedKey), "the part rolled back, the key is free, the failed call is audited")
	var auditStatus int
	var auditMutation, auditRequestID string
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT status, mutation, request_id FROM livewire_audit WHERE idempotency_key = $1::uuid", refusedKey).
		Scan(&auditStatus, &auditMutation, &auditRequestID))
	require.Equal(t, http.StatusConflict, auditStatus, "written on the pool after the rollback")
	require.Equal(t, []string{"createPart", refusedKey}, []string{auditMutation, auditRequestID})
	var reservations int
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT count(*) FROM livewire_idempotency WHERE key = $1::uuid", refusedKey).Scan(&reservations))
	require.Zero(t, reservations)
}

func TestEngineReadsUnderTheTenantAndReRunsOnNotify(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	live := newLiveEngine(t, database)
	insertPart(t, database, "bolt")

	payload, err := live.engine.Read(asAlice(t), livewire.ReadRequest{Target: "parts", Source: livewire.MapSource{}})
	require.NoError(t, err)
	var list livewire.ListResponse[livePart]
	require.NoError(t, json.Unmarshal(payload, &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, "north", list.Items[0].Tenant, "ListPage on the pool ran in the engine's read transaction")

	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(live.server.URL, "http"), nil)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	t.Cleanup(func() { _ = connection.Close() }) // the engine closes it first; the error carries nothing
	require.NoError(t, connection.WriteJSON(livewire.RequestFrame{ID: "1", Operation: livewire.OperationSubscribe, Target: "parts"}))
	snapshot := nextPush(t, connection)
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)

	added := insertPart(t, database, "nut") // outside the engine: only the NOTIFY tells it
	change := nextPush(t, connection)
	require.Equal(t, livewire.PushListChange, change.Kind, "a re-run after the notification, not a resync")
	var listChange livewire.ListChange
	require.NoError(t, json.Unmarshal(change.Payload, &listChange))
	require.Len(t, listChange.Upserts, 1)
	require.Contains(t, string(listChange.Upserts[0]), added)
}

func TestEngineBatchHistoryNamesEachCall(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	live := newLiveEngine(t, database)
	key := uuid.NewString()
	calls := []livewire.BatchCall{
		{Target: "createPart", Body: json.RawMessage(`{"name":"bolt"}`)},
		{Target: "importPart", Body: json.RawMessage(`{"name":"nut"}`)},
	}
	body, err := json.Marshal(calls)
	require.NoError(t, err)
	_, err = live.engine.Batch(asAlice(t), livewire.BatchRequest{Body: body, IdempotencyKey: key, RequestID: key})
	require.NoError(t, err)

	rows, err := database.pool.Query(t.Context(), `SELECT parts.name, history.mutation, history.request_id, history.actor
		FROM parts_history history JOIN parts ON parts.id = history.id ORDER BY parts.name`)
	require.NoError(t, err)
	type historyActor struct{ Name, Mutation, RequestID, Actor string }
	history, err := pgx.CollectRows(rows, pgx.RowToStructByPos[historyActor])
	require.NoError(t, err)
	require.Equal(t, []historyActor{
		{Name: "bolt", Mutation: "createPart", RequestID: key, Actor: "password:alice"},
		{Name: "nut", Mutation: "importPart", RequestID: key, Actor: "password:alice"},
	}, history, "each call of the batch set its own mutation (D58)")
	var audited []string
	require.NoError(t, database.pool.QueryRow(t.Context(), "SELECT array_agg(mutation ORDER BY mutation) FROM livewire_audit WHERE request_id = $1", key).Scan(&audited))
	require.Equal(t, []string{"createPart", "importPart"}, audited, "one audit row per call")
}

func TestEngineExportsCSVInItsReadTransaction(t *testing.T) {
	t.Parallel()
	database := newDatabase(t)
	live := newLiveEngine(t, database)
	insertPart(t, database, "bolt")
	insertPart(t, database, "nut")

	var written strings.Builder
	rows, err := live.engine.Export(asAlice(t), livewire.ExportRequest{Target: "parts", Source: livewire.MapSource{"sort": "-name"}, Writer: &written})
	require.NoError(t, err)
	require.Equal(t, 2, rows, "two chunks of one row")
	lines := strings.Split(strings.TrimSpace(written.String()), "\n")
	require.Len(t, lines, 3)
	require.Equal(t, "id,name,tenant", lines[0])
	require.True(t, strings.HasSuffix(lines[1], ",nut,north"), "sorted, under the tenant of the engine's transaction: %s", lines[1])
}

// nextPush reads frames until a push arrives and returns it.
func nextPush(t *testing.T, connection *websocket.Conn) livewire.PushFrame {
	t.Helper()
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		_, data, err := connection.ReadMessage()
		require.NoError(t, err)
		var probe map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &probe))
		if _, isPush := probe["sub"]; !isPush {
			var answer livewire.ResponseFrame
			require.NoError(t, json.Unmarshal(data, &answer))
			require.True(t, answer.OK, "%s", data)
			continue
		}
		var push livewire.PushFrame
		require.NoError(t, json.Unmarshal(data, &push))
		return push
	}
}
