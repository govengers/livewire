package client_test

import (
	"bytes"
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
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/client"
	"github.com/govengers/livewire/query"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const apiKey = "key-1"

// recorded is one request the fake REST server saw.
type recorded struct {
	method        string
	path          string
	query         string
	authorization string
	key           string
	contentType   string
	length        int64
	body          string
}

// restServer answers every request with handler and records it.
type restServer struct {
	mutex    sync.Mutex
	requests []recorded
}

func (s *restServer) last() recorded {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.requests[len(s.requests)-1]
}

func (s *restServer) count() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.requests)
}

func newRESTClient(t *testing.T, handler http.HandlerFunc) (*client.Client, *restServer) {
	t.Helper()
	recorder := &restServer{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		recorder.mutex.Lock()
		recorder.requests = append(recorder.requests, recorded{
			method: request.Method, path: request.URL.EscapedPath(), query: request.URL.RawQuery, authorization: request.Header.Get("Authorization"),
			key: request.Header.Get(client.IdempotencyKeyHeader), contentType: request.Header.Get("Content-Type"), length: request.ContentLength, body: string(body),
		})
		recorder.mutex.Unlock()
		handler(writer, request)
	}))
	t.Cleanup(server.Close)
	created, err := client.New(&client.Options{BaseURL: server.URL + "/api/", APIKey: apiKey})
	require.NoError(t, err)
	return created, recorder
}

func answer(status int, body string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}
}

func TestOptionsAreValidatedAndCopied(t *testing.T) {
	testCases := []struct {
		name    string
		options *client.Options
		message string
	}{
		{"nil", nil, "needs options"},
		{"no base URL", &client.Options{APIKey: apiKey}, "no http or https URL"},
		{"a relative base URL", &client.Options{BaseURL: "/api", APIKey: apiKey}, "no http or https URL"},
		{"no API key", &client.Options{BaseURL: "https://example.com"}, "APIKey is required"},
		{"a key of two lines", &client.Options{BaseURL: "https://example.com", APIKey: "a\nb"}, "one line"},
		{"a negative duration", &client.Options{BaseURL: "https://example.com", APIKey: apiKey, RequestTimeout: -1}, "negative"},
		{"a socket URL of http", &client.Options{BaseURL: "https://example.com", APIKey: apiKey, SocketURL: "https://example.com/socket"}, "no ws or wss URL"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := client.New(testCase.options)
			require.ErrorContains(t, err, testCase.message)
		})
	}
	options := &client.Options{BaseURL: "https://example.com/api", APIKey: apiKey}
	before := *options
	_, err := client.New(options)
	require.NoError(t, err)
	require.Equal(t, before, *options, "New does not write the caller's options")
}

func TestReadSendsTheKeyAndTheParameters(t *testing.T) {
	created, server := newRESTClient(t, answer(http.StatusOK, `{"limit":10,"offset":0,"total":1,"items":[{"id":"a"}]}`))
	parameters := client.Parameters{}
	parameters.SetPath("board", "a b/c")
	parameters.Set("tag", []string{"x", "y"})
	parameters.Set("empty", "")
	var data livewire.ListResponse[map[string]string]
	require.NoError(t, created.Read(t.Context(), "/boards/{board}/stats", parameters, &data))
	require.Equal(t, 1, data.Total)
	request := server.last()
	require.Equal(t, recorded{method: http.MethodGet, path: "/api/boards/a%20b%2Fc/stats", query: "tag=x%2Cy", authorization: "Bearer " + apiKey}, request)
	require.ErrorContains(t, created.Read(t.Context(), "/boards/{board}/stats", client.Parameters{}, &data), "the path parameter board is missing")
}

