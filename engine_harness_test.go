package livewire_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/govengers/auth"
	"github.com/govengers/auth/authorization"
	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/query"
)

const (
	roleReader = "reader"
	roleWriter = "writer"
	// frameWait bounds every wait of a test on the socket.
	frameWait = 5 * time.Second
)

type groupParameters struct {
	Group string `query:"group"`
}

type countItem struct {
	Count int `json:"count"`
}

type createGadget struct {
	Name  string `json:"name" validate:"required"`
	Group string `json:"group"`
}

type blockParameters struct {
	Wait string `query:"wait"`
}

type uploadParameters struct{}

type uploadAnswer struct {
	Bytes int64 `json:"bytes"`
}

// testbed is one engine behind an httptest server with every fake wired in.
type testbed struct {
	t           *testing.T
	world       *world
	feed        *fakeFeed
	identities  *fakeIdentities
	transactor  *fakeTransactor
	filters     *fakeFilters
	idempotency *fakeIdempotency
	presences   *fakePresences
	outcomes    *outcomes
	engine      *livewire.Engine
	server      *httptest.Server
	// release unblocks the mutation "blockGadget".
	release chan struct{}
}

// newTestbed starts an engine; adjust changes the options before NewEngine.
func newTestbed(t *testing.T, adjust func(options *livewire.EngineOptions)) *testbed {
	t.Helper()
	feed := newFakeFeed()
	bed := &testbed{
		t: t, world: newWorld(feed), feed: feed, identities: newFakeIdentities(),
		filters: newFakeFilters(), idempotency: newFakeIdempotency(), presences: newFakePresences(feed),
		outcomes: &outcomes{}, release: make(chan struct{}),
	}
	bed.transactor = &fakeTransactor{world: bed.world}
	bed.identities.set(person("ada", roleReader, roleWriter))
	options := &livewire.EngineOptions{
		Registry: bed.registry(), ChangeFeed: feed, IdentitySource: bed.identities, Transactor: bed.transactor,
		FilterStore: bed.filters, IdempotencyStore: bed.idempotency, PresenceStore: bed.presences,
		AllowedOrigins: []string{"https://app.example"}, DeliveryBatchWait: 10 * time.Millisecond, RetryInterval: 5 * time.Millisecond,
	}
	if adjust != nil {
		adjust(options)
	}
	engine, err := livewire.NewEngine(options)
	require.NoError(t, err)
	bed.engine = engine
	bed.server = httptest.NewServer(identified(bed.identities, engine.Handler()))
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { _ = engine.Run(ctx) })
	t.Cleanup(func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), frameWait)
		defer closeCancel()
		require.NoError(t, engine.Close(closeContext))
		bed.server.Close()
		cancel()
		running.Wait()
	})
	if options.ChangeFeed != nil {
		bed.waitConnected()
	}
	return bed
}

func (b *testbed) waitConnected() {
	b.t.Helper()
	select {
	case <-b.feed.connected:
	case <-time.After(frameWait):
		b.t.Fatal("the change feed never connected")
	}
}

func person(subject string, roles ...string) *auth.Identity {
	return &auth.Identity{Provider: "test", Subject: subject, Name: strings.ToUpper(subject[:1]) + subject[1:], Active: true, Roles: roles}
}

func (b *testbed) registry() *livewire.Registry {
	registry, err := livewire.NewRegistry(&livewire.RegistryOptions{Observer: b.outcomes.observe})
	require.NoError(b.t, err)
	require.NoError(b.t, registry.Add(b.gadgets(), b.groupList(), b.counter(), b.createGadget(), b.failGadget(), b.blockGadget(), b.panicGadget(), b.removeGadget(), b.upload()))
	return registry
}

