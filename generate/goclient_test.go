package generate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/generate/internal/fixture/billing"
	"github.com/govengers/livewire/generate/internal/fixture/fixtureclient"
)

func newFixtureClient(t *testing.T, served *fixtureServer, key string) *fixtureclient.Client {
	t.Helper()
	created, err := fixtureclient.New(&client.Options{BaseURL: served.baseURL, APIKey: key, ReconnectDelay: 5 * time.Millisecond})
	require.NoError(t, err)
	return created
}

// requireRefusal requires err to be a refusal of status and code and answers its fields.
func requireRefusal(t *testing.T, err error, status int, code livewire.Code) map[string]string {
	t.Helper()
	var refusal *client.Error
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, status, refusal.Status, refusal.Error())
	require.Equal(t, code, refusal.Code)
	return refusal.Fields
}

func TestGoClientServesEveryRouteKind(t *testing.T) {
	served := startFixtureServer(t)
	user, admin := newFixtureClient(t, served, userKey), newFixtureClient(t, served, adminKey)
	ctx := t.Context()

	first, err := user.CreatePart(ctx, fixture.CreatePart{Name: "bolt", Kind: fixture.KindBolt}, nil)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), first.ID.Version(), "a ClientID create gets a UUID v7 (D18)")
	second, err := user.CreatePart(ctx, fixture.CreatePart{ID: newPartID(), Name: "nut", Kind: fixture.KindNut}, nil)
	require.NoError(t, err)

	page, err := user.GetParts(ctx, fixtureclient.PartsListQuery{Limit: 1, Sort: []string{"-createdAt"}})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, 2, page.Total)
	require.NotNil(t, page.Next, "cursor mode answers the next page's cursor (D38)")
	rest, err := user.GetParts(ctx, fixtureclient.PartsListQuery{Limit: 1, After: *page.Next})
	require.NoError(t, err)
	require.NotEqual(t, page.Items[0].ID, rest.Items[0].ID)

	filtered, err := user.GetParts(ctx, fixtureclient.PartsListQuery{Filter: &fixtureclient.PartsFilter{
		Where:  new(client.And(fixtureclient.PartsNameEqual("bolt"), client.Not(fixtureclient.PartsKindIn("nut")), fixtureclient.PartsCreatedAtGreater(client.Relative("-7d")))),
		Search: "bo",
	}})
	require.NoError(t, err, "the typed filter document parses on the server")
	require.Equal(t, 2, filtered.Total)
	_, err = user.GetParts(ctx, fixtureclient.PartsListQuery{Filter: &fixtureclient.PartsFilter{Where: new(client.Condition(fixtureclient.PartsField("extra"), "eq", "x"))}})
	requireRefusal(t, err, http.StatusBadRequest, livewire.CodeInvalidFilter)

	item, err := user.GetPartsItem(ctx, fixtureclient.PartsItemParameters{ID: first.ID})
	require.NoError(t, err)
	require.Equal(t, "bolt", item.Name)
	_, err = user.GetPartsItem(ctx, fixtureclient.PartsItemParameters{ID: newPartID()})
	requireRefusal(t, err, http.StatusNotFound, livewire.CodeNotFound)

	distinct, err := user.GetPartsDistinct(ctx, fixtureclient.PartsDistinctQuery{Field: "kind"})
	require.NoError(t, err)
	require.Equal(t, []livewire.DistinctValue{{Value: "bolt", Count: 1}, {Value: "nut", Count: 1}}, distinct)

	history, err := user.GetPartsHistory(ctx, fixtureclient.PartsHistoryQuery{ID: second.ID})
	require.NoError(t, err)
	require.Equal(t, "nut", history.Items[0].Data.Name)

	served.joinPresence(t, "parts", first.ID.String())
	present, err := user.GetPartsPresence(ctx, fixtureclient.PartsPresenceParameters{Key: first.ID.String()})
	require.NoError(t, err)
	require.Equal(t, "user", present.Items[0].Display)

	notes, err := user.GetNotes(ctx, fixtureclient.NotesListQuery{Sort: []string{"id"}})
	require.NoError(t, err)
	require.Equal(t, "note", notes.Items[0].Text)
	labels, err := user.GetLabels(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"red", "green"}, labels.Items)
	dashboard, err := user.GetDashboard(ctx, fixtureclient.DashboardParameters{Board: "main board"})
	require.NoError(t, err)
	require.Equal(t, "main board", dashboard.Title)
	_, err = user.GetDashboardPresence(ctx, fixtureclient.DashboardPresenceParameters{Board: "main", Key: "k"})
	require.NoError(t, err)

	renamed, err := user.RenamePart(ctx, fixtureclient.RenamePartParameters{ID: first.ID}, fixture.RenamePart{Name: "screw"}, nil)
	require.NoError(t, err)
	require.Equal(t, "screw", renamed.Name)
	charged, err := user.ChargePart(ctx, fixtureclient.ChargePartParameters{Invoice: "I-1"}, billing.Record{Body: billing.ErrorBody{Reason: "late"}}, nil)
	require.NoError(t, err)
	require.Equal(t, billing.Part{Invoice: "I-1:late"}, *charged)
	_, err = user.Rebuild(ctx, nil)
	requireRefusal(t, err, http.StatusForbidden, livewire.CodeForbidden)
	rebuilt, err := admin.Rebuild(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rebuilt.Count)

	attachment, err := user.UploadAttachment(ctx, fixtureclient.UploadAttachmentParameters{ID: first.ID}, livewire.File{Body: strings.NewReader("png!"), Size: 4, ContentType: "image/png"}, nil)
	require.NoError(t, err)
	require.Equal(t, fixture.Attachment{ID: first.ID, Size: 4, Type: "image/png"}, *attachment)
	_, err = user.UploadAttachment(ctx, fixtureclient.UploadAttachmentParameters{ID: first.ID}, livewire.File{Body: strings.NewReader("gif!"), Size: 4, ContentType: "image/gif"}, nil)
	requireRefusal(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest)

	var exported bytes.Buffer
	rows, err := user.ExportParts(ctx, fixtureclient.PartsExportQuery{}, &exported)
	require.NoError(t, err)
	require.Equal(t, 2, rows)
	require.Equal(t, 3, strings.Count(exported.String(), "\n"))
	served.backend.cutExport = true
	_, err = user.ExportParts(ctx, fixtureclient.PartsExportQuery{}, &exported)
	require.Error(t, err, "a cut export fails (D60)")
	served.backend.cutExport = false

	require.NoError(t, user.DeletePart(ctx, fixtureclient.DeletePartParameters{ID: second.ID}, nil))
	_, err = user.GetPartsItem(ctx, fixtureclient.PartsItemParameters{ID: second.ID})
	requireRefusal(t, err, http.StatusNotFound, livewire.CodeNotFound)
}