func TestRefusalsAreErrors(t *testing.T) {
	testCases := []struct {
		name     string
		status   int
		body     string
		expected client.Error
	}{
		{
			"a livewire body", http.StatusBadRequest, `{"code":"invalid_filter","message":"the filter is invalid","fields":{"where":"unknown"}}`,
			client.Error{Status: 400, Code: livewire.CodeInvalidFilter, Message: "the filter is invalid", Fields: map[string]string{"where": "unknown"}},
		},
		{"a proxy's page", http.StatusBadGateway, `<html>bad gateway</html>`, client.Error{Status: 502, Code: livewire.CodeInternal, Message: "internal"}},
		{"an unknown code", http.StatusNotFound, `{"code":"gone"}`, client.Error{Status: 404, Code: livewire.CodeNotFound, Message: "not_found"}},
		{"unauthenticated", http.StatusUnauthorized, ``, client.Error{Status: 401, Code: livewire.CodeUnauthenticated, Message: "unauthenticated"}},
		{"forbidden", http.StatusForbidden, ``, client.Error{Status: 403, Code: livewire.CodeForbidden, Message: "forbidden"}},
		{"a conflict", http.StatusConflict, ``, client.Error{Status: 409, Code: livewire.CodeConflict, Message: "conflict"}},
		{"a bad request", http.StatusBadRequest, ``, client.Error{Status: 400, Code: livewire.CodeInvalidRequest, Message: "invalid_request"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			created, _ := newRESTClient(t, answer(testCase.status, testCase.body))
			err := created.Read(t.Context(), "/parts", nil, nil)
			var refusal *client.Error
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, testCase.expected, *refusal)
			require.Contains(t, refusal.Error(), testCase.expected.Message)
		})
	}
}

func TestTransportFailuresWrapTheCause(t *testing.T) {
	created, err := client.New(&client.Options{BaseURL: "http://127.0.0.1:1", APIKey: apiKey})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = created.Read(ctx, "/parts", nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	var refusal *client.Error
	require.NotErrorAs(t, err, &refusal)
}

func TestAnUndecodableAnswerFails(t *testing.T) {
	created, _ := newRESTClient(t, answer(http.StatusOK, `{"total":"x"}`))
	var data livewire.ListResponse[string]
	require.ErrorContains(t, created.Read(t.Context(), "/parts", nil, &data), "does not decode")
}

func TestCallsCarryTheirIdempotencyKey(t *testing.T) {
	created, server := newRESTClient(t, answer(http.StatusCreated, `{"id":"a"}`))
	var response map[string]string
	require.NoError(t, created.Call(t.Context(), &client.Call{Method: http.MethodPost, Path: "/parts", Body: map[string]string{"name": "bolt"}}, &response))
	require.Equal(t, "a", response["id"])
	first := server.last()
	require.Equal(t, `{"name":"bolt"}`, first.body)
	require.Equal(t, "application/json", first.contentType)
	key, err := uuid.Parse(first.key)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), key.Version())

	require.NoError(t, created.Call(t.Context(), &client.Call{Method: http.MethodDelete, Path: "/parts/{id}", Parameters: client.Parameters{"id": "a"}, IdempotencyKey: "fixed"}, nil))
	require.Equal(t, recorded{method: http.MethodDelete, path: "/api/parts/a", authorization: "Bearer " + apiKey, key: "fixed"}, server.last())
	require.ErrorContains(t, created.Call(t.Context(), &client.Call{Method: http.MethodPost, Path: "/parts", Body: func() {}}, nil), "the body of POST /parts")
	require.ErrorContains(t, created.Call(t.Context(), &client.Call{Method: "BAD METHOD", Path: "/parts"}, nil), "BAD METHOD")
}

func TestUploadsSendARawBodyWithItsLength(t *testing.T) {
	created, server := newRESTClient(t, answer(http.StatusCreated, `{"size":5}`))
	file := livewire.File{Body: strings.NewReader("hello"), Size: 5, ContentType: "image/png"}
	var response map[string]int
	require.NoError(t, created.Upload(t.Context(), &client.Upload{Path: "/parts/{id}/attachments", Parameters: client.Parameters{"id": "a"}, File: file, IdempotencyKey: "upload-1"}, &response))
	require.Equal(t, 5, response["size"])
	require.Equal(t, recorded{method: http.MethodPost, path: "/api/parts/a/attachments", authorization: "Bearer " + apiKey, key: "upload-1", contentType: "image/png", length: 5, body: "hello"}, server.last())
	require.ErrorContains(t, created.Upload(t.Context(), &client.Upload{Path: "/x"}, nil), "needs a body")
}

