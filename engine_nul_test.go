package livewire_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/govengers/auth"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

// TestEveryInputLivewireDecodesRefusesUNUL covers the inputs livewire decodes itself besides
// a mutation's body and the declared parameters (D95): a U+0000 answers 400 naming the
// member, PostgreSQL text never sees it.
func TestEveryInputLivewireDecodesRefusesUNUL(t *testing.T) {
	bed := newTestbed(t, nil)
	ada := auth.WithIdentity(t.Context(), person("ada", roleReader))
	filter := `"filter":{"where":{"op":"eq","field":"name","value":"Anchor"}}`
	for member, body := range map[string]string{
		"name":     `{"resource":"gadgets","name":"a\u0000b",` + filter + `}`,
		"context":  `{"resource":"gadgets","context":"c\u0000",` + filter + `}`,
		"resource": `{"resource":"gadgets\u0000",` + filter + `}`,
	} {
		_, err := bed.engine.SaveFilter(ada, []byte(body))
		fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, member)
		require.Equal(t, "must not contain U+0000", fields[member], "the stored filter's %s", member)
	}

	socket := bed.dial("ada")
	board := map[string]string{"team": "red", "key": "board"}
	joined := socket.do(livewire.OperationJoin, "team", board, json.RawMessage(`{"note":{"deep":"\u0000"}}`))
	require.Equal(t, http.StatusBadRequest, joined.Status, "a presence state holding U+0000")
	require.Equal(t, "must not contain U+0000", joined.Error.Fields["note.deep"])
	joined = socket.do(livewire.OperationJoin, "team", board, json.RawMessage(`"\u0000"`))
	require.Equal(t, "must not contain U+0000", joined.Error.Fields["body"], "a state that is one string")
	require.True(t, socket.do(livewire.OperationJoin, "team", board, json.RawMessage(`{"note":"here"}`)).OK)

	nulKey := map[string]string{"team": "red", "key": "bo\x00ard"}
	for _, operation := range []livewire.Operation{livewire.OperationJoin, livewire.OperationQuery} {
		target := "team"
		if operation == livewire.OperationQuery {
			target = "team.presence"
		}
		answered := socket.do(operation, target, nulKey, nil)
		require.Equal(t, http.StatusBadRequest, answered.Status, "%s with a presence key holding U+0000", operation)
		require.Equal(t, "must not contain U+0000", answered.Error.Fields[livewire.ParameterPresenceKey])
	}

	file := &livewire.File{ContentType: "image/png", Size: 3, Body: strings.NewReader("png"), Filename: "a\x00b.png"}
	_, _, err := bed.engine.Upload(auth.WithIdentity(t.Context(), person("ada", roleWriter)), livewire.UploadRequest{Target: "uploadPicture", File: file})
	fields := requireError(t, err, http.StatusBadRequest, livewire.CodeInvalidRequest, "file")
	require.Equal(t, "the file name must not contain U+0000", fields["file"])
}
