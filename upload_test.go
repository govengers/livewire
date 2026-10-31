package livewire_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

type attachment struct {
	Name string `json:"name"`
}

func uploadOptions() *livewire.UploadOptions[partID, attachment] {
	return &livewire.UploadOptions[partID, attachment]{
		Check:        authorization.IsUser(),
		ContentTypes: []string{"image/jpeg", "image/png"},
		MaximumBytes: 20 << 20,
		Handle: func(_ context.Context, _ *partID, file *livewire.File) (*attachment, error) {
			return &attachment{Name: file.Filename}, nil
		},
	}
}

func TestUploadDeclarationErrors(t *testing.T) {
	testCases := []struct {
		name   string
		change func(options *livewire.UploadOptions[partID, attachment])
		want   string
	}{
		{name: "no handle", change: func(options *livewire.UploadOptions[partID, attachment]) { options.Handle = nil }, want: "Handle is required"},
		{name: "no check", change: func(options *livewire.UploadOptions[partID, attachment]) { options.Check = nil }, want: "Check is required"},
		{name: "no content types", change: func(options *livewire.UploadOptions[partID, attachment]) { options.ContentTypes = nil }, want: "ContentTypes is required"},
		{name: "no size limit", change: func(options *livewire.UploadOptions[partID, attachment]) { options.MaximumBytes = 0 }, want: "MaximumBytes must be positive"},
		{name: "bad status", change: func(options *livewire.UploadOptions[partID, attachment]) { options.Status = http.StatusFound }, want: "not a success status"},
		{name: "content type with parameters", change: func(options *livewire.UploadOptions[partID, attachment]) {
			options.ContentTypes = []string{"text/plain; charset=utf-8"}
		}, want: "not a plain media type"},
		{name: "content type malformed", change: func(options *livewire.UploadOptions[partID, attachment]) { options.ContentTypes = []string{"image"} }, want: "not a plain media type"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := uploadOptions()
			testCase.change(options)
			err := addError(t, livewire.NewUpload("uploadAttachment", "/parts/{id}/attachments", options))
			require.ErrorContains(t, err, testCase.want)
			require.ErrorContains(t, err, `upload "uploadAttachment"`)
		})
	}
	require.ErrorContains(t, addError(t, livewire.NewUpload("x", "/x", uploadOptions())), "is not in the path")
	require.ErrorContains(t, addError(t, livewire.NewUpload[partID, attachment]("x", "/x/{id}", nil)), "Check is required")
}

func TestUpload(t *testing.T) {
	options := uploadOptions()
	upload := livewire.NewUpload("uploadAttachment", "/parts/{id}/attachments", options)
	require.NoError(t, addError(t, upload))
	require.Equal(t, livewire.KindUpload, upload.Kind())
	require.Equal(t, http.MethodPost, upload.Method())
	require.Equal(t, "/parts/{id}/attachments", upload.Path())
	require.Equal(t, http.StatusOK, upload.Status())
	require.Equal(t, int64(20<<20), upload.MaximumBytes())
	require.Equal(t, []string{"image/jpeg", "image/png"}, upload.ContentTypes())
	require.Equal(t, reflect.TypeFor[partID](), upload.ParametersType())
	require.Equal(t, reflect.TypeFor[attachment](), upload.ResponseType())
	require.Nil(t, upload.Tables())
	options.ContentTypes[0] = "changed"
	require.Equal(t, "image/jpeg", upload.ContentTypes()[0])

	require.Equal(t, "image/jpeg", upload.Accepts("image/jpeg; q=1"))
	require.Equal(t, "image/png", upload.Accepts("IMAGE/PNG"))
	require.Empty(t, upload.Accepts("audio/webm"))
	require.Empty(t, upload.Accepts(""))

	parameters, err := upload.DecodeParameters(livewire.MapSource{"id": "3f7b2c10-9e1d-4a6b-8c2f-1a2b3c4d5e6f"})
	require.NoError(t, err)
	response, err := upload.Handle(userContext(), parameters, &livewire.File{ContentType: "image/png", Size: 3, Body: strings.NewReader("png"), Filename: "a.png"})
	require.NoError(t, err)
	require.Equal(t, &attachment{Name: "a.png"}, response)

	created := uploadOptions()
	created.Status = http.StatusCreated
	require.Equal(t, http.StatusCreated, livewire.NewUpload("u", "/u/{id}", created).Status())
}
