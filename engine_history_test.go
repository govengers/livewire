package livewire_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

const documentID = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b40"

func historyOf(t *testing.T, payload json.RawMessage) livewire.ListResponse[livewire.Version[document]] {
	t.Helper()
	var history livewire.ListResponse[livewire.Version[document]]
	require.NoError(t, json.Unmarshal(payload, &history))
	return history
}

func TestHistoryReadsTheVersionsOfOneItem(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.addVersion(document{ID: documentID, Title: "draft", Secret: "s1"}, livewire.ChangeInsert)
	bed.addVersion(document{ID: documentID, Title: "final", Secret: "s2"}, livewire.ChangeUpdate)
	reader := auth.WithIdentity(t.Context(), person("ada", roleReader))
	read := func(ctx context.Context, parameters livewire.MapSource) (json.RawMessage, error) {
		return bed.engine.Read(ctx, livewire.ReadRequest{Target: "documents", Kind: livewire.ReadHistory, Source: parameters})
	}

	payload, err := read(reader, livewire.MapSource{"id": documentID, "filter": `{"where":{"field":"operation","op":"eq","value":"update"}}`, "sort": "-id"})
	require.NoError(t, err)
	history := historyOf(t, payload)
	require.Len(t, history.Items, 2, "the fake loader answers every version; the query below carries the filter")
	require.Equal(t, 2, history.Total)
	require.Equal(t, "final", history.Items[1].Data.Title)
	require.Empty(t, history.Items[1].Data.Secret, "the data passes the column checks (D39)")
	require.Equal(t, livewire.ChangeUpdate, history.Items[1].Operation)
	require.NotContains(t, string(payload), "s2")
	text, arguments := bed.historyQueries[0].PageSQL(&query.ResourceSQL{From: "documents_history", IDColumn: "version"})
	require.Contains(t, text, "id = $1", "the item key scopes the history")
	require.Contains(t, text, "operation")
	require.Equal(t, documentID, arguments[0])
	require.Contains(t, text, "ORDER BY (version) DESC")

	payload, err = read(auth.WithIdentity(t.Context(), admin("root")), livewire.MapSource{"id": documentID})
	require.NoError(t, err)
	require.Equal(t, "s2", historyOf(t, payload).Items[1].Data.Secret, "an admin reads the secret")

	_, err = read(reader, livewire.MapSource{"id": "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b41"})
	requireStatus(t, err, http.StatusNotFound)
	_, err = read(reader, livewire.MapSource{"id": documentID, "after": "x"})
	requireStatus(t, err, http.StatusBadRequest)
	_, err = read(reader, livewire.MapSource{"id": documentID, "sort": "title"})
	requireStatus(t, err, http.StatusBadRequest)
	_, err = read(reader, livewire.MapSource{})
	requireStatus(t, err, http.StatusBadRequest)
	_, err = bed.engine.Read(reader, livewire.ReadRequest{Target: "gadgets", Kind: livewire.ReadHistory, Source: livewire.MapSource{"id": "a"}})
	requireStatus(t, err, http.StatusNotFound)
}

func TestHistorySubscriptionReRunsOnItsTable(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.addVersion(document{ID: documentID, Title: "draft"}, livewire.ChangeInsert)
	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "documents.history", map[string]string{"id": documentID}, nil))
	snapshot := socket.push()
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.Len(t, historyOf(t, snapshot.Payload).Items, 1)

	bed.addVersion(document{ID: documentID, Title: "final"}, livewire.ChangeUpdate)
	bed.feed.publish(livewire.Change{Table: "documents", Operation: livewire.ChangeUpdate, ID: documentID})
	changes := socket.push()
	require.Equal(t, livewire.PushListChange, changes.Kind, "the history is diffed by version")
	change := changeOf(t, changes.Payload)
	require.Len(t, change.Upserts, 1)
	require.Contains(t, string(change.Upserts[0]), `"id":2`)
	require.Eventually(t, func() bool {
		return outcomeWith(bed, livewire.OutcomeRerun, func(outcome livewire.Outcome) bool { return outcome.Target == "documents.history" })
	}, frameWait, time.Millisecond, "a re-run names its read kind like its subscribe and end")
}

