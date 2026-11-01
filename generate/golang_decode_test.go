package generate_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
	"github.com/govengers/livewire/generate/internal/fixture"
	"github.com/govengers/livewire/query"
)

// responseRegistry declares a mutation (or an upload) answering Response.
func responseRegistry[Response any](t *testing.T, upload bool) *livewire.Registry {
	t.Helper()
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	if upload {
		require.NoError(t, registry.Add(livewire.NewUpload("answer", "/answers", &livewire.UploadOptions[livewire.NoParameters, Response]{
			Check: authorization.IsUser(), ContentTypes: []string{"text/plain"}, MaximumBytes: 10,
			Handle: func(context.Context, *livewire.NoParameters, *livewire.File) (*Response, error) {
				return new(Response), nil
			},
		})))
		return registry
	}
	require.NoError(t, registry.Add(livewire.NewMutation("answer", http.MethodPost, "/answers", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, Response]{
		Check: authorization.IsUser(), Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*Response, error) {
			return new(Response), nil
		},
	})))
	return registry
}

func roundTrip(t *testing.T, value, decoded any) error {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return json.Unmarshal(encoded, decoded)
}

// TestGoClientAcceptsWhatEncodingJSONDecodes generates a client for every type encoding/json
// reads back what it wrote, and proves the round trip with encoding/json itself; the types
// the generator refuses do not round-trip.
func TestGoClientAcceptsWhatEncodingJSONDecodes(t *testing.T) {
	testCases := map[string]struct {
		registry *livewire.Registry
		value    any
		decoded  any
	}{
		"MarshalJSON with UnmarshalText":     {objectRegistry[fixture.JSONWithText](t), fixture.JSONWithText{}, new(fixture.JSONWithText)},
		"a string kind with MarshalText":     {objectRegistry[fixture.TextString](t), fixture.TextString("ready"), new(fixture.TextString)},
		"UnmarshalJSON over any field":       {objectRegistry[fixture.DecodesItself](t), fixture.DecodesItself{}, new(fixture.DecodesItself)},
		"an embedded UnmarshalJSON":          {objectRegistry[fixture.EmbedsDecoder](t), fixture.EmbedsDecoder{}, new(fixture.EmbedsDecoder)},
		"pointer-receiver codecs":            {objectRegistry[fixture.PointerCodec](t), &fixture.PointerCodec{}, new(fixture.PointerCodec)},
		"pointer-receiver codecs in a field": {objectRegistry[fixture.Page[fixture.PointerCodec]](t), fixture.Page[fixture.PointerCodec]{}, new(fixture.Page[fixture.PointerCodec])},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, roundTrip(t, testCase.value, testCase.decoded), "encoding/json decodes it")
			options := goOptions(t.TempDir())
			options.CommandLine = generate.CommandLineOptions{}
			require.NoError(t, generate.Write(testCase.registry, options))
		})
	}
	for _, refused := range []struct{ value, decoded any }{
		{fixture.JSONOnly{}, new(fixture.JSONOnly)},
		{fixture.TextOnly{}, new(fixture.TextOnly)},
		{fixture.TextReader{}, new(fixture.TextReader)},
		{fixture.HoldsStringer{Hook: time.Second}, new(fixture.HoldsStringer)},
	} {
		require.Error(t, roundTrip(t, refused.value, refused.decoded), "%T: what the generator refuses encoding/json cannot decode", refused.value)
	}
}

func TestGoClientFillsAPromotedIDOrRefusesItCleanly(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, generate.Write(createRegistry[fixture.CreateHidden](t), goOptions(directory)))
	require.Contains(t, readGenerated(t, directory, "client/client.go"), "if body.ID == uuid.Nil {", "promoted through an unexported embedded struct")

	refused := t.TempDir()
	require.NotPanics(t, func() {
		err := generate.Write(createRegistry[fixture.CreateThroughPointer](t), goOptions(refused))
		require.ErrorContains(t, err, "fills in a client id (D18) only")
	})
	entries, err := os.ReadDir(refused)
	require.NoError(t, err)
	require.Empty(t, entries, "a refused Write writes nothing")
}

func TestEnumValuesThatSpellOneNameKeepAConstantEach(t *testing.T) {
	values := []string{"nut", "Nut", "+1", "1", "in-progress", "in_progress"}
	registry, err := livewire.NewRegistry(nil)
	require.NoError(t, err)
	require.NoError(t, registry.Add(livewire.NewResource("parts", "/parts", &livewire.ResourceOptions[fixture.Note]{
		Read: authorization.IsUser(),
		Fields: query.Fields{
			{Name: "id", Type: query.TypeInteger, SQL: "parts.id", Filter: true, Sort: true},
			{Name: "state", Type: query.TypeEnum, SQL: "parts.state", Enum: values, Filter: true},
		},
		List: func(context.Context, *query.ListQuery) (*livewire.Page[fixture.Note], error) {
			return &livewire.Page[fixture.Note]{}, nil
		},
	})))
	directory := t.TempDir()
	options := goOptions(directory)
	options.CommandLine = generate.CommandLineOptions{}
	require.NoError(t, generate.Write(registry, options))
	filters := readGenerated(t, directory, "client/filters.go")
	for constant, value := range map[string]string{
		"PartsStateNut": "nut", "PartsStateNut2": "Nut", "PartsState1": "+1", "PartsState12": "1",
		"PartsStateInProgress": "in-progress", "PartsStateInProgress2": "in_progress",
	} {
		require.Regexp(t, `\t`+constant+` +PartsState = "`+regexp.QuoteMeta(value)+`"\n`, filters)
	}
}