func TestGoClientBatchesAndStoresFilters(t *testing.T) {
	served := startFixtureServer(t)
	user := newFixtureClient(t, served, userKey)
	ctx := t.Context()
	create, err := fixtureclient.BatchCreatePart(fixture.CreatePart{Name: "batched"})
	require.NoError(t, err)
	var body fixture.CreatePart
	require.NoError(t, json.Unmarshal(create.Body, &body))
	require.Equal(t, uuid.Version(7), body.ID.Version(), "a batched create carries its client id")
	rename, err := fixtureclient.BatchRenamePart(fixtureclient.RenamePartParameters{ID: body.ID}, fixture.RenamePart{Name: "renamed"})
	require.NoError(t, err)
	remove, err := fixtureclient.BatchDeletePart(fixtureclient.DeletePartParameters{ID: newPartID()})
	require.NoError(t, err)
	results, err := user.Batch(ctx, []livewire.BatchCall{create, rename, remove}, nil)
	require.NoError(t, err)
	require.Equal(t, []int{http.StatusCreated, http.StatusOK, http.StatusNoContent}, []int{results[0].Status, results[1].Status, results[2].Status})
	item, err := user.GetPartsItem(ctx, fixtureclient.PartsItemParameters{ID: body.ID})
	require.NoError(t, err)
	require.Equal(t, "renamed", item.Name)
	rebuild, err := fixtureclient.BatchRebuild()
	require.NoError(t, err)
	_, err = user.Batch(ctx, []livewire.BatchCall{rebuild}, nil)
	fields := requireRefusal(t, err, http.StatusForbidden, livewire.CodeForbidden)
	require.Equal(t, "forbidden", fields["calls.0"], "a refusal names the failing call (D47)")
	charge, err := fixtureclient.BatchChargePart(fixtureclient.ChargePartParameters{Invoice: "I"}, billing.Record{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"invoice": "I"}, charge.Parameters)

	document, err := json.Marshal(fixtureclient.PartsFilter{Where: new(fixtureclient.PartsNameStartsWith("ren"))})
	require.NoError(t, err)
	named, err := user.SaveFilter(ctx, &livewire.FilterRequest{Resource: "parts", Name: "mine", Filter: document})
	require.NoError(t, err)
	listed, err := user.ListFilters(ctx, "parts")
	require.NoError(t, err)
	require.Equal(t, named.ID, listed.Items[0].ID)
	page, err := user.GetParts(ctx, fixtureclient.PartsListQuery{FilterID: named.ID})
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.NoError(t, user.DeleteFilter(ctx, named.ID))
	listed, err = user.ListFilters(ctx, "")
	require.NoError(t, err)
	require.Empty(t, listed.Items)
}

func TestGoClientRetriesACallUnderItsKey(t *testing.T) {
	served := startFixtureServer(t)
	user := newFixtureClient(t, served, userKey)
	ctx := t.Context()
	body, options := fixtureclient.PrepareCreatePart(fixture.CreatePart{Name: "once"})
	require.Equal(t, uuid.Version(7), body.ID.Version())
	first, err := user.CreatePart(ctx, body, &options)
	require.NoError(t, err)
	again, err := user.CreatePart(ctx, body, &options)
	require.NoError(t, err, "a retry answers the recorded response (D16)")
	require.Equal(t, first, again)
	page, err := user.GetParts(ctx, fixtureclient.PartsListQuery{})
	require.NoError(t, err)
	require.Equal(t, 1, page.Total, "the handler ran once")
	_, err = user.RenamePart(ctx, fixtureclient.RenamePartParameters{ID: body.ID}, fixture.RenamePart{Name: "other"}, &options)
	requireRefusal(t, err, http.StatusConflict, livewire.CodeConflict)
}