func TestHistoryDeclarationMistakes(t *testing.T) {
	history := func(context.Context, *query.ListQuery) (*livewire.Page[livewire.Version[part]], error) {
		return &livewire.Page[livewire.Version[part]]{}, nil
	}
	unversioned := partOptions()
	unversioned.History = history
	require.ErrorContains(t, addError(t, livewire.NewResource("parts", "/parts", unversioned)), "a History loader needs Versioned and Item")

	byName := partOptions()
	byName.Versioned, byName.History = true, history
	byName.Item = &livewire.ItemKey{Parameter: "name", Field: "name"}
	require.ErrorContains(t, addError(t, livewire.NewResource("parts", "/parts", byName)), `the item key on the uuid field "id"`)

	versioned := partOptions()
	versioned.Versioned, versioned.History = true, history
	resource := newParts(t, versioned)
	require.Equal(t, "/parts/{id}/history", resource.HistoryPath())
	require.Empty(t, newParts(t, partOptions()).HistoryPath(), "no History loader, no route")
	require.NoError(t, livewire.HistoryFields().Validate())
}

// thing is owned by one subject; ExternalID is a value its owner may set.
type thing struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
	Owner      string `json:"owner"`
	Version    int    `json:"version"`
}

const alicesThing = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b51"

// thingsKeyedOn declares the things of alice and mallory with the item key on field: the Get
// loader enforces ownership, the History loader answers the versions of the id the query
// is scoped to. Mallory's thing carries alice's thing's id as its externalId.
func thingsKeyedOn(field string) *livewire.Resource {
	things := []thing{
		{ID: alicesThing, ExternalID: "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b52", Owner: "alice", Version: 1},
		{ID: "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b53", ExternalID: alicesThing, Owner: "mallory", Version: 1},
	}
	value := func(item thing) string {
		if field == "externalId" {
			return item.ExternalID
		}
		return item.ID
	}
	return livewire.NewResource("things", "/things", &livewire.ResourceOptions[thing]{
		Read: authorization.IsUser(), Versioned: true,
		Fields: query.Fields{
			{Name: "id", Type: query.TypeUUID, SQL: "things.id", Filter: true},
			{Name: "externalId", Type: query.TypeUUID, SQL: "things.external_id", Filter: true},
		},
		Item: &livewire.ItemKey{Parameter: "key", Field: field},
		List: func(context.Context, *query.ListQuery) (*livewire.Page[thing], error) {
			return &livewire.Page[thing]{}, nil
		},
		Get: func(ctx context.Context, listQuery *query.ListQuery) (*thing, error) {
			_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "things", IDColumn: "things.id"})
			for _, item := range things {
				if slices.Contains(arguments, any(value(item))) && item.Owner == auth.IdentityFrom(ctx).Subject {
					return &item, nil
				}
			}
			return nil, livewire.NotFound()
		},
		History: func(_ context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[thing]], error) {
			_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "things_history", IDColumn: "version"})
			for _, item := range things {
				if item.ID == arguments[0] {
					return &livewire.Page[livewire.Version[thing]]{Items: []livewire.Version[thing]{{ID: 1, Operation: livewire.ChangeInsert, Data: &item}}, Total: 1}, nil
				}
			}
			return &livewire.Page[livewire.Version[thing]]{}, nil
		},
	})
}

func TestHistoryIsScopedByTheItemsOwnID(t *testing.T) {
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.ErrorContains(t, registry.Add(thingsKeyedOn("externalId")), `the item key on the uuid field "id"`,
		"a history keyed on another field would let mallory's thing name alice's history")

	registry, err = livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(thingsKeyedOn("id")))
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: newFakeIdentities(), Transactor: &fakeTransactor{}, SameHostOnly: true})
	require.NoError(t, err)
	history := func(subject string) (json.RawMessage, error) {
		ctx := auth.WithIdentity(t.Context(), person(subject))
		return engine.Read(ctx, livewire.ReadRequest{Target: "things", Kind: livewire.ReadHistory, Source: livewire.MapSource{"key": alicesThing}})
	}
	_, err = history("mallory")
	requireStatus(t, err, http.StatusNotFound)
	payload, err := history("alice")
	require.NoError(t, err)
	require.Contains(t, string(payload), `"owner":"alice"`)
}

