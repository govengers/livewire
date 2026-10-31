package query_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire/query"
)

const testUUID = `"6f1c1f2e-8a39-4c7a-9f7e-1a2b3c4d5e6f"`

func validDocuments() map[string]string {
	return map[string]string{
		"empty":       `{}`,
		"search only": `{"search":"gtr"}`,
		"and or not":  `{"where":{"and":[{"field":"status","op":"in","value":["offline"]},{"or":[{"field":"name","op":"contains","value":"gtr"},{"not":{"field":"enabled","op":"eq","value":true}}]}]}}`,
		"string ops":  `{"where":{"and":[{"field":"name","op":"eq","value":"a"},{"field":"name","op":"ne","value":"a"},{"field":"name","op":"notIn","value":["a","b"]},{"field":"name","op":"startsWith","value":"a"},{"field":"name","op":"isNull","value":false}]}}`,
		"text ops":    `{"where":{"field":"note","op":"contains","value":"x"}}`,
		"int ops":     `{"where":{"and":[{"field":"port","op":"gt","value":1},{"field":"port","op":"gte","value":1},{"field":"port","op":"lt","value":1},{"field":"port","op":"lte","value":1},{"field":"port","op":"in","value":[1,2]}]}}`,
		"float ops":   `{"where":{"and":[{"field":"load","op":"lt","value":0.5},{"field":"load","op":"eq","value":2}]}}`,
		"bool ops":    `{"where":{"and":[{"field":"enabled","op":"ne","value":false},{"field":"enabled","op":"isNull","value":true}]}}`,
		"time ops":    `{"where":{"and":[{"field":"seenAt","op":"gt","value":{"relative":"-7d"}},{"field":"seenAt","op":"lte","value":"2026-09-23T10:00:00Z"},{"field":"seenAt","op":"in","value":[{"relative":"-1h"},"2026-09-23T10:00:00+02:00"]}]}}`,
		"enum ops":    `{"where":{"field":"status","op":"notIn","value":["online"]}}`,
		"uuid ops":    `{"where":{"field":"groupId","op":"eq","value":` + testUUID + `}}`,
		"array ops":   `{"where":{"and":[{"field":"tags","op":"has","value":"a"},{"field":"tags","op":"hasAny","value":["a","b"]},{"field":"tags","op":"hasAll","value":["a"]},{"field":"tags","op":"isEmpty","value":true}]}}`,
		"depth 8":     `{"where":` + nest(7, `{"field":"port","op":"eq","value":1}`) + `}`,
		"200 nodes":   `{"where":{"or":[` + conditions(199) + `]}}`,
		"500 values":  `{"where":{"field":"port","op":"in","value":[` + numbers(500) + `]}}`,
		"2^53":        `{"where":{"and":[{"field":"port","op":"eq","value":9007199254740992},{"field":"port","op":"eq","value":-9007199254740992}]}}`,
		"1 KiB":       `{"where":{"field":"name","op":"eq","value":"` + strings.Repeat("a", 1024) + `"}}`,
		"large float": `{"where":{"and":[{"field":"load","op":"gt","value":1e16},{"field":"load","op":"lt","value":1e300},{"field":"load","op":"eq","value":1e-300}]}}`,
		"1 KiB time":  `{"where":{"field":"seenAt","op":"gt","value":"` + longTimestamp(1024) + `"}}`,
		"year bounds": `{"where":{"field":"seenAt","op":"in","value":["0000-01-01T00:00:00Z","9999-12-31T23:59:59Z"]}}`,
	}
}

func TestParseAcceptsValidDocuments(t *testing.T) {
	for name, raw := range validDocuments() {
		t.Run(name, func(t *testing.T) {
			mustParse(t, raw)
		})
	}
}