func TestBatchAndStoredFilters(t *testing.T) {
	created, server := newRESTClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/batch":
			answer(http.StatusOK, `[{"status":201,"payload":{"id":"a"}}]`)(writer, request)
		case request.Method == http.MethodPost:
			answer(http.StatusCreated, `{"id":"filter-1","resource":"parts","filter":{}}`)(writer, request)
		case request.Method == http.MethodGet:
			answer(http.StatusOK, `{"limit":0,"offset":0,"total":0,"items":[]}`)(writer, request)
		default:
			writer.WriteHeader(http.StatusNoContent)
		}
	})
	results, err := created.Batch(t.Context(), []livewire.BatchCall{{Target: "createPart", Body: json.RawMessage(`{}`)}}, &client.CallOptions{IdempotencyKey: "batch-1"})
	require.NoError(t, err)
	require.Equal(t, []livewire.BatchResult{{Status: 201, Payload: json.RawMessage(`{"id":"a"}`)}}, results)
	require.Equal(t, "batch-1", server.last().key)
	require.Equal(t, `[{"target":"createPart","body":{}}]`, server.last().body)

	stored, err := created.SaveFilter(t.Context(), &livewire.FilterRequest{Resource: "parts", Filter: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.Equal(t, "filter-1", stored.ID)
	listed, err := created.ListFilters(t.Context(), "parts")
	require.NoError(t, err)
	require.Empty(t, listed.Items)
	require.Equal(t, "resource=parts", server.last().query)
	require.NoError(t, created.DeleteFilter(t.Context(), "filter-1"))
	require.Equal(t, "/api/filters/filter-1", server.last().path)
}

// evictingServer answers 404 to a read with filterId and the list to one with an inline filter.
func evictingServer(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodPost:
		answer(http.StatusCreated, `{"id":"filter-1","resource":"parts","filter":{"where":{"field":"name","op":"eq","value":"a"}}}`)(writer, request)
	case request.URL.Query().Has(livewire.ParameterFilterID):
		answer(http.StatusNotFound, `{"code":"not_found","message":"not found"}`)(writer, request)
	case strings.HasSuffix(request.URL.Path, "/export"):
		writer.Header().Set("Trailer", client.ExportRowsTrailer)
		_, _ = io.WriteString(writer, "id\na\n")
		writer.Header().Set(client.ExportRowsTrailer, "1")
	default:
		answer(http.StatusOK, `{"limit":10,"offset":0,"total":1,"items":[]}`)(writer, request)
	}
}

func TestAnEvictedUnnamedFilterIsSentInlineOnce(t *testing.T) {
	created, server := newRESTClient(t, evictingServer)
	document := json.RawMessage(`{"where":{"field":"name","op":"eq","value":"a"}}`)
	var data livewire.ListResponse[string]
	err := created.Read(t.Context(), "/parts", client.Parameters{livewire.ParameterFilterID: "filter-1"}, &data)
	require.ErrorContains(t, err, "not found", "a filterId the client does not know is not recovered")
	_, err = created.SaveFilter(t.Context(), &livewire.FilterRequest{Resource: "parts", Filter: document})
	require.NoError(t, err)
	before := server.count()
	require.NoError(t, created.Read(t.Context(), "/parts", client.Parameters{livewire.ParameterFilterID: "filter-1", "limit": "5"}, &data))
	require.Equal(t, before+2, server.count())
	require.Equal(t, "filter=%7B%22where%22%3A%7B%22field%22%3A%22name%22%2C%22op%22%3A%22eq%22%2C%22value%22%3A%22a%22%7D%7D&limit=5", server.last().query)

	var exported bytes.Buffer
	rows, err := created.Export(t.Context(), "/parts/export", client.Parameters{livewire.ParameterFilterID: "filter-1"}, &exported)
	require.NoError(t, err)
	require.Equal(t, 1, rows)
	require.Equal(t, "id\na\n", exported.String())

	require.NoError(t, created.DeleteFilter(t.Context(), "filter-1"))
	err = created.Read(t.Context(), "/parts", client.Parameters{livewire.ParameterFilterID: "filter-1"}, &data)
	require.ErrorContains(t, err, "not found", "a deleted filter is forgotten")
}

func TestACutExportFails(t *testing.T) {
	created, _ := newRESTClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Trailer", client.ExportRowsTrailer)
		_, _ = io.WriteString(writer, "id\na\n") // ends without the trailer: the export broke off
	})
	var exported bytes.Buffer
	_, err := created.Export(t.Context(), "/parts/export", nil, &exported)
	require.ErrorContains(t, err, "without its row count")

	broken, _ := newRESTClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(writer, "id\n") // the connection closes short of the length
	})
	_, err = broken.Export(t.Context(), "/parts/export", nil, &exported)
	require.ErrorContains(t, err, "cut off")

	refused, _ := newRESTClient(t, answer(http.StatusForbidden, `{"code":"forbidden","message":"forbidden"}`))
	_, err = refused.Export(t.Context(), "/parts/export", nil, &exported)
	var refusal *client.Error
	require.ErrorAs(t, err, &refusal)
}