func gadgetFields() query.Fields {
	return query.Fields{
		{Name: "id", Type: query.TypeString, SQL: "gadgets.id", Filter: true, Sort: true},
		{Name: "name", Type: query.TypeText, SQL: "gadgets.name", Filter: true, Sort: true, Search: true},
		{Name: "group", Type: query.TypeString, SQL: "gadgets.group", Filter: true, Sort: true},
		{Name: "createdAt", Type: query.TypeTime, SQL: "gadgets.created_at", Filter: true, Sort: true},
		{Name: "secret", Type: query.TypeString, SQL: "gadgets.secret", Read: authorization.IsAdmin()},
	}
}

func (b *testbed) gadgets() *livewire.Resource {
	return livewire.NewResource("gadgets", "/gadgets", &livewire.ResourceOptions[gadget]{
		Tables: []string{"gadgets"},
		Read:   authorization.HasRole(roleReader),
		Fields: gadgetFields(),
		Item:   &livewire.ItemKey{Parameter: "id", Field: "id"},
		List: func(ctx context.Context, _ *query.ListQuery) ([]gadget, int, error) {
			if err := b.world.load(ctx); err != nil {
				return nil, 0, err
			}
			items := b.world.sorted("")
			return items, len(items), nil
		},
		Get: func(ctx context.Context, listQuery *query.ListQuery) (*gadget, error) {
			if err := b.world.load(ctx); err != nil {
				return nil, err
			}
			_, arguments := listQuery.PageSQL(&query.ResourceSQL{From: "gadgets", IDColumn: "gadgets.id"})
			for _, argument := range arguments {
				if id, ok := argument.(string); ok {
					if item, found := b.world.lookup(id); found {
						return &item, nil
					}
				}
			}
			return nil, livewire.NotFound()
		},
		Distinct: func(ctx context.Context, field string, _ *query.ListQuery) ([]livewire.DistinctValue, error) {
			counts := map[string]int{}
			for _, item := range b.world.sorted("") {
				counts[item.Group]++
			}
			values := make([]livewire.DistinctValue, 0, len(counts))
			for _, group := range []string{"a", "b", "c"} {
				if counts[group] > 0 {
					values = append(values, livewire.DistinctValue{Value: group, Count: counts[group]})
				}
			}
			return values, nil
		},
	})
}

// groupList is the typed-parameters list of one group, partitioned by it (D25).
func (b *testbed) groupList() *livewire.List {
	return livewire.NewList("groupGadgets", "/groups", &livewire.ListOptions[gadget, groupParameters]{
		Tables:    []string{"gadgets"},
		Read:      authorization.HasRole(roleReader),
		Partition: func(_ context.Context, parameters any) string { return parameters.(*groupParameters).Group },
		Load: func(ctx context.Context, parameters *groupParameters) ([]gadget, error) {
			if err := b.world.load(ctx); err != nil {
				return nil, err
			}
			return b.world.sorted(parameters.Group), nil
		},
	})
}

func (b *testbed) counter() *livewire.Object {
	return livewire.NewObject("gadgetCount", "/gadget-count", &livewire.ObjectOptions[countItem, livewire.NoParameters]{
		Tables: []string{"gadgets"},
		Read:   authorization.HasRole(roleReader),
		Load: func(ctx context.Context, _ *livewire.NoParameters) (*countItem, error) {
			if err := b.world.load(ctx); err != nil {
				return nil, err
			}
			return &countItem{Count: len(b.world.sorted(""))}, nil
		},
	})
}

func (b *testbed) createGadget() *livewire.Mutation {
	return livewire.NewMutation("createGadget", http.MethodPost, "/gadgets", &livewire.MutationOptions[livewire.NoParameters, createGadget, gadget]{
		Check:  authorization.HasRole(roleWriter),
		Status: http.StatusCreated,
		Handle: func(ctx context.Context, _ *livewire.NoParameters, body *createGadget) (*gadget, error) {
			item := gadget{ID: "g-" + body.Name, Name: body.Name, Group: body.Group}
			if _, exists := b.world.lookup(item.ID); exists {
				return nil, livewire.Conflict()
			}
			transactionOf(ctx).stage(func() { b.world.put(item) }, livewire.Change{Table: "gadgets", Operation: livewire.ChangeInsert, ID: item.ID, Partition: item.Group})
			return &item, nil
		},
	})
}

