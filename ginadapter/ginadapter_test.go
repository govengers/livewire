package ginadapter_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/ginadapter"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const idempotencyKey = "0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b4c"

func jsonHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}}
}

func errorBody(t *testing.T, recorder *httptest.ResponseRecorder) livewire.ErrorBody {
	t.Helper()
	var body livewire.ErrorBody
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return body
}

func TestEveryReadRouteRunsTheEngineRead(t *testing.T) {
	server := newServer(t)
	testCases := []struct {
		name    string
		path    string
		subject string
		status  int
		body    string
	}{
		{"a list", "/api/parts", "rita", http.StatusOK, `{"limit":100,"offset":0,"total":1,"items":[{"id":"` + firstID + `","name":"bolt","version":1}]}`},
		{"a list as an admin", "/api/parts", "root", http.StatusOK, `{"limit":100,"offset":0,"total":1,"items":[{"id":"` + firstID + `","name":"bolt","secret":"s1","version":1}]}`},
		{"an item", "/api/parts/" + firstID, "rita", http.StatusOK, `{"id":"` + firstID + `","name":"bolt","version":1}`},
		{"a missing item", "/api/parts/" + secondID, "rita", http.StatusNotFound, `{"code":"not_found","message":"not found"}`},
		{"distinct", "/api/parts/distinct?field=name", "rita", http.StatusOK, `[{"value":"bolt","count":1}]`},
		{"history", "/api/parts/" + firstID + "/history", "rita", http.StatusOK, `{"limit":100,"offset":0,"total":1,"items":[{"id":1,"operation":"insert","validFrom":"1970-01-01T00:00:01Z","validTo":null,"actor":"","realActor":"","mutation":"","requestId":"","data":{"id":"` + firstID + `","name":"bolt","version":1}}]}`},
		{"presence of an item", "/api/parts/presence?key=" + firstID, "rita", http.StatusOK, `{"limit":0,"offset":0,"total":0,"items":[]}`},
		{"a typed list", "/api/stats?group=a", "rita", http.StatusOK, `{"limit":1,"offset":0,"total":1,"items":[{"id":"s1","group":"a"}]}`},
		{"an object", "/api/teams/blue/board", "rita", http.StatusOK, `{"team":"blue"}`},
		{"presence of an object", "/api/teams/blue/board/presence?key=k", "rita", http.StatusOK, `{"limit":0,"offset":0,"total":0,"items":[]}`},
		{"a bad filter", "/api/parts?filter=" + url.QueryEscape(`{"where":{"field":"nothing"}}`), "rita", http.StatusBadRequest, ""},
		{"no identity", "/api/parts", "", http.StatusUnauthorized, `{"code":"unauthenticated","message":"authentication required"}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := server.do(http.MethodGet, testCase.path, testCase.subject, nil, nil)
			require.Equal(t, testCase.status, recorder.Code, recorder.Body.String())
			require.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
			if testCase.body != "" {
				require.JSONEq(t, testCase.body, recorder.Body.String())
			}
		})
	}
	event := server.lastEvent()
	require.Equal(t, "parts", event["livewire.target"])
	require.Equal(t, "read", event["livewire.kind"])
	require.Equal(t, http.StatusUnauthorized, event["livewire.status"])
	require.Equal(t, "unauthenticated", event["livewire.code"])
	require.Equal(t, wideevent.OutcomeFailure, event["outcome"])
}

func TestCursorPagesAnswerNext(t *testing.T) {
	server := newServer(t)
	server.api.put(part{ID: secondID, Name: "nut"})
	first := server.do(http.MethodGet, "/api/parts?limit=1", "rita", nil, nil)
	require.Equal(t, http.StatusOK, first.Code)
	var page livewire.ListResponse[part]
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &page))
	require.NotNil(t, page.Next)
	second := server.do(http.MethodGet, "/api/parts?limit=1&after="+url.QueryEscape(*page.Next), "rita", nil, nil)
	page = livewire.ListResponse[part]{}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &page), second.Body.String())
	require.Equal(t, secondID, page.Items[0].ID)
	require.Nil(t, page.Next)
}

func TestRESTBodiesEqualTheSocketPayloads(t *testing.T) {
	server := newServer(t)
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	header := http.Header{subjectHeader: {"ada"}}
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http")+"/socket", header)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	t.Cleanup(func() { _ = connection.Close() }) // the engine closes it at the cleanup; the error carries nothing
	ask := func(frame livewire.RequestFrame) livewire.ResponseFrame {
		t.Helper()
		require.NoError(t, connection.WriteJSON(frame))
		require.NoError(t, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
		var answer livewire.ResponseFrame
		require.NoError(t, connection.ReadJSON(&answer))
		return answer
	}

	for _, read := range []struct {
		path   string
		target string
		params map[string]string
	}{
		{"/api/parts?sort=-name", "parts", map[string]string{"sort": "-name"}},
		{"/api/parts/" + firstID, "parts.item", map[string]string{"id": firstID}},
		{"/api/parts/" + firstID + "/history", "parts.history", map[string]string{"id": firstID}},
		{"/api/stats?group=a", "stats", map[string]string{"group": "a"}},
	} {
		rest := server.do(http.MethodGet, read.path, "ada", nil, nil)
		socket := ask(livewire.RequestFrame{ID: read.target, Operation: livewire.OperationQuery, Target: read.target, Parameters: read.params})
		require.True(t, socket.OK, "%+v", socket.Error)
		require.Equal(t, rest.Body.String(), string(socket.Payload), "REST and socket answer the same bytes (D23): %s", read.target)
	}

	rest := server.do(http.MethodPost, "/api/parts", "ada", strings.NewReader(`{"name":"nut"}`), http.Header{ginadapter.IdempotencyKeyHeader: {idempotencyKey}})
	require.Equal(t, http.StatusCreated, rest.Code)
	socket := ask(livewire.RequestFrame{ID: "call", Operation: livewire.OperationCall, Target: "createPart", Body: json.RawMessage(`{"name":"nut"}`), IdempotencyKey: idempotencyKey})
	require.Equal(t, http.StatusCreated, socket.Status)
	require.Equal(t, rest.Body.String(), string(socket.Payload), "a call answers the same bytes, the replay included")
	require.Equal(t, int64(1), server.api.created.Load())
}

func TestMutationRoutes(t *testing.T) {
	server := newServer(t)
	keyed := http.Header{ginadapter.IdempotencyKeyHeader: {idempotencyKey}}
	created := server.do(http.MethodPost, "/api/parts", "ada", strings.NewReader(`{"name":"nut"}`), keyed)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	require.JSONEq(t, `{"id":"`+secondID+`","name":"nut","secret":"","version":0}`, created.Body.String())
	replayed := server.do(http.MethodPost, "/api/parts", "ada", strings.NewReader(`{"name":"nut"}`), keyed)
	require.Equal(t, created.Body.String(), replayed.Body.String(), "the key replays over REST (D16)")
	require.Equal(t, int64(1), server.api.created.Load())
	event := server.lastEvent()
	require.Equal(t, "createPart", event["livewire.target"])
	require.Equal(t, "mutation", event["livewire.kind"])
	require.Equal(t, wideevent.OutcomeSuccess, event["outcome"])

	removed := server.do(http.MethodDelete, "/api/parts/"+secondID, "ada", nil, nil)
	require.Equal(t, http.StatusNoContent, removed.Code)
	require.Empty(t, removed.Body.String())

	unknown := server.do(http.MethodPost, "/api/parts", "ada", strings.NewReader(`{"name":"nut","color":"red"}`), nil)
	require.Equal(t, http.StatusBadRequest, unknown.Code)
	require.Contains(t, errorBody(t, unknown).Fields, "color")
	large := server.do(http.MethodPost, "/api/parts", "ada", strings.NewReader(`{"name":"`+strings.Repeat("x", 2048)+`"}`), nil)
	require.Equal(t, http.StatusBadRequest, large.Code)
	require.Equal(t, "larger than 1024 bytes", errorBody(t, large).Fields["body"])

	body := &countingReader{reader: strings.NewReader(`{"name":"nut"}`)}
	refused := server.do(http.MethodPost, "/api/parts", "rita", body, nil)
	require.Equal(t, http.StatusForbidden, refused.Code)
	require.Zero(t, body.read, "a refused call reads no body (D8)")
	require.Equal(t, http.StatusConflict, server.do(http.MethodPost, "/api/parts/fail", "ada", nil, nil).Code)
}

// countingReader counts the bytes read from it.
type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	read, err := c.reader.Read(buffer)
	c.read += read
	return read, err
}

func TestBatchRouteIsAllOrNothing(t *testing.T) {
	server := newServer(t)
	failing := `[{"target":"createPart","body":{"name":"nut"}},{"target":"failPart"}]`
	refused := server.do(http.MethodPost, "/api/batch", "ada", strings.NewReader(failing), nil)
	require.Equal(t, http.StatusConflict, refused.Code)
	require.Equal(t, "conflict", errorBody(t, refused).Fields["calls.1"])
	require.Len(t, server.api.sorted(), 1, "the first call rolled back with the second")

	succeeding := `[{"target":"createPart","body":{"name":"nut"}},{"target":"removePart","params":{"id":"` + firstID + `"}}]`
	committed := server.do(http.MethodPost, "/api/batch", "ada", strings.NewReader(succeeding), http.Header{ginadapter.IdempotencyKeyHeader: {idempotencyKey}})
	require.Equal(t, http.StatusOK, committed.Code, committed.Body.String())
	require.JSONEq(t, `[{"status":201,"payload":{"id":"`+secondID+`","name":"nut","secret":"","version":0}},{"status":204}]`, committed.Body.String())
	require.Equal(t, []part{{ID: secondID, Name: "nut"}}, server.api.sorted())
	require.Equal(t, "batch", server.lastEvent()["livewire.target"])
	require.Equal(t, http.StatusBadRequest, server.do(http.MethodPost, "/api/batch", "ada", strings.NewReader(strings.Repeat(" ", 2048)), nil).Code)
}

func TestFilterRoutes(t *testing.T) {
	server := newServer(t)
	saved := server.do(http.MethodPost, "/api/filters", "rita", strings.NewReader(`{"resource":"parts","name":"Bolts","filter":{"where":{"field":"name","op":"eq","value":"bolt"}}}`), jsonHeader())
	require.Equal(t, http.StatusCreated, saved.Code, saved.Body.String())
	var filter livewire.FilterItem
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &filter))

	listed := server.do(http.MethodGet, "/api/filters?resource=parts", "rita", nil, nil)
	require.Equal(t, http.StatusOK, listed.Code)
	require.Contains(t, listed.Body.String(), filter.ID)
	read := server.do(http.MethodGet, "/api/parts?filterId="+filter.ID, "rita", nil, nil)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())

	require.Equal(t, http.StatusNoContent, server.do(http.MethodDelete, "/api/filters/"+filter.ID, "rita", nil, nil).Code)
	require.Equal(t, "filters", server.lastEvent()["livewire.kind"])
	require.Equal(t, http.StatusNotFound, server.do(http.MethodGet, "/api/parts?filterId="+filter.ID, "rita", nil, nil).Code)
	require.Equal(t, http.StatusBadRequest, server.do(http.MethodPost, "/api/filters", "rita", strings.NewReader(strings.Repeat(" ", 2048)), nil).Code)
}

func TestUploadRoute(t *testing.T) {
	server := newServer(t)
	path := "/api/parts/" + firstID + "/pictures"
	raw := server.do(http.MethodPost, path, "ada", strings.NewReader("png-bytes"), http.Header{"Content-Type": {"image/png; charset=binary"}})
	require.Equal(t, http.StatusOK, raw.Code, raw.Body.String())
	require.JSONEq(t, `{"part":"`+firstID+`","bytes":9,"contentType":"image/png","filename":""}`, raw.Body.String())

	multipartBody, contentType := multipartFile(t, "file", "a.png", "image/png", "png-bytes")
	uploaded := server.do(http.MethodPost, path, "ada", multipartBody, http.Header{"Content-Type": {contentType}})
	require.Equal(t, http.StatusOK, uploaded.Code, uploaded.Body.String())
	require.JSONEq(t, `{"part":"`+firstID+`","bytes":9,"contentType":"image/png","filename":"a.png"}`, uploaded.Body.String())

	chunked := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, io.MultiReader(strings.NewReader("png")))
	chunked.ContentLength = -1
	chunked.Header.Set("Content-Type", "image/png")
	chunked.Header.Set(subjectHeader, "ada")
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, chunked)
	require.Equal(t, http.StatusOK, recorder.Code, "a body without a length is read within the limit")

	tooLarge, tooLargeType := multipartFile(t, "file", "a.png", "image/png", strings.Repeat("x", 17))
	unnamed, unnamedType := multipartFile(t, "picture", "a.png", "image/png", "png")
	untyped, untypedType := multipartFile(t, "file", "a.png", "", "png")
	refusals := []struct {
		name        string
		subject     string
		body        io.Reader
		contentType string
		status      int
	}{
		{"a raw type not declared", "ada", strings.NewReader("text"), "text/plain", http.StatusBadRequest},
		{"a raw body past the limit", "ada", strings.NewReader(strings.Repeat("x", 17)), "image/png", http.StatusBadRequest},
		{"a multipart file past the limit", "ada", tooLarge, tooLargeType, http.StatusBadRequest},
		{"a multipart part of another name", "ada", unnamed, unnamedType, http.StatusBadRequest},
		{"a multipart part without a type", "ada", untyped, untypedType, http.StatusBadRequest},
		{"the static check", "rita", strings.NewReader("png"), "image/png", http.StatusForbidden},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			response := server.do(http.MethodPost, path, refusal.subject, refusal.body, http.Header{"Content-Type": {refusal.contentType}})
			require.Equal(t, refusal.status, response.Code, response.Body.String())
		})
	}
}

func multipartFile(t *testing.T, field, filename, contentType, content string) (io.Reader, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := map[string][]string{"Content-Disposition": {`form-data; name="` + field + `"; filename="` + filename + `"`}}
	if contentType != "" {
		header["Content-Type"] = []string{contentType}
	}
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return &body, writer.FormDataContentType()
}

func TestExportRoute(t *testing.T) {
	server := newServer(t)
	exported := server.do(http.MethodGet, "/api/parts/export?format=csv", "rita", nil, nil)
	require.Equal(t, http.StatusOK, exported.Code)
	require.Equal(t, "text/csv; charset=utf-8", exported.Header().Get("Content-Type"))
	require.Equal(t, `attachment; filename=parts.csv`, exported.Header().Get("Content-Disposition"))
	require.Equal(t, "id,name\n"+firstID+",bolt\n", exported.Body.String(), "the guarded secret is no column")
	require.Equal(t, 1, server.lastEvent()["livewire.rows"])
	require.Equal(t, "id,name,secret\n"+firstID+",bolt,s1\n", server.do(http.MethodGet, "/api/parts/export", "root", nil, nil).Body.String())

	refused := server.do(http.MethodGet, "/api/parts/export?format=xlsx", "rita", nil, nil)
	require.Equal(t, http.StatusBadRequest, refused.Code, "a refusal before the first byte is an error body")
	require.Contains(t, errorBody(t, refused).Fields, "format")

	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpServer.URL+"/api/parts/export?filter="+url.QueryEscape(`{"where":{"field":"name","op":"eq","value":"fail"}}`), nil)
	require.NoError(t, err)
	request.Header.Set(subjectHeader, "rita")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_, err = io.ReadAll(response.Body)
	require.Error(t, err, "a failure after the first byte breaks the body")
	require.NoError(t, response.Body.Close())
	require.Eventually(t, func() bool { return server.lastEvent()["outcome"] == wideevent.OutcomeFailure }, 5*time.Second, time.Millisecond)
	http.DefaultClient.CloseIdleConnections()
}

func TestRegisterRoutesValidates(t *testing.T) {
	server := newServer(t)
	require.Error(t, ginadapter.RegisterRoutes(gin.New(), nil))
	require.Error(t, ginadapter.RegisterRoutes(nil, &ginadapter.Options{Engine: server.engine}))
	require.Error(t, ginadapter.RegisterRoutes(gin.New(), &ginadapter.Options{}))
	require.Error(t, ginadapter.RegisterRoutes(gin.New(), &ginadapter.Options{Engine: server.engine, MaximumBodyBytes: -1}))
	taken := gin.New()
	taken.GET("/api/parts", func(*gin.Context) {})
	err := ginadapter.RegisterRoutes(taken, &ginadapter.Options{Engine: server.engine})
	require.ErrorContains(t, err, "GET /api/parts")
}
