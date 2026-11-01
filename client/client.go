package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/govengers/livewire"
)

// The headers the client sends and reads; a test pins them to the REST adapter's.
const (
	IdempotencyKeyHeader = "Idempotency-Key"
	ExportRowsTrailer    = "Livewire-Export-Rows"
)

const (
	defaultRequestTimeout        = 30 * time.Second
	defaultReconnectDelay        = time.Second
	defaultMaximumReconnectDelay = 30 * time.Second
	// maximumErrorBytes bounds the error body a refusal is read up to.
	maximumErrorBytes = 1 << 20
)

// Options configures a Client. BaseURL is the API's root, http or https, the registry's
// BasePath included ("https://example.com/api/v1"): every route is relative to it. APIKey is
// the key the server's API-key resolver takes (D20), sent as `Authorization: Bearer`. Both
// are required.
//
// HTTP sends the REST requests (default: a new http.Client without a timeout; bound each
// request with its context). SocketURL is the socket endpoint, ws or wss (default: BaseURL
// with the ws scheme and "/socket" appended). RequestTimeout bounds the answer of a
// subscribe and the socket's handshake (default 30 s); ReconnectDelay is the first wait
// before a reconnect (default 1 s), doubling with jitter up to MaximumReconnectDelay
// (default 30 s). No duration may be negative.
type Options struct {
	BaseURL               string
	APIKey                string
	HTTP                  *http.Client
	SocketURL             string
	RequestTimeout        time.Duration
	ReconnectDelay        time.Duration
	MaximumReconnectDelay time.Duration
}

// Client talks to one livewire API as the identity of one API key: REST requests and one
// socket for every subscription, opened on the first and closed after the last. Build it
// with New; it needs no Close. It is safe for concurrent use.
type Client struct {
	options Options
	filters *filterMemory
	live    *live
}

// New validates and copies options (DES-001).
func New(options *Options) (*Client, error) {
	if options == nil {
		return nil, errors.New("livewire client: New needs options")
	}
	resolved := *options
	if err := resolveOptions(&resolved); err != nil {
		return nil, err
	}
	client := &Client{options: resolved, filters: &filterMemory{documents: map[string]string{}}}
	client.live = newLive(client)
	return client, nil
}

func resolveOptions(options *Options) error {
	base, err := url.Parse(options.BaseURL)
	switch {
	case err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "":
		return fmt.Errorf("livewire client: BaseURL %q is no http or https URL", options.BaseURL)
	case strings.TrimSpace(options.APIKey) == "" || strings.ContainsAny(options.APIKey, "\r\n"):
		return errors.New("livewire client: APIKey is required and must be one line")
	case options.RequestTimeout < 0 || options.ReconnectDelay < 0 || options.MaximumReconnectDelay < 0:
		return errors.New("livewire client: no duration may be negative")
	}
	options.BaseURL = strings.TrimSuffix(options.BaseURL, "/")
	if options.SocketURL == "" {
		options.SocketURL = "ws" + strings.TrimPrefix(options.BaseURL, "http") + "/socket"
	}
	socket, err := url.Parse(options.SocketURL)
	if err != nil || (socket.Scheme != "ws" && socket.Scheme != "wss") || socket.Host == "" {
		return fmt.Errorf("livewire client: SocketURL %q is no ws or wss URL", options.SocketURL)
	}
	if options.HTTP == nil {
		options.HTTP = &http.Client{}
	}
	defaults := []struct {
		value    *time.Duration
		fallback time.Duration
	}{
		{&options.RequestTimeout, defaultRequestTimeout},
		{&options.ReconnectDelay, defaultReconnectDelay},
		{&options.MaximumReconnectDelay, defaultMaximumReconnectDelay},
	}
	for _, duration := range defaults {
		if *duration.value == 0 {
			*duration.value = duration.fallback
		}
	}
	return nil
}

// CallOptions are the options of one mutation, upload or batch. IdempotencyKey is the key
// the server records the call under (D16); empty, the call gets a fresh one. Send the same
// key on every retry of one call, so a call that ran answers its recorded response.
type CallOptions struct {
	IdempotencyKey string
}

// NewIdempotencyKey is a fresh Idempotency-Key: a UUID v7, monotonic within the process.
func NewIdempotencyKey() string {
	return NewUUIDv7().String()
}

// NewUUIDv7 is a fresh UUID version 7, monotonic within the process: the client id of a
// create that takes one (D18).
func NewUUIDv7() uuid.UUID {
	return uuid.Must(uuid.NewV7()) // fails only when crypto/rand does, which crashes the process first
}

// NewUUIDv4 is a fresh random UUID version 4.
func NewUUIDv4() uuid.UUID {
	return uuid.New()
}