type idParameters struct {
	ID string `path:"id"`
}

func (b *testbed) removeGadget() *livewire.Mutation {
	return livewire.NewMutation("removeGadget", http.MethodDelete, "/gadgets/{id}", &livewire.MutationOptions[idParameters, livewire.NoBody, livewire.NoContent]{
		Check: authorization.HasRole(roleWriter),
		Handle: func(ctx context.Context, parameters *idParameters, _ *livewire.NoBody) (*livewire.NoContent, error) {
			transactionOf(ctx).stage(func() { b.world.remove(parameters.ID) }, livewire.Change{Table: "gadgets", Operation: livewire.ChangeDelete, ID: parameters.ID})
			return &livewire.NoContent{}, nil
		},
	})
}

func (b *testbed) failGadget() *livewire.Mutation {
	return livewire.NewMutation("failGadget", http.MethodPost, "/gadgets/fail", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
		Check: authorization.HasRole(roleWriter),
		Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
			return nil, livewire.Conflict()
		},
	})
}

// blockGadget waits for the testbed's release, so a call stays in flight.
func (b *testbed) blockGadget() *livewire.Mutation {
	return livewire.NewMutation("blockGadget", http.MethodPost, "/gadgets/block", &livewire.MutationOptions[blockParameters, livewire.NoBody, countItem]{
		Check: authorization.HasRole(roleWriter),
		Handle: func(ctx context.Context, _ *blockParameters, _ *livewire.NoBody) (*countItem, error) {
			select {
			case <-b.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &countItem{Count: 1}, nil
		},
	})
}

func (b *testbed) panicGadget() *livewire.Mutation {
	return livewire.NewMutation("panicGadget", http.MethodPost, "/gadgets/panic", &livewire.MutationOptions[livewire.NoParameters, livewire.NoBody, livewire.NoContent]{
		Check: authorization.HasRole(roleWriter),
		Handle: func(context.Context, *livewire.NoParameters, *livewire.NoBody) (*livewire.NoContent, error) {
			panic("the handler panicked")
		},
	})
}

func (b *testbed) upload() *livewire.Upload {
	return livewire.NewUpload("uploadPicture", "/pictures", &livewire.UploadOptions[uploadParameters, uploadAnswer]{
		Check:        authorization.HasRole(roleWriter),
		ContentTypes: []string{"image/png"},
		MaximumBytes: 1024,
		Handle: func(_ context.Context, _ *uploadParameters, file *livewire.File) (*uploadAnswer, error) {
			return &uploadAnswer{Bytes: file.Size}, nil
		},
	})
}

// client is one test socket. A reader goroutine hands every frame over; pushes read while
// waiting for a response are kept in order.
type client struct {
	t          *testing.T
	connection *websocket.Conn
	next       int
	frames     chan []byte
	ended      chan error
	pushes     []livewire.PushFrame
	responses  map[string]livewire.ResponseFrame
}

// dial opens a socket as subject without an Origin header (a non-browser client).
func (b *testbed) dial(subject string) *client {
	b.t.Helper()
	connection, response, err := b.dialRaw(subject, http.Header{})
	require.NoError(b.t, err)
	require.NoError(b.t, response.Body.Close())
	socket := &client{t: b.t, connection: connection, frames: make(chan []byte, 4096), ended: make(chan error, 1), responses: map[string]livewire.ResponseFrame{}}
	var reader sync.WaitGroup
	reader.Go(func() {
		for {
			_, data, err := connection.ReadMessage()
			if err != nil {
				socket.ended <- err
				return
			}
			socket.frames <- data
		}
	})
	b.t.Cleanup(func() {
		_ = connection.Close()
		reader.Wait()
	})
	return socket
}

func (b *testbed) dialRaw(subject string, header http.Header) (*websocket.Conn, *http.Response, error) {
	header.Set(subjectHeader, subject)
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(b.server.URL, "http"), header)
}