func TestParseRefusesInvalidDocuments(t *testing.T) {
	testCases := []struct{ name, raw, path string }{
		{"not an object", `[]`, ""},
		{"not JSON", `{"where":`, ""},
		{"trailing data", `{} {}`, ""},
		{"unknown top key", `{"limit":3}`, "limit"},
		{"duplicate top key", `{"search":"a","search":"b"}`, ""},
		{"duplicate nested key", `{"where":{"field":"port","op":"eq","field":"name","value":1}}`, ""},
		{"search not a string", `{"search":3}`, "search"},
		{"search too long", `{"search":"` + strings.Repeat("a", 1025) + `"}`, "search"},
		{"where not an object", `{"where":5}`, "where"},
		{"empty and", `{"where":{"and":[]}}`, "where.and"},
		{"empty or", `{"where":{"or":[]}}`, "where.or"},
		{"and not a list", `{"where":{"and":{}}}`, "where.and"},
		{"not a node", `{"where":{"not":[]}}`, "where.not"},
		{"two kinds", `{"where":{"and":[{"field":"port","op":"eq","value":1}],"or":[]}}`, "where"},
		{"extra key on a condition", `{"where":{"field":"port","op":"eq","value":1,"extra":1}}`, "where"},
		{"field not a string", `{"where":{"field":1,"op":"eq","value":1}}`, "where.field"},
		{"op not a string", `{"where":{"field":"port","op":1,"value":1}}`, "where.op"},
		{"unknown field", `{"where":{"and":[{"field":"port","op":"eq","value":1},{"or":[{"field":"nope","op":"eq","value":1}]}]}}`, "where.and[1].or[0].field"},
		{"unfilterable field", `{"where":{"field":"secret","op":"eq","value":"x"}}`, "where.field"},
		{"json field", `{"where":{"field":"data","op":"eq","value":"x"}}`, "where.field"},
		{"unknown op", `{"where":{"field":"port","op":"like","value":1}}`, "where.op"},
		{"op not for type", `{"where":{"field":"enabled","op":"gt","value":true}}`, "where.op"},
		{"contains on int", `{"where":{"field":"port","op":"contains","value":"1"}}`, "where.op"},
		{"has on string", `{"where":{"field":"name","op":"has","value":"1"}}`, "where.op"},
		{"missing value", `{"where":{"field":"port","op":"eq"}}`, "where.value"},
		{"wrong type", `{"where":{"field":"port","op":"eq","value":"1"}}`, "where.value"},
		{"string not a string", `{"where":{"field":"name","op":"eq","value":1}}`, "where.value"},
		{"bool not a bool", `{"where":{"field":"enabled","op":"eq","value":"true"}}`, "where.value"},
		{"not an integer", `{"where":{"field":"port","op":"eq","value":1.5}}`, "where.value"},
		{"float beyond float64", `{"where":{"field":"load","op":"eq","value":1e309}}`, "where.value"},
		{"float underflowing to zero", `{"where":{"field":"load","op":"eq","value":1e-400}}`, "where.value"},
		{"2^53+1", `{"where":{"field":"port","op":"eq","value":9007199254740993}}`, "where.value"},
		{"-2^53-1", `{"where":{"field":"port","op":"eq","value":-9007199254740993}}`, "where.value"},
		{"huge exponent", `{"where":{"field":"load","op":"eq","value":1e-999999}}`, "where.value"},
		{"integer with a huge exponent", `{"where":{"field":"port","op":"eq","value":1e500}}`, "where.value"},
		{"fraction rounding to an integer", `{"where":{"field":"port","op":"eq","value":1.0000000000000001}}`, "where.value"},
		{"enum value", `{"where":{"field":"status","op":"eq","value":"sleeping"}}`, "where.value"},
		{"bad uuid", `{"where":{"field":"groupId","op":"eq","value":"x"}}`, "where.value"},
		{"uuid not a string", `{"where":{"field":"groupId","op":"eq","value":1}}`, "where.value"},
		{"bad time", `{"where":{"field":"seenAt","op":"gt","value":"yesterday"}}`, "where.value"},
		{"time not a string", `{"where":{"field":"seenAt","op":"gt","value":1}}`, "where.value"},
		{"bad relative", `{"where":{"field":"seenAt","op":"gt","value":{"relative":"-1x"}}}`, "where.value"},
		{"relative with extra key", `{"where":{"field":"seenAt","op":"gt","value":{"relative":"-1h","x":1}}}`, "where.value"},
		{"isNull not bool", `{"where":{"field":"name","op":"isNull","value":"yes"}}`, "where.value"},
		{"isEmpty not bool", `{"where":{"field":"tags","op":"isEmpty","value":1}}`, "where.value"},
		{"in not a list", `{"where":{"field":"port","op":"in","value":1}}`, "where.value"},
		{"empty in", `{"where":{"field":"port","op":"in","value":[]}}`, "where.value"},
		{"bad in item", `{"where":{"field":"port","op":"in","value":[1,"2"]}}`, "where.value[1]"},
		{"bad hasAny item", `{"where":{"field":"tags","op":"hasAny","value":["a",1]}}`, "where.value[1]"},
		{"null value", `{"where":{"field":"name","op":"eq","value":null}}`, "where.value"},
		{"null node", `{"where":{"and":[null]}}`, "where.and[0]"},
		{"depth 9", `{"where":` + nest(8, `{"field":"port","op":"eq","value":1}`) + `}`, "where.not.not.not.not.not.not.not.not"},
		{"201 nodes", `{"where":{"or":[` + conditions(200) + `]}}`, "where.or[199]"},
		{"501 values", `{"where":{"field":"port","op":"in","value":[` + numbers(501) + `]}}`, "where.value"},
		{"long string", `{"where":{"field":"name","op":"eq","value":"` + strings.Repeat("a", 1025) + `"}}`, "where.value"},
		{"17 KiB", `{"search":"` + strings.Repeat("a", 17*1024) + `"}`, ""},
		{"long time", `{"where":{"field":"seenAt","op":"gt","value":"` + longTimestamp(1025) + `"}}`, "where.value"},
		{"NUL in a value", `{"where":{"field":"name","op":"eq","value":"a\u0000b"}}`, "where.value"},
		{"NUL in a list element", `{"where":{"field":"tags","op":"hasAny","value":["a","b\u0000"]}}`, "where.value[1]"},
		{"NUL in an enum value", `{"where":{"field":"status","op":"eq","value":"online\u0000"}}`, "where.value"},
		{"NUL in a time", `{"where":{"field":"seenAt","op":"gt","value":"2026-09-23T10:00:00Z\u0000"}}`, "where.value"},
		{"NUL in a relative time", `{"where":{"field":"seenAt","op":"gt","value":{"relative":"-7d\u0000"}}}`, "where.value"},
		{"NUL in search", `{"search":"a\u0000"}`, "search"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := query.Parse(testFields, []byte(testCase.raw))
			requireFilterPath(t, err, testCase.path)
		})
	}
}

