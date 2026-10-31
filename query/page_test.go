package query_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/query"
)

func TestParseSort(t *testing.T) {
	terms, err := query.ParseSort(testFields, "-seenAt, name")
	require.NoError(t, err)
	require.Equal(t, []query.SortTerm{{Field: "seenAt", Descending: true}, {Field: "name"}}, terms)

	terms, err = query.ParseSort(testFields, " ")
	require.NoError(t, err)
	require.Nil(t, terms)

	testCases := []struct{ raw, path string }{
		{"nope", "sort[0]"},
		{"tags", "sort[0]"},
		{"data", "sort[0]"},
		{"name,name", "sort[1]"},
		{"-", "sort[0]"},
		{"port,,name", "sort[1]"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.raw, func(t *testing.T) {
			_, err := query.ParseSort(testFields, testCase.raw)
			fields := requireQueryError(t, err, query.CodeInvalidSort)
			require.Contains(t, fields, testCase.path)
		})
	}
}

func TestParsePage(t *testing.T) {
	testCases := []struct {
		name          string
		limit, offset string
		want          query.Page
		wantPath      string
	}{
		{name: "defaults", want: query.Page{Limit: 500}},
		{name: "values", limit: "20", offset: "40", want: query.Page{Limit: 20, Offset: 40}},
		{name: "zero limit", limit: "0", want: query.Page{Limit: 0}},
		{name: "clamped", limit: "9999", want: query.Page{Limit: 500}},
		{name: "negative limit", limit: "-1", wantPath: "limit"},
		{name: "negative offset", offset: "-1", wantPath: "offset"},
		{name: "unparsable limit", limit: "ten", wantPath: "limit"},
		{name: "fractional offset", offset: "1.5", wantPath: "offset"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			page, err := query.ParsePage(testCase.limit, testCase.offset, 500)
			if testCase.wantPath != "" {
				fields := requireQueryError(t, err, query.CodeInvalidPage)
				require.Contains(t, fields, testCase.wantPath)
				return
			}
			require.NoError(t, err)
			require.Equal(t, testCase.want, page)
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	seenAt := time.Date(2026, 9, 23, 10, 0, 0, 500, time.UTC)
	sort := []query.SortTerm{{Field: "seenAt", Descending: true}, {Field: "port"}, {Field: "name"}, {Field: "load"}, {Field: "status"}, {Field: "groupId"}, {Field: "note"}}
	original := &query.Cursor{
		Sort:   sort,
		Values: []any{seenAt, nil, "widget", 0.5, "online", group, strings.Repeat("x", 5000)},
		ID:     alpha,
	}
	encoded, err := original.Encode()
	require.NoError(t, err)
	require.NotContains(t, encoded, "=")

	decoded, err := query.ParseCursor(testFields, encoded)
	require.NoError(t, err)
	require.Equal(t, original, decoded)

	listQuery, err := query.Compile(testFields, nil, sort, query.Page{Limit: 10, After: decoded}, nil)
	require.NoError(t, err)
	require.Equal(t, decoded, listQuery.Cursor)
}

func TestCursorValuesAreColumnContents(t *testing.T) {
	// The driver's types and values beyond the document limits round-trip: int32, a uuid as
	// [16]byte, an integer above 2^53, a time with an offset, an enum value not declared.
	var groupBytes [16]byte
	groupBytes[15] = 0x0a
	original := &query.Cursor{
		Sort:   []query.SortTerm{{Field: "port"}, {Field: "groupId"}, {Field: "load"}, {Field: "seenAt"}, {Field: "status"}},
		Values: []any{int32(8080), groupBytes, float32(0.5), time.Date(2026, 9, 23, 12, 0, 0, 0, time.FixedZone("CEST", 7200)), "retired"},
		ID:     beta,
	}
	encoded, err := original.Encode()
	require.NoError(t, err)
	decoded, err := query.ParseCursor(testFields, encoded)
	require.NoError(t, err)
	require.Equal(t, []any{int64(8080), group, 0.5, time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC), "retired"}, decoded.Values)

	big := &query.Cursor{Sort: []query.SortTerm{{Field: "port"}}, Values: []any{int64(9007199254740993)}, ID: beta}
	encoded, err = big.Encode()
	require.NoError(t, err)
	decoded, err = query.ParseCursor(testFields, encoded)
	require.NoError(t, err)
	require.Equal(t, []any{int64(9007199254740993)}, decoded.Values)
}

func TestCursorNonFiniteValues(t *testing.T) {
	sort := []query.SortTerm{{Field: "load"}, {Field: "ratio"}, {Field: "seenAt"}, {Field: "port"}}
	for _, testCase := range []struct {
		name     string
		values   []any
		wantNaN  bool  // the first value decodes to NaN, which Equal cannot compare
		wantRest []any // the decoded values after the first
	}{
		{"NaN and infinities", []any{math.NaN(), float32(math.Inf(-1)), "infinity", int64(1)}, true, []any{math.Inf(-1), "infinity", int64(1)}},
		{"positive infinity and negative infinite time", []any{math.Inf(1), float32(0.1), "-infinity", nil}, false, []any{float64(float32(0.1)), "-infinity", nil}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := (&query.Cursor{Sort: sort, Values: testCase.values, ID: alpha}).Encode()
			require.NoError(t, err)
			decoded, err := query.ParseCursor(testFields, encoded)
			require.NoError(t, err)
			require.Equal(t, testCase.wantNaN, math.IsNaN(decoded.Values[0].(float64)))
			if !testCase.wantNaN {
				require.Equal(t, testCase.values[0], decoded.Values[0])
			}
			require.Equal(t, testCase.wantRest, decoded.Values[1:])
		})
	}
}