func keyOf(options *CallOptions) string {
	if options == nil || options.IdempotencyKey == "" {
		return NewIdempotencyKey()
	}
	return options.IdempotencyKey
}

// Read sends GET path with parameters and decodes the answer into data (a pointer). A 404
// on a filterId the client stored or registered without a name (the server evicted it,
// D54) is retried once with the filter inline. A refusal is an *Error; a failure to reach
// the server wraps the http.Client's error.
func (c *Client) Read(ctx context.Context, path string, parameters Parameters, data any) error {
	return c.recovering(parameters, func(parameters Parameters) error {
		return c.send(ctx, &request{method: http.MethodGet, path: path, parameters: parameters}, data)
	})
}

// Call is one mutation as Client.Call sends it: the method, the route, its parameters, the
// JSON body (nil for none) and the Idempotency-Key ("" for a fresh one).
type Call struct {
	Method         string
	Path           string
	Parameters     Parameters
	Body           any
	IdempotencyKey string
}

// Call sends a mutation with its Idempotency-Key and decodes the answer into response
// (nil, or no content, decodes nothing).
func (c *Client) Call(ctx context.Context, call *Call, response any) error {
	outgoing := &request{method: call.Method, path: call.Path, parameters: call.Parameters, key: keyOf(&CallOptions{IdempotencyKey: call.IdempotencyKey})}
	if call.Body != nil {
		encoded, err := json.Marshal(call.Body)
		if err != nil {
			return fmt.Errorf("livewire: the body of %s %s: %w", call.Method, call.Path, err)
		}
		outgoing.body, outgoing.contentType, outgoing.size = bytes.NewReader(encoded), "application/json", int64(len(encoded))
	}
	return c.send(ctx, outgoing, response)
}

// Upload is one upload as Client.Upload sends it (D17): the route, its parameters, the file
// (its Body is read once and sent raw with its Size as the Content-Length and its
// ContentType, which must be one the upload declares; Filename is not sent) and the
// Idempotency-Key ("" for a fresh one).
type Upload struct {
	Path           string
	Parameters     Parameters
	File           livewire.File
	IdempotencyKey string
}

// Upload sends a file as a raw body with its Content-Length, so the server streams it to
// the handler (D60), and decodes the answer into response.
func (c *Client) Upload(ctx context.Context, upload *Upload, response any) error {
	if upload.File.Body == nil || upload.File.Size < 0 || upload.File.ContentType == "" {
		return errors.New("livewire: an upload needs a body, a size and a content type")
	}
	return c.send(ctx, &request{
		method: http.MethodPost, path: upload.Path, parameters: upload.Parameters, key: keyOf(&CallOptions{IdempotencyKey: upload.IdempotencyKey}),
		body: upload.File.Body, contentType: upload.File.ContentType, size: upload.File.Size,
	}, response)
}

// Batch runs several mutations in one transaction, all or nothing (D33, D47), under one
// Idempotency-Key, and answers one result per call in order.
func (c *Client) Batch(ctx context.Context, calls []livewire.BatchCall, options *CallOptions) ([]livewire.BatchResult, error) {
	var results []livewire.BatchResult
	err := c.Call(ctx, &Call{Method: http.MethodPost, Path: livewire.BatchPath, Body: calls, IdempotencyKey: keyOf(options)}, &results)
	return results, err
}

// SaveFilter stores a filter (D13). An unnamed one is remembered as it was sent, so a read
// of its id the server evicted sends it inline once (D54).
func (c *Client) SaveFilter(ctx context.Context, filterRequest *livewire.FilterRequest) (*livewire.FilterItem, error) {
	var stored livewire.FilterItem
	if err := c.Call(ctx, &Call{Method: http.MethodPost, Path: livewire.FiltersPath, Body: filterRequest}, &stored); err != nil {
		return nil, err
	}
	if filterRequest.Name == "" {
		c.filters.remember(stored.ID, string(filterRequest.Filter))
	}
	return &stored, nil
}

// ListFilters answers the caller's named filters of a resource (or "name.history"), of
// every one for "".
func (c *Client) ListFilters(ctx context.Context, resource string) (*livewire.ListResponse[livewire.FilterItem], error) {
	parameters := Parameters{}
	parameters.Set("resource", resource)
	var listed livewire.ListResponse[livewire.FilterItem]
	if err := c.Read(ctx, livewire.FiltersPath, parameters, &listed); err != nil {
		return nil, err
	}
	return &listed, nil
}

// DeleteFilter removes one of the caller's filters; one that is not there is no error.
func (c *Client) DeleteFilter(ctx context.Context, id string) error {
	c.filters.forget(id)
	return c.send(ctx, &request{method: http.MethodDelete, path: livewire.FiltersPath + "/{id}", parameters: Parameters{"id": id}}, nil)
}