// longTimestamp is a valid RFC 3339 time padded with fractional digits to the byte length.
func longTimestamp(length int) string {
	const prefix, suffix = "2026-09-23T10:00:00.", "Z"
	return prefix + strings.Repeat("1", length-len(prefix)-len(suffix)) + suffix
}

func TestParseNamesTheNumberFault(t *testing.T) {
	testCases := []struct{ name, raw, want string }{
		{"exponent out of range", `{"where":{"field":"load","op":"eq","value":1e-999999}}`, "the exponent must be between -400 and 400"},
		{"float too large", `{"where":{"field":"load","op":"eq","value":1e309}}`, "the number is too large for a float"},
		{"float too small", `{"where":{"field":"load","op":"eq","value":1e-400}}`, "the number is too small for a float"},
		{"integer beyond 2^53", `{"where":{"field":"port","op":"eq","value":9007199254740993}}`, "the integer must be at most 2^53 in magnitude"},
		{"integer with a fraction", `{"where":{"field":"port","op":"eq","value":1.5}}`, "must be an integer"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := query.Parse(testFields, []byte(testCase.raw))
			fields := requireQueryError(t, err, query.CodeInvalidFilter)
			require.Equal(t, testCase.want, fields["where.value"])
		})
	}
}

func TestParseKeepsTimesInsideTheRFC3339Years(t *testing.T) {
	timeDocument := func(value string) string {
		return `{"where":{"field":"seenAt","op":"gt","value":"` + value + `"}}`
	}
	for _, accepted := range []string{"0000-01-01T00:00:00Z", "0000-01-01T01:00:00+01:00", "9999-12-31T23:59:59.999999999Z", "9999-12-31T23:59:59-00:00"} {
		mustParse(t, timeDocument(accepted))
	}
	for _, refused := range []string{"0000-01-01T00:30:00+01:00", "9999-12-31T23:00:00-02:00"} {
		_, err := query.Parse(testFields, []byte(timeDocument(refused)))
		fields := requireQueryError(t, err, query.CodeInvalidFilter)
		require.Contains(t, fields["where.value"], "outside the years 0 to 9999")
	}
}

