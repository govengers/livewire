package livewiretest

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/govengers/livewire"
)

// ParityOptions configures Parity. Application and Authenticate are required: Authenticate
// makes a request the fixture identity's (an API key header, a session cookie) and runs for
// every REST request and the socket's upgrade. Reads gives the parameters of the reads that
// need them (an item's key, a distinct read's field, a presence read's key); a read that needs
// none runs without parameters when Reads has none for it, and several Reads for one target
// each run.
type ParityOptions struct {
	Application  Application
	Authenticate func(request *http.Request)
	Reads        []Read
}

// Parity fails the test for every read of the registry whose REST body differs from its
// socket payload byte for byte (D23): the answer of a socket query, and for a live read the
// first push of a subscribe too, each against the REST body of the same read as the same
// identity, through the application's Handler. It covers every read kind of every declaration
// (a resource's list, item, distinct and history reads, a list's and an object's read, and
// the presence reads when the engine serves them); a read that needs parameters and has no
// Read fails the gate, as does a read the application answers with anything but 200.
func Parity(tb testing.TB, options *ParityOptions) {
	tb.Helper()
	if options == nil {
		tb.Fatalf("livewiretest: Parity needs options")
		return
	}
	server := startServer(tb, "Parity", &options.Application, options.Authenticate)
	defer server.close()
	reads := resolveReads(tb, "Parity", readTargets(options.Application.Engine), options.Reads)
	ctx := context.Background()
	connection, err := server.dial(ctx)
	if err != nil {
		tb.Fatalf("livewiretest: Parity: %v", err)
		return
	}
	defer connection.close()
	for _, read := range reads {
		if err := checkParity(ctx, server, connection, read); err != nil {
			tb.Errorf("livewiretest: Parity: %s %v: %v", read.target.name(), read.parameters, err)
		}
	}
}

// checkParity compares one read's REST body with its socket query payload and, for a live
// read, with the first push of a subscribe, which it unsubscribes after.
func checkParity(ctx context.Context, server *testServer, connection *socket, read resolvedRead) error {
	rest, err := server.read(ctx, read)
	if err != nil {
		return err
	}
	queried, err := connection.payload(livewire.OperationQuery, read)
	if err != nil {
		return err
	}
	if !bytes.Equal(rest, queried.Payload) {
		return fmt.Errorf("the REST body and the socket query's payload differ\n%s", byteDifference(rest, queried.Payload))
	}
	if !read.target.live {
		return nil
	}
	subscribed, err := connection.payload(livewire.OperationSubscribe, read)
	if err != nil {
		return err
	}
	first, err := connection.push(subscribed.Subscription, time.Now().Add(requestTimeout))
	if err != nil {
		return fmt.Errorf("the first push of the subscription: %w", err)
	}
	if _, err := connection.request(livewire.OperationUnsubscribe, subscribed.Subscription, nil); err != nil {
		return err
	}
	if !bytes.Equal(rest, first.Payload) {
		return fmt.Errorf("the REST body and the subscription's first push (%s) differ\n%s", first.Kind, byteDifference(rest, first.Payload))
	}
	return nil
}

// differenceContext is how many bytes around the first difference a message shows.
const differenceContext = 60

// byteDifference shows where two bodies first differ, with some bytes before it.
func byteDifference(rest, socket []byte) string {
	offset := 0
	for offset < len(rest) && offset < len(socket) && rest[offset] == socket[offset] {
		offset++
	}
	start := max(0, offset-differenceContext)
	excerpt := func(body []byte) string {
		return string(body[start:min(len(body), offset+differenceContext)])
	}
	return fmt.Sprintf("at byte %d (of %d and %d):\nREST:   ...%s\nsocket: ...%s", offset, len(rest), len(socket), excerpt(rest), excerpt(socket))
}