func TestGoClientRecoversAnEvictedFilter(t *testing.T) {
	served := startFixtureServer(t)
	user := newFixtureClient(t, served, userKey)
	ctx := t.Context()
	_, err := user.CreatePart(ctx, fixture.CreatePart{Name: "kept"}, nil)
	require.NoError(t, err)
	document, err := json.Marshal(fixtureclient.PartsFilter{Where: new(fixtureclient.PartsNameEqual("kept"))})
	require.NoError(t, err)
	unnamed, err := user.SaveFilter(ctx, &livewire.FilterRequest{Resource: "parts", Filter: document})
	require.NoError(t, err)
	served.filters.evict(unnamed.ID) // the store's cap replaced it (D54)

	page, err := user.GetParts(ctx, fixtureclient.PartsListQuery{FilterID: unnamed.ID})
	require.NoError(t, err, "the read sent the filter inline once")
	require.Equal(t, 1, page.Total)
	var exported bytes.Buffer
	_, err = user.ExportParts(ctx, fixtureclient.PartsExportQuery{FilterID: unnamed.ID}, &exported)
	require.NoError(t, err)

	pushes, err := user.SubscribeParts(ctx, fixtureclient.PartsListQuery{FilterID: unnamed.ID})
	require.NoError(t, err)
	snapshot := receivePush(t, pushes)
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind, "the subscribe registered the filter inline again")

	stranger := newFixtureClient(t, served, adminKey)
	_, err = stranger.GetParts(ctx, fixtureclient.PartsListQuery{FilterID: "filter-unknown"})
	requireRefusal(t, err, http.StatusNotFound, livewire.CodeNotFound)
}

func receivePush[Data any](t *testing.T, pushes <-chan client.Push[Data]) client.Push[Data] {
	t.Helper()
	select {
	case push, open := <-pushes:
		require.True(t, open, "the subscription ended")
		return push
	case <-time.After(10 * time.Second):
		t.Fatal("no push arrived")
		return client.Push[Data]{}
	}
}

func TestGoClientSubscribes(t *testing.T) {
	served := startFixtureServer(t)
	user := newFixtureClient(t, served, userKey)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first, err := user.CreatePart(ctx, fixture.CreatePart{Name: "first"}, nil)
	require.NoError(t, err)

	parts, err := user.SubscribeParts(ctx, fixtureclient.PartsListQuery{Filter: &fixtureclient.PartsFilter{Where: new(fixtureclient.PartsNameIsNull(false))}})
	require.NoError(t, err)
	snapshot := receivePush(t, parts)
	require.Equal(t, livewire.PushSnapshot, snapshot.Kind)
	require.Equal(t, []uuid.UUID{first.ID}, idsOf(snapshot.Data.Items))
	item, err := user.SubscribePartsItem(ctx, fixtureclient.PartsItemParameters{ID: first.ID})
	require.NoError(t, err)
	require.Equal(t, livewire.PushReplace, receivePush(t, item).Kind)
	dashboard, err := user.SubscribeDashboard(ctx, fixtureclient.DashboardParameters{Board: "b"})
	require.NoError(t, err)
	replace := receivePush(t, dashboard)
	require.Equal(t, client.Push[fixture.Dashboard]{Kind: livewire.PushReplace, Data: fixture.Dashboard{Title: "b"}}, replace)

	second, err := user.CreatePart(ctx, fixture.CreatePart{Name: "second"}, nil)
	require.NoError(t, err)
	diff := receivePush(t, parts)
	require.Equal(t, livewire.PushListChange, diff.Kind, "a write reaches the subscription as a diff")
	require.ElementsMatch(t, []uuid.UUID{first.ID, second.ID}, idsOf(diff.Data.Items), "the diff is applied")
	require.Equal(t, 2, diff.Data.Total)
	require.NotNil(t, diff.Change)

	served.keys.revoke(userKey)
	served.engine.RevalidateAll() // the identity is gone: the socket closes 4401
	var ended *client.EndedError
	for _, pushes := range []<-chan client.Push[livewire.ListResponse[fixture.Part]]{parts} {
		last := receivePush(t, pushes)
		for last.Kind != livewire.PushError {
			last = receivePush(t, pushes)
		}
		require.ErrorAs(t, last.Err, &ended)
		require.Equal(t, livewire.CloseIdentityGone, ended.CloseCode)
	}
	_, err = user.SubscribeParts(ctx, fixtureclient.PartsListQuery{})
	require.ErrorAs(t, err, &ended, "the live side ended for good")
}

func idsOf(parts []fixture.Part) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(parts))
	for _, part := range parts {
		ids = append(ids, part.ID)
	}
	return ids
}