func TestParseRefusesADocumentWhoseCanonicalFormExceedsTheLimit(t *testing.T) {
	testCases := map[string]string{
		"numbers written out": `{"where":{"and":[{"field":"load","op":"in","value":[` + repeat("1e15", 500) + `]},{"field":"load","op":"in","value":[` + repeat("1e15", 500) + `]}]}}`,
		"U+2028 escaped":      `{"where":{"field":"name","op":"in","value":[` + repeat(`"`+strings.Repeat(" ", 341)+`"`, 14) + `]}}`,
	}
	for name, raw := range testCases {
		t.Run(name, func(t *testing.T) {
			require.LessOrEqual(t, len(raw), query.MaximumDocumentBytes)
			_, err := query.Parse(testFields, []byte(raw))
			fields := requireQueryError(t, err, query.CodeInvalidFilter)
			require.Contains(t, fields[""], "canonical form")
		})
	}
}

func repeat(item string, count int) string {
	parts := make([]string, count)
	for index := range parts {
		parts[index] = item
	}
	return strings.Join(parts, ",")
}

func TestCanonicalIsIdempotent(t *testing.T) {
	for name, raw := range validDocuments() {
		t.Run(name, func(t *testing.T) {
			first, err := query.Canonical(mustParse(t, raw))
			require.NoError(t, err)
			second, err := query.Canonical(mustParse(t, string(first)))
			require.NoError(t, err)
			require.Equal(t, string(first), string(second))
		})
	}
}

func TestParseRefusesSearchWithoutSearchableField(t *testing.T) {
	fields := query.Fields{{Name: "port", Type: query.TypeInteger, SQL: "t.port", Filter: true}}
	_, err := query.Parse(fields, []byte(`{"search":"x"}`))
	requireFilterPath(t, err, "search")
}

func TestParseNamesTheSameFaultEveryTime(t *testing.T) {
	for range 50 {
		_, err := query.Parse(testFields, []byte(`{"where":5,"search":3,"zzz":1}`))
		requireFilterPath(t, err, "search")
	}
}

func TestParseRelativeTimes(t *testing.T) {
	relativeDocument := func(expression string) string {
		return `{"where":{"field":"seenAt","op":"gt","value":{"relative":"` + expression + `"}}}`
	}
	accepted := map[string]float64{
		"-7d":                        -7 * 24 * 3600,
		"-1h30m":                     -5400,
		"+2w":                        14 * 24 * 3600,
		"6h":                         6 * 3600,
		strings.Repeat("0s", 512):    0,
		strings.Repeat("1s", 1024/2): 512,
	}
	for expression, wantSeconds := range accepted {
		_, arguments := compileSQL(t, relativeDocument(expression))
		require.Equal(t, []any{wantSeconds}, arguments, expression)
	}
	refused := []string{"7", "-1x", "1.5h", "-30500w", "-20000w", "213504d", strings.Repeat("0s", 513)}
	for _, expression := range refused {
		_, err := query.Parse(testFields, []byte(relativeDocument(expression)))
		requireFilterPath(t, err, "where.value")
	}
}

func TestOperatorsOfReturnsACopy(t *testing.T) {
	require.Equal(t, []query.Operator{query.OperatorEqual, query.OperatorNotEqual, query.OperatorIsNull}, query.OperatorsOf(query.TypeBoolean))
	require.Empty(t, query.OperatorsOf(query.TypeJSON))
	leaked := query.OperatorsOf(query.TypeBoolean)
	leaked[0] = query.OperatorGreater
	require.Equal(t, query.OperatorEqual, query.OperatorsOf(query.TypeBoolean)[0])
	_, err := query.Parse(testFields, []byte(`{"where":{"field":"enabled","op":"gt","value":true}}`))
	requireFilterPath(t, err, "where.op")
}

func TestIsTimeDependent(t *testing.T) {
	testCases := []struct {
		name string
		raw  string
		want bool
	}{
		{"relative scalar", `{"where":{"field":"seenAt","op":"gt","value":{"relative":"-7d"}}}`, true},
		{"relative in a list", `{"where":{"field":"seenAt","op":"in","value":["2026-09-23T10:00:00Z",{"relative":"-7d"}]}}`, true},
		{"relative under not and or", `{"where":{"not":{"or":[{"field":"port","op":"eq","value":1},{"field":"seenAt","op":"lt","value":{"relative":"1d"}}]}}}`, true},
		{"absolute time", `{"where":{"field":"seenAt","op":"gt","value":"2026-09-23T10:00:00Z"}}`, false},
		{"no where", `{"search":"x"}`, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, mustParse(t, testCase.raw).IsTimeDependent())
		})
	}
	require.False(t, (*query.Document)(nil).IsTimeDependent())
}