// Export streams GET path (D35) into writer and answers the rows the server wrote. An
// export the server cut (D60) fails: its body breaks off or it ends without the
// Livewire-Export-Rows trailer; writer then holds part of the rows. A 404 on an evicted
// unnamed filterId is retried inline once (D54), before anything is written.
func (c *Client) Export(ctx context.Context, path string, parameters Parameters, writer io.Writer) (int, error) {
	rows := 0
	err := c.recovering(parameters, func(parameters Parameters) error {
		response, err := c.do(ctx, &request{method: http.MethodGet, path: path, parameters: parameters})
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if _, err := io.Copy(writer, response.Body); err != nil {
			return fmt.Errorf("livewire: GET %s: the export was cut off: %w", path, err)
		}
		written, err := strconv.Atoi(response.Trailer.Get(ExportRowsTrailer))
		if err != nil {
			return fmt.Errorf("livewire: GET %s: the export ended without its row count", path)
		}
		rows = written
		return nil
	})
	return rows, err
}

// request is one REST request: the route and its parameters, an Idempotency-Key ("" for
// none) and a body of a known size.
type request struct {
	method      string
	path        string
	parameters  Parameters
	key         string
	body        io.Reader
	contentType string
	size        int64
}

// send sends a request and decodes a successful answer into response, unless response is
// nil or the answer has no content.
func (c *Client) send(ctx context.Context, outgoing *request, response any) error {
	answer, err := c.do(ctx, outgoing)
	if err != nil {
		return err
	}
	defer func() { _ = answer.Body.Close() }()
	content, err := io.ReadAll(answer.Body)
	if err != nil {
		return fmt.Errorf("livewire: %s %s: the response was cut off: %w", outgoing.method, outgoing.path, err)
	}
	if response == nil || len(content) == 0 {
		return nil
	}
	if err := json.Unmarshal(content, response); err != nil {
		return fmt.Errorf("livewire: %s %s: the response does not decode: %w", outgoing.method, outgoing.path, err)
	}
	return nil
}

// do sends a request and answers the response of a success, whose body the caller closes;
// a refusal is an *Error.
func (c *Client) do(ctx context.Context, outgoing *request) (*http.Response, error) {
	target, err := route(outgoing.path, outgoing.parameters)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, outgoing.method, c.options.BaseURL+target, outgoing.body)
	if err != nil {
		return nil, fmt.Errorf("livewire: %s %s: %w", outgoing.method, outgoing.path, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.options.APIKey)
	httpRequest.Header.Set("Accept", "application/json")
	if outgoing.key != "" {
		httpRequest.Header.Set(IdempotencyKeyHeader, outgoing.key)
	}
	if outgoing.body != nil {
		httpRequest.ContentLength = outgoing.size
		httpRequest.Header.Set("Content-Type", outgoing.contentType)
	}
	response, err := c.options.HTTP.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("livewire: %s %s got no answer: %w", outgoing.method, outgoing.path, err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, maximumErrorBytes)) // a cut error body is still an error of its status
	return nil, errorOf(response.StatusCode, body)
}

// recovering runs a read and, when it answers 404 for a filterId whose document this
// client stored or registered without a name (the server evicted it, D54), runs it once
// more with that document inline.
func (c *Client) recovering(parameters Parameters, run func(Parameters) error) error {
	err := run(parameters)
	inline, ok := c.filters.inline(parameters, err)
	if !ok {
		return err
	}
	return run(inline)
}

// filterMemory keeps the documents of the unnamed filters a Client stored and its socket
// registered, by filterId, as they were sent (D54).
type filterMemory struct {
	mutex     sync.Mutex
	documents map[string]string
}

func (m *filterMemory) remember(id, document string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.documents[id] = document
}

func (m *filterMemory) forget(id string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	delete(m.documents, id)
}

// inline answers the parameters with the filterId replaced by its document inline when err
// is a 404 and the document is known.
func (m *filterMemory) inline(parameters Parameters, err error) (Parameters, bool) {
	var refusal *Error
	filterID, sent := parameters[livewire.ParameterFilterID]
	if !sent || !errors.As(err, &refusal) || refusal.Code != livewire.CodeNotFound {
		return nil, false
	}
	m.mutex.Lock()
	document, known := m.documents[filterID]
	m.mutex.Unlock()
	if !known {
		return nil, false
	}
	inline := Parameters{livewire.ParameterFilter: document}
	for name, value := range parameters {
		if name != livewire.ParameterFilterID {
			inline[name] = value
		}
	}
	return inline, true
}