func TestCursorRefusesTimesOutsideTheRFC3339Years(t *testing.T) {
	sort := []query.SortTerm{{Field: "seenAt"}}
	_, err := (&query.Cursor{Sort: sort, Values: []any{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}, ID: alpha}).Encode()
	require.ErrorContains(t, err, "outside 0 to 9999")

	// A driver returns local times: the UTC instant decides, and the wire carries UTC.
	east, west := time.FixedZone("east", 2*3600), time.FixedZone("west", -2*3600)
	for _, inRange := range []time.Time{time.Date(10000, 1, 1, 1, 0, 0, 0, east), time.Date(-1, 12, 31, 23, 0, 0, 0, west)} {
		encoded, err := (&query.Cursor{Sort: sort, Values: []any{inRange}, ID: alpha}).Encode()
		require.NoError(t, err, inRange)
		decoded, err := query.ParseCursor(testFields, encoded)
		require.NoError(t, err)
		require.Equal(t, []any{inRange.UTC()}, decoded.Values)
	}
	for _, outOfRange := range []time.Time{time.Date(9999, 12, 31, 23, 0, 0, 0, west), time.Date(0, 1, 1, 1, 0, 0, 0, east)} {
		_, err := (&query.Cursor{Sort: sort, Values: []any{outOfRange}, ID: alpha}).Encode()
		require.ErrorContains(t, err, "outside 0 to 9999", outOfRange)
	}

	encoded, err := (&query.Cursor{Sort: sort, Values: []any{time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)}, ID: alpha}).Encode()
	require.NoError(t, err)
	_, err = query.ParseCursor(testFields, encoded)
	require.NoError(t, err)

	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"sort":"seenAt","values":["9999-12-31T23:00:00-02:00"],"id":"` + alpha + `"}`))
	_, err = query.ParseCursor(testFields, forged)
	require.Equal(t, "carries a value that is not of type time", requireQueryError(t, err, query.CodeInvalidCursor)["after"])
}

func TestCursorEncodeReportsAnUnsupportedValueByMessageOnly(t *testing.T) {
	_, err := (&query.Cursor{Sort: []query.SortTerm{{Field: "port"}}, Values: []any{make(chan int)}, ID: alpha}).Encode()
	require.ErrorContains(t, err, "encode cursor value")
	var unsupported *json.UnsupportedTypeError
	require.False(t, errors.As(err, &unsupported), "the encoding/json error must not be part of the API")
}

func TestParseCursorRefusesTamperedCursors(t *testing.T) {
	encode := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	testCases := []struct{ name, encoded, want string }{
		{"not base64", "%%%", "is not a cursor"},
		{"not JSON", encode(`{`), "is not a cursor"},
		{"unknown sort field", encode(`{"sort":"nope","values":[1],"id":"` + alpha + `"}`), "names a sort this resource does not have"},
		{"unsortable field", encode(`{"sort":"tags","values":["a"],"id":"` + alpha + `"}`), "names a sort this resource does not have"},
		{"fewer values than terms", encode(`{"sort":"port,name","values":[1],"id":"` + alpha + `"}`), "does not match its sort"},
		{"more values than terms", encode(`{"sort":"port","values":[1,2],"id":"` + alpha + `"}`), "does not match its sort"},
		{"no id", encode(`{"sort":"port","values":[1],"id":""}`), "carries an id that is not a uuid"},
		{"id not a uuid", encode(`{"sort":"port","values":[1],"id":"x"}`), "carries an id that is not a uuid"},
		{"integer as a string", encode(`{"sort":"port","values":["high"],"id":"` + alpha + `"}`), "carries a value that is not of type int"},
		{"integer with a fraction", encode(`{"sort":"port","values":[1.5],"id":"` + alpha + `"}`), "carries a value that is not of type int"},
		{"float as a string", encode(`{"sort":"load","values":["0.5"],"id":"` + alpha + `"}`), "carries a value that is not of type float"},
		{"string as a number", encode(`{"sort":"name","values":[1],"id":"` + alpha + `"}`), "carries a value that is not of type string"},
		{"uuid not a uuid", encode(`{"sort":"groupId","values":["x"],"id":"` + alpha + `"}`), "carries a value that is not of type uuid"},
		{"time not a time", encode(`{"sort":"seenAt","values":["yesterday"],"id":"` + alpha + `"}`), "carries a value that is not of type time"},
		{"relative time", encode(`{"sort":"seenAt","values":[{"relative":"-1d"}],"id":"` + alpha + `"}`), "carries a value that is not of type time"},
		{"NUL in a string", encode(`{"sort":"name","values":["a\u0000b"],"id":"` + alpha + `"}`), "carries a string with U+0000"},
		{"NUL in an enum", encode(`{"sort":"status","values":["online\u0000"],"id":"` + alpha + `"}`), "carries a string with U+0000"},
		{"finite float as a string", encode(`{"sort":"load","values":["1.5"],"id":"` + alpha + `"}`), "carries a value that is not of type float"},
		{"time as another word", encode(`{"sort":"seenAt","values":["forever"],"id":"` + alpha + `"}`), "carries a value that is not of type time"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := query.ParseCursor(testFields, testCase.encoded)
			fields := requireQueryError(t, err, query.CodeInvalidCursor)
			require.Equal(t, testCase.want, fields["after"])
		})
	}
}

func TestCompileRefusesACursorOfAnotherSort(t *testing.T) {
	cursor := &query.Cursor{Sort: []query.SortTerm{{Field: "port"}}, Values: []any{int64(1)}, ID: alpha}
	_, err := query.Compile(testFields, nil, []query.SortTerm{{Field: "name"}}, query.Page{Limit: 1, After: cursor}, nil)
	fields := requireQueryError(t, err, query.CodeInvalidCursor)
	require.Equal(t, `was taken under sort "port", not "name"`, fields["after"])

	short := &query.Cursor{Sort: []query.SortTerm{{Field: "port"}}, ID: alpha}
	_, err = query.Compile(testFields, nil, []query.SortTerm{{Field: "port"}}, query.Page{Limit: 1, After: short}, nil)
	requireQueryError(t, err, query.CodeInvalidCursor)
}