func TestFieldNames(t *testing.T) {
	document := mustParse(t, `{"where":{"and":[{"field":"port","op":"eq","value":1},{"not":{"or":[{"field":"name","op":"eq","value":"a"},{"field":"port","op":"eq","value":2}]}}]}}`)
	require.Equal(t, []string{"name", "port"}, document.FieldNames())
	require.Empty(t, mustParse(t, `{}`).FieldNames())
	require.Empty(t, (*query.Document)(nil).FieldNames())
}

func TestCanonicalIsStable(t *testing.T) {
	same := []string{
		`{"where":{"and":[{"field":"port","op":"in","value":[1,2.0,3e0]},{"field":"name","op":"eq","value":"a<b"}]},"search":"x"}`,
		`{ "search" : "x", "where" : { "and" : [ { "value" : [1.0, 2, 30e-1], "op" : "in", "field" : "port" }, {"op":"eq","value":"a<b","field":"name"} ] } }`,
	}
	want := `{"search":"x","where":{"and":[{"field":"port","op":"in","value":[1,2,3]},{"field":"name","op":"eq","value":"a<b"}]}}`
	for _, raw := range same {
		canonical, err := query.Canonical(mustParse(t, raw))
		require.NoError(t, err)
		require.Equal(t, want, string(canonical))
	}
	firstHash, err := query.Hash(mustParse(t, same[0]))
	require.NoError(t, err)
	secondHash, err := query.Hash(mustParse(t, same[1]))
	require.NoError(t, err)
	require.Equal(t, firstHash, secondHash)
	require.Len(t, firstHash, 64)
	otherHash, err := query.Hash(mustParse(t, `{"search":"y"}`))
	require.NoError(t, err)
	require.NotEqual(t, firstHash, otherHash)
}

func TestCanonicalForms(t *testing.T) {
	testCases := []struct{ name, raw, want string }{
		{"empty", `{}`, `{}`},
		{"float keeps its fraction", `{"where":{"field":"load","op":"eq","value":0.50}}`, `{"where":{"field":"load","op":"eq","value":0.5}}`},
		{"uuid lower case", `{"where":{"field":"id","op":"in","value":["6F1C1F2E-8A39-4C7A-9F7E-1A2B3C4D5E6F"]}}`, `{"where":{"field":"id","op":"in","value":["6f1c1f2e-8a39-4c7a-9f7e-1a2b3c4d5e6f"]}}`},
		{"time in UTC", `{"where":{"field":"seenAt","op":"lt","value":"2026-09-23T12:00:00.5+02:00"}}`, `{"where":{"field":"seenAt","op":"lt","value":"2026-09-23T10:00:00.5Z"}}`},
		{"relative in seconds", `{"where":{"field":"seenAt","op":"in","value":[{"relative":"-7d"},{"relative":"-1w"},{"relative":"-168h"},{"relative":"+90m"}]}}`, `{"where":{"field":"seenAt","op":"in","value":[{"relative":"-604800s"},{"relative":"-604800s"},{"relative":"-604800s"},{"relative":"5400s"}]}}`},
		{"not and or", `{"where":{"not":{"or":[{"field":"enabled","op":"isNull","value":true},{"field":"tags","op":"isEmpty","value":false}]}}}`, `{"where":{"not":{"or":[{"field":"enabled","op":"isNull","value":true},{"field":"tags","op":"isEmpty","value":false}]}}}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			canonical, err := query.Canonical(mustParse(t, testCase.raw))
			require.NoError(t, err)
			require.Equal(t, testCase.want, string(canonical))
		})
	}
	canonical, err := query.Canonical(nil)
	require.NoError(t, err)
	require.Equal(t, `{}`, string(canonical))
}

// nest wraps the condition in depth not nodes, so the condition sits at depth+1.
func nest(depth int, condition string) string {
	out := condition
	for range depth {
		out = `{"not":` + out + `}`
	}
	return out
}

func conditions(count int) string {
	parts := make([]string, count)
	for index := range parts {
		parts[index] = `{"field":"port","op":"eq","value":1}`
	}
	return strings.Join(parts, ",")
}

func numbers(count int) string {
	parts := make([]string, count)
	for index := range parts {
		parts[index] = fmt.Sprint(index)
	}
	return strings.Join(parts, ",")
}
