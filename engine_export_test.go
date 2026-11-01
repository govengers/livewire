package livewire_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestExportStreamsTheVisibleColumns(t *testing.T) {
	bed := newTestbed(t, nil)
	bed.world.put(gadget{ID: "a", Name: "Anchor", Secret: "s"})
	bed.world.put(gadget{ID: "b", Name: "Bolt"})
	export := func(ctx context.Context, parameters livewire.MapSource) (string, int, error) {
		var written bytes.Buffer
		rows, err := bed.engine.Export(ctx, livewire.ExportRequest{Target: "gadgets", Source: parameters, Writer: &written})
		return written.String(), rows, err
	}
	reader := auth.WithIdentity(t.Context(), person("ada", roleReader))

	text, rows, err := export(reader, livewire.MapSource{"format": "csv", "sort": "-name"})
	require.NoError(t, err)
	require.Equal(t, 2, rows)
	require.Equal(t, "id,name,group,createdAt\na,Anchor\nb,Bolt\n", text, "the guarded secret is no column (D39)")
	outcome := bed.outcomes.of(livewire.OutcomeExport)[0]
	require.Equal(t, 2, outcome.Rows)
	require.Equal(t, "gadgets", outcome.Target)
	require.Equal(t, http.StatusOK, outcome.Status)

	text, _, err = export(auth.WithIdentity(t.Context(), admin("root")), nil)
	require.NoError(t, err)
	require.Contains(t, text, "id,name,group,createdAt,secret\n", "an admin exports the secret")

	testCases := []struct {
		name       string
		identity   *auth.Identity
		target     string
		parameters livewire.MapSource
		status     int
	}{
		{"another format", person("ada", roleReader), "gadgets", livewire.MapSource{"format": "xlsx"}, http.StatusBadRequest},
		{"an unsortable field", person("ada", roleReader), "gadgets", livewire.MapSource{"sort": "secret"}, http.StatusBadRequest},
		{"no export loader", person("ada", roleReader), "openGadgets", nil, http.StatusNotFound},
		{"no declaration", person("ada", roleReader), "nothing", nil, http.StatusNotFound},
		{"the static check", nil, "gadgets", nil, http.StatusUnauthorized},
		{"a failing loader", person("ada", roleReader), "gadgets", livewire.MapSource{"filter": `{"where":{"field":"name","op":"eq","value":"fail"}}`}, http.StatusInternalServerError},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := t.Context()
			if testCase.identity != nil {
				ctx = auth.WithIdentity(ctx, testCase.identity)
			}
			_, _, err := export(ctx, testCase.parameters)
			if testCase.target != "gadgets" {
				_, err = bed.engine.Export(ctx, livewire.ExportRequest{Target: testCase.target, Writer: &bytes.Buffer{}})
			}
			requireStatus(t, err, testCase.status)
		})
	}
}
