package livewire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/govengers/auth/authorization"
)

// File is the binary body of an upload as the handler receives it: the declared content
// type it matched (without parameters), its size in bytes, the body to read once, and the
// client's file name when a multipart part carried one.
type File struct {
	ContentType string
	Size        int64
	Body        io.Reader
	Filename    string
}

// UploadOptions declares a binary write (D17). Check, ContentTypes, MaximumBytes and Handle
// are required; Status is the success status (default 200).
type UploadOptions[Parameters, Response any] struct {
	Check        authorization.Check
	Status       int
	ContentTypes []string
	MaximumBytes int64
	Handle       func(ctx context.Context, parameters *Parameters, file *File) (*Response, error)
}

// Upload is a binary write: REST only, POST, multipart or raw body, declared content types
// and size limit, typed parameters, JSON response.
type Upload struct {
	signature
	parametersType reflect.Type
	responseType   reflect.Type
	status         int
	contentTypes   []string
	maximumBytes   int64
	handle         func(ctx context.Context, parameters any, file *File) (any, error)
}

// NewUpload declares a binary write at path, whose `{name}` placeholders are the
// `path:"name"` fields of Parameters. The options are copied. A mistake is reported by
// Registry.Add (D43).
func NewUpload[Parameters, Response any](name, path string, options *UploadOptions[Parameters, Response]) *Upload {
	if options == nil {
		options = &UploadOptions[Parameters, Response]{}
	}
	upload := &Upload{
		signature:      newSignature(name, http.MethodPost, path, nil, options.Check),
		parametersType: reflect.TypeFor[Parameters](),
		responseType:   reflect.TypeFor[Response](),
		status:         intOrDefault(options.Status, http.StatusOK),
		contentTypes:   slices.Clone(options.ContentTypes),
		maximumBytes:   options.MaximumBytes,
	}
	if handle := options.Handle; handle != nil {
		upload.handle = func(ctx context.Context, parameters any, file *File) (any, error) {
			return handle(ctx, parameters.(*Parameters), file)
		}
	}
	upload.compileParameters(upload.parametersType)
	upload.failWith(upload.validate(options.Status))
	return upload
}

// Kind is KindUpload.
func (u *Upload) Kind() Kind { return KindUpload }

// ParametersType is the Go type of the parameters struct.
func (u *Upload) ParametersType() reflect.Type { return u.parametersType }

// ResponseType is the Go type of the response.
func (u *Upload) ResponseType() reflect.Type { return u.responseType }

// Status is the success status.
func (u *Upload) Status() int { return u.status }

// ContentTypes are the accepted media types; the result is the caller's copy.
func (u *Upload) ContentTypes() []string { return slices.Clone(u.contentTypes) }

// MaximumBytes is the size limit of one upload.
func (u *Upload) MaximumBytes() int64 { return u.maximumBytes }

// Accepts returns the declared media type a Content-Type header value matches, parameters
// ignored, or "" when none does.
func (u *Upload) Accepts(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !slices.Contains(u.contentTypes, mediaType) {
		return ""
	}
	return mediaType
}

// Handle runs the handler with the decoded parameters (a *Parameters) and the file; the response
// is a *Response.
func (u *Upload) Handle(ctx context.Context, parameters any, file *File) (any, error) {
	return u.handle(ctx, parameters, file)
}

func (u *Upload) declaration() Declaration { return u }

func (u *Upload) routes() []route { return []route{{u.method, u.path}} }

func (u *Upload) validate(status int) error {
	switch {
	case u.handle == nil:
		return errors.New("Handle is required")
	case len(u.contentTypes) == 0:
		return errors.New("ContentTypes is required")
	case u.maximumBytes <= 0:
		return errors.New("MaximumBytes must be positive")
	case status != 0 && !isSuccessStatus(status):
		return fmt.Errorf("Status %d is not a success status", status)
	}
	for _, contentType := range u.contentTypes {
		if !isPlainMediaType(contentType) {
			return fmt.Errorf("ContentTypes has %q, which is not a plain media type", contentType)
		}
	}
	return nil
}

// isPlainMediaType accepts "type/subtype" without parameters.
func isPlainMediaType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == contentType && strings.Count(mediaType, "/") == 1
}