func TestHistoryFiltersOverHistoryFieldsOnEveryPath(t *testing.T) {
	bed := newTestbed(t, func(options *livewire.EngineOptions) { options.SafetyTickInterval = 5 * time.Millisecond })
	bed.addVersion(document{ID: documentID, Title: "draft"}, livewire.ChangeInsert)
	bed.addVersion(document{ID: documentID, Title: "final"}, livewire.ChangeUpdate)
	socket := bed.dial("ada")
	updates := `{"where":{"field":"operation","op":"eq","value":"update"}}`
	inline := socket.do(livewire.OperationSubscribe, "documents.history", map[string]string{"id": documentID, "filter": updates}, nil)
	subscribed(t, inline)
	require.NotEmpty(t, inline.FilterID, "the inline history filter is registered under documents.history")
	require.Equal(t, livewire.PushSnapshot, socket.push().Kind)
	bed.filters.mutex.Lock()
	stored, ok := bed.filters.filters[inline.FilterID]
	bed.filters.mutex.Unlock()
	require.True(t, ok)
	require.Equal(t, "documents.history", stored.Resource)

	byID := socket.do(livewire.OperationSubscribe, "documents.history", map[string]string{"id": documentID, "filterId": inline.FilterID}, nil)
	subscribed(t, byID)
	socket.push()
	refused := socket.do(livewire.OperationQuery, "documents", map[string]string{"filterId": inline.FilterID}, nil)
	require.Equal(t, http.StatusNotFound, refused.Status, "a history filter is not the resource's")

	recent := `{"where":{"field":"validFrom","op":"gt","value":{"relative":"-7d"}}}`
	subscribed(t, socket.do(livewire.OperationSubscribe, "documents.history", map[string]string{"id": documentID, "filter": recent}, nil))
	socket.push()
	loads := len(bed.historyQueriesSoFar())
	require.Eventually(t, func() bool { return len(bed.historyQueriesSoFar()) > loads }, frameWait, time.Millisecond, "a relative time re-runs on the safety tick")

	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	payload, err := bed.engine.SaveFilter(ctx, []byte(`{"resource":"documents.history","name":"Updates","filter":`+updates+`}`))
	require.NoError(t, err, "POST /filters stores a history filter")
	require.Contains(t, string(payload), `"resource":"documents.history"`)
	_, err = bed.engine.SaveFilter(ctx, []byte(`{"resource":"gadgets.history","filter":{}}`))
	requireStatus(t, err, http.StatusBadRequest)
}

func TestDeletedItemHasNoHistory(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.addVersion(document{ID: documentID, Title: "draft"}, livewire.ChangeInsert)
	ctx := auth.WithIdentity(t.Context(), person("ada", roleReader))
	read := func(parameters livewire.MapSource) error {
		_, err := bed.engine.Read(ctx, livewire.ReadRequest{Target: "documents", Kind: livewire.ReadHistory, Source: parameters})
		return err
	}
	deletes := `{"where":{"field":"operation","op":"eq","value":"delete"}}`
	requireStatus(t, read(livewire.MapSource{"id": documentID, "filter": deletes}), http.StatusBadRequest)

	socket := bed.dial("ada")
	subscribed(t, socket.do(livewire.OperationSubscribe, "documents.history", map[string]string{"id": documentID}, nil))
	socket.push()
	bed.deleteDocument(documentID)
	bed.addVersion(document{ID: documentID, Title: "draft"}, livewire.ChangeDelete)
	bed.feed.publish(livewire.Change{Table: "documents", Operation: livewire.ChangeDelete, ID: documentID})
	ended := socket.push()
	require.Equal(t, livewire.PushError, ended.Kind, "the history of a deleted item ends with its item (D56)")
	require.Equal(t, livewire.CodeNotFound, errorOf(t, ended.Payload).Code)
	requireStatus(t, read(livewire.MapSource{"id": documentID}), http.StatusNotFound)
}

func TestHistoryQueryCarriesTheCountCap(t *testing.T) {
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	var caps []int
	options := partOptions()
	options.Versioned, options.CountCap = true, 2
	options.History = func(_ context.Context, listQuery *query.ListQuery) (*livewire.Page[livewire.Version[part]], error) {
		caps = append(caps, listQuery.CountCap)
		return &livewire.Page[livewire.Version[part]]{Total: 3}, nil
	}
	require.NoError(t, registry.Add(livewire.NewResource("parts", "/parts", options)))
	engine, err := livewire.NewEngine(&livewire.EngineOptions{Registry: registry, IdentitySource: newFakeIdentities(), Transactor: &fakeTransactor{}, SameHostOnly: true})
	require.NoError(t, err)
	payload, err := engine.Read(userContext(), livewire.ReadRequest{Target: "parts", Kind: livewire.ReadHistory, Source: livewire.MapSource{"id": documentID}})
	require.NoError(t, err)
	require.Equal(t, []int{2}, caps, "the loader counts at most CountCap + 1 (D14)")
	require.Contains(t, string(payload), `"total":3`)
}