func TestParametersTakeTheirWireForm(t *testing.T) {
	instant := time.Date(2026, 11, 1, 12, 0, 0, 5, time.UTC)
	id := uuid.MustParse("0190a7a4-3e5f-7c3b-9b1e-2f6d1c2a3b41")
	text, yes, number, wide := "a", true, 3, int64(4)
	parameters := client.Parameters{}
	for name, value := range map[string]any{
		"string": "s", "bool": true, "int": 1, "int64": int64(2), "uuid": id, "time": instant, "list": []string{"a", "b"},
		"stringPointer": &text, "boolPointer": &yes, "intPointer": &number, "int64Pointer": &wide, "uuidPointer": &id, "timePointer": &instant,
		"emptyString": "", "false": false, "zero": 0, "zero64": int64(0), "nilUUID": uuid.Nil, "zeroTime": time.Time{}, "emptyList": []string{},
		"nilString": (*string)(nil), "nilBool": (*bool)(nil), "nilInt": (*int)(nil), "nilInt64": (*int64)(nil), "nilUUIDPointer": (*uuid.UUID)(nil), "nilTime": (*time.Time)(nil),
	} {
		parameters.Set(name, value)
	}
	require.Equal(t, client.Parameters{
		"string": "s", "bool": "true", "int": "1", "int64": "2", "uuid": id.String(), "time": "2026-11-01T12:00:00.000000005Z", "list": "a,b",
		"stringPointer": "a", "boolPointer": "true", "intPointer": "3", "int64Pointer": "4", "uuidPointer": id.String(), "timePointer": "2026-11-01T12:00:00.000000005Z",
	}, parameters)
	parameters.SetPath("zero", 0)
	require.Equal(t, "0", parameters["zero"])
	require.NoError(t, parameters.SetJSON("filter", map[string]int{"a": 1}))
	require.JSONEq(t, `{"a":1}`, parameters["filter"])
	require.Error(t, parameters.SetJSON("filter", func() {}))
	require.Panics(t, func() { parameters.Set("float", 1.5) })
}

type field string

func TestFilterDocumentsMarshalAsTheServerReadsThem(t *testing.T) {
	instant := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	node := client.And(
		client.Condition(field("name"), query.OperatorEqual, "bolt"),
		client.Or(client.Condition(field("createdAt"), query.OperatorGreater, client.Relative("-7d")), client.Condition(field("createdAt"), query.OperatorLess, client.At(instant))),
		client.Not(client.Condition(field("tags"), query.OperatorIsEmpty, true)),
		client.And[field](),
	)
	encoded, err := json.Marshal(node)
	require.NoError(t, err)
	require.JSONEq(t, `{"and":[
		{"field":"name","op":"eq","value":"bolt"},
		{"or":[{"field":"createdAt","op":"gt","value":{"relative":"-7d"}},{"field":"createdAt","op":"lt","value":"2026-11-01T12:00:00Z"}]},
		{"not":{"field":"tags","op":"isEmpty","value":true}},
		{"and":[]}
	]}`, string(encoded))
	parsed, err := query.Parse(query.Fields{
		{Name: "name", Type: query.TypeText, SQL: "name", Filter: true},
		{Name: "createdAt", Type: query.TypeTime, SQL: "created_at", Filter: true},
		{Name: "tags", Type: query.TypeStringArray, SQL: "tags", Filter: true},
	}, []byte(`{"where":`+strings.Replace(string(encoded), `,{"and":[]}`, "", 1)+`}`))
	require.NoError(t, err, "the query package parses the document")
	require.True(t, parsed.IsTimeDependent())
}

func TestTheKeysAndIDsAreFreshUUIDs(t *testing.T) {
	require.NotEqual(t, client.NewIdempotencyKey(), client.NewIdempotencyKey())
	require.Equal(t, uuid.Version(7), client.NewUUIDv7().Version())
	require.Equal(t, uuid.Version(4), client.NewUUIDv4().Version())
	var ended *client.EndedError
	require.ErrorAs(t, error(&client.EndedError{CloseCode: 4401}), &ended)
	require.Contains(t, ended.Error(), "4401")
	require.False(t, errors.Is(ended, context.Canceled))
}
