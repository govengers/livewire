package ginadapter

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/govengers/livewire"
)

// ExportRowsTrailer is the trailer of a complete export: the rows it wrote. The response
// declares it with its headers and sets it only when the export finished: a signal on top of
// the aborted body, for a client that reads trailers.
const ExportRowsTrailer = "Livewire-Export-Rows"

// export streams a resource's export as CSV (D35). The headers go out with the first byte
// the loader writes, so a refusal before it is answered as an error body; a success ends with
// the ExportRowsTrailer. A failure after the first byte aborts the response, so the client's
// read of the body fails, whatever middleware runs: on HTTP/1.1 the connection is closed
// under it; on HTTP/2 a write deadline in the past resets the stream, and the handler panics
// with http.ErrAbortHandler for the middleware that lets it through.
func (h *handlers) export(name string) gin.HandlerFunc {
	recorded := unit{target: name, kind: "export"}
	return func(ginContext *gin.Context) {
		writer := &exportWriter{ginContext: ginContext, filename: name + ".csv"}
		request := livewire.ExportRequest{Target: name, Source: sourceOf(ginContext), Writer: writer}
		rows, err := h.engine.Export(ginContext.Request.Context(), request)
		switch {
		case err == nil:
			writer.start()
			ginContext.Writer.Header().Set(ExportRowsTrailer, strconv.Itoa(rows))
			record(ginContext, recorded, http.StatusOK, nil).Int("livewire.rows", rows)
		case !writer.started:
			answer(ginContext, recorded, 0, nil, err)
		default:
			record(ginContext, recorded, http.StatusOK, err).Int("livewire.rows", rows)
			abort(ginContext)
		}
	}
}

// exportWriter writes the CSV headers and the status with the first byte.
type exportWriter struct {
	ginContext *gin.Context
	filename   string
	started    bool
}

func (w *exportWriter) Write(data []byte) (int, error) {
	w.start()
	return w.ginContext.Writer.Write(data)
}

func (w *exportWriter) start() {
	if w.started {
		return
	}
	w.started = true
	header := w.ginContext.Writer.Header()
	header.Set("Content-Type", "text/csv; charset=utf-8")
	header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": w.filename}))
	header.Set("Trailer", ExportRowsTrailer)
	w.ginContext.Writer.WriteHeader(http.StatusOK)
	w.ginContext.Writer.WriteHeaderNow()
}

// abort ends a response whose body is broken: the connection is closed under it (HTTP/1.1),
// else a write deadline in the past resets the stream (HTTP/2; a recovery middleware that
// swallows http.ErrAbortHandler, as gin.Recovery does, cannot undo it) and the handler panics
// with http.ErrAbortHandler.
func abort(ginContext *gin.Context) {
	ginContext.Writer.Flush()
	if unwrapper, ok := ginContext.Writer.(interface{ Unwrap() http.ResponseWriter }); ok {
		if hijacker, ok := unwrapper.Unwrap().(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				_ = connection.Close() // the response is broken on purpose; the close error adds nothing
				return
			}
		}
	}
	_ = http.NewResponseController(ginContext.Writer).SetWriteDeadline(time.Now().Add(-time.Second)) // a writer without deadlines leaves the panic alone to abort
	panic(http.ErrAbortHandler)
}

// multipartOverhead bounds the bytes of a multipart body besides the file: boundaries and
// part headers.
const multipartOverhead = 64 << 10

// readFile reads the file of an upload (D17): a multipart/form-data body's part "file", its
// content type from the part, or a raw body, its content type from the request. A raw body
// with a Content-Length streams to the handler as it is, refused before it is read when it
// is larger than MaximumBytes; a multipart part and a raw body without a length are read
// into memory, at most MaximumBytes of them, so the file's size is known before the handler
// runs. Reading stops at MaximumBytes + 1: a larger file is 400 at "file".
func readFile(ginContext *gin.Context, upload *livewire.Upload) (*livewire.File, error) {
	request := ginContext.Request
	limit := upload.MaximumBytes()
	// A malformed Content-Type is no multipart type: it is read as a raw body, whose type the
	// engine then refuses as not declared (400 at "file").
	mediaType, parameters, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaType != "multipart/form-data" {
		if request.ContentLength >= 0 {
			body := http.MaxBytesReader(ginContext.Writer, request.Body, limit)
			return &livewire.File{ContentType: request.Header.Get("Content-Type"), Size: request.ContentLength, Body: body}, nil
		}
		return bufferFile(request.Body, request.Header.Get("Content-Type"), "", limit)
	}
	body := http.MaxBytesReader(ginContext.Writer, request.Body, limit+multipartOverhead)
	reader := multipart.NewReader(body, parameters["boundary"])
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" {
		return nil, livewire.Invalid(map[string]string{"file": "a multipart upload carries its file as the first part, named file"})
	}
	contentType := part.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream" // a part without one (RFC 7578)
	}
	return bufferFile(part, contentType, part.FileName(), limit)
}

// bufferFile reads at most limit bytes of body into memory.
func bufferFile(body io.Reader, contentType, filename string, limit int64) (*livewire.File, error) {
	var buffer bytes.Buffer
	read, err := io.Copy(&buffer, io.LimitReader(body, limit+1))
	var tooLarge *http.MaxBytesError
	switch {
	case read > limit || errors.As(err, &tooLarge):
		return nil, livewire.Invalid(map[string]string{"file": "larger than " + strconv.FormatInt(limit, 10) + " bytes"})
	case err != nil:
		return nil, livewire.Invalid(map[string]string{"file": "could not be read"})
	}
	return &livewire.File{ContentType: contentType, Size: read, Body: &buffer, Filename: filename}, nil
}