func (c *client) send(operation livewire.Operation, target string, parameters map[string]string, body any) string {
	c.t.Helper()
	c.next++
	id := strconv.Itoa(c.next)
	frame := livewire.RequestFrame{ID: id, Operation: operation, Target: target, Parameters: parameters}
	if body != nil {
		frame.Body = mustJSON(body)
	}
	require.NoError(c.t, c.connection.WriteJSON(frame))
	return id
}

func (c *client) sendFrame(frame livewire.RequestFrame) string {
	c.t.Helper()
	c.next++
	frame.ID = strconv.Itoa(c.next)
	require.NoError(c.t, c.connection.WriteJSON(frame))
	return frame.ID
}

func (c *client) do(operation livewire.Operation, target string, parameters map[string]string, body any) livewire.ResponseFrame {
	c.t.Helper()
	return c.response(c.send(operation, target, parameters, body))
}

// readOne takes one frame into the pushes or the responses, waiting at most wait.
func (c *client) readOne(wait time.Duration) error {
	var data []byte
	select {
	case data = <-c.frames:
	case err := <-c.ended:
		c.ended <- err
		return err
	case <-time.After(wait):
		return errFrameTimeout
	}
	var probe map[string]json.RawMessage
	require.NoError(c.t, json.Unmarshal(data, &probe))
	if _, isPush := probe["sub"]; isPush {
		var push livewire.PushFrame
		require.NoError(c.t, json.Unmarshal(data, &push))
		c.pushes = append(c.pushes, push)
		return nil
	}
	var response livewire.ResponseFrame
	require.NoError(c.t, json.Unmarshal(data, &response))
	c.responses[response.ID] = response
	return nil
}

var errFrameTimeout = errors.New("no frame arrived in time")

func (c *client) response(id string) livewire.ResponseFrame {
	c.t.Helper()
	for {
		if response, ok := c.responses[id]; ok {
			delete(c.responses, id)
			return response
		}
		require.NoError(c.t, c.readOne(frameWait))
	}
}

func (c *client) push() livewire.PushFrame {
	c.t.Helper()
	for len(c.pushes) == 0 {
		require.NoError(c.t, c.readOne(frameWait))
	}
	push := c.pushes[0]
	c.pushes = c.pushes[1:]
	return push
}

// closeCode reads until the socket ends and returns the close code.
func (c *client) closeCode() int {
	c.t.Helper()
	for {
		err := c.readOne(frameWait)
		if err == nil {
			continue
		}
		var closeError *websocket.CloseError
		if errors.As(err, &closeError) {
			return closeError.Code
		}
		c.t.Fatalf("the socket ended without a close frame: %v", err)
	}
}

// noPush checks that no frame arrives within the duration.
func (c *client) noPush(duration time.Duration) {
	c.t.Helper()
	require.Empty(c.t, c.pushes)
	select {
	case data := <-c.frames:
		c.t.Fatalf("an unexpected frame arrived: %s", data)
	case <-time.After(duration):
	}
}

func subscribed(t *testing.T, response livewire.ResponseFrame) string {
	t.Helper()
	require.True(t, response.OK, "the subscribe failed: %+v", response.Error)
	require.NotEmpty(t, response.Subscription)
	return response.Subscription
}

func listOf(t *testing.T, payload json.RawMessage) livewire.ListResponse[gadget] {
	t.Helper()
	var list livewire.ListResponse[gadget]
	require.NoError(t, json.Unmarshal(payload, &list))
	return list
}

func changeOf(t *testing.T, payload json.RawMessage) livewire.ListChange {
	t.Helper()
	var change livewire.ListChange
	require.NoError(t, json.Unmarshal(payload, &change))
	return change
}

func errorOf(t *testing.T, payload json.RawMessage) livewire.ErrorBody {
	t.Helper()
	var body livewire.ErrorBody
	require.NoError(t, json.Unmarshal(payload, &body))
	return body
}
