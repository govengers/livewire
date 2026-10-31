package livewire

import (
	"context"
	"io"
	"mime"
	"net/http"
	"reflect"
	"slices"

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
type UploadOptions[Params, Response any] struct {
	Check        authorization.Check
	Status       int
	ContentTypes []string
	MaximumBytes int64
	Handle       func(ctx context.Context, parameters *Params, file *File) (*Response, error)
}

// Upload is a binary write: REST only, POST, multipart or raw body, declared content types
// and size limit, typed parameters, JSON response.
type Upload struct {
	Signature
	parametersType reflect.Type
	responseType   reflect.Type
	status         int
	contentTypes   []string
	maximumBytes   int64
	handle         func(ctx context.Context, parameters any, file *File) (any, error)
}

// NewUpload declares a binary write at path, whose `{name}` placeholders are the
// `path:"name"` fields of Params. A mistake is reported by Registry.Add.
func NewUpload[Params, Response any](name, path string, options *UploadOptions[Params, Response]) *Upload {
	if options == nil {
		options = &UploadOptions[Params, Response]{}
	}
	upload := &Upload{
		Signature:      newSignature(name, http.MethodPost, path, nil, options.Check),
		parametersType: reflect.TypeFor[Params](),
		responseType:   reflect.TypeFor[Response](),
		status:         options.Status,
		contentTypes:   slices.Clone(options.ContentTypes),
		maximumBytes:   options.MaximumBytes,
	}
	if options.Handle != nil {
		upload.handle = func(ctx context.Context, parameters any, file *File) (any, error) {
			return options.Handle(ctx, parameters.(*Params), file)
		}
	}
	upload.compileParameters(upload.parametersType)
	upload.validate(options.Handle == nil)
	return upload
}

func (u *Upload) validate(handleMissing bool) {
	switch {
	case handleMissing:
		u.fail("Handle is required")
	case len(u.contentTypes) == 0:
		u.fail("ContentTypes is required")
	case u.maximumBytes <= 0:
		u.fail("MaximumBytes must be positive")
	case u.status != 0 && (u.status < http.StatusOK || u.status > 299):
		u.fail("Status %d is not a success status", u.status)
	}
	for _, contentType := range u.contentTypes {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != contentType {
			u.fail("ContentTypes has %q, which is not a plain media type", contentType)
		}
	}
	if u.status == 0 {
		u.status = http.StatusOK
	}
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

func (u *Upload) routes() []route { return []route{{u.method, u.path}} }

// Handle runs the handler with the decoded parameters (a *Params) and the file; the response
// is a *Response.
func (u *Upload) Handle(ctx context.Context, parameters any, file *File) (any, error) {
	return u.handle(ctx, parameters, file)
}
