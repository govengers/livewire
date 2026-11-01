package livewiretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/govengers/livewire"
)

// socket is one socket of the application, used by one gate's goroutine: a reader goroutine
// hands every frame over, and the gate sorts them into responses and pushes per subscription.
type socket struct {
	connection *websocket.Conn
	frames     chan []byte
	failed     chan error
	closed     chan struct{}
	responses  map[string]livewire.ResponseFrame
	pushes     map[string][]livewire.PushFrame
	sequence   int
}

// dial opens the application's socket as the server's identity. The upgrade carries the
// server's own Origin, which the engine always allows (D9), so a cookie session passes.
func (s *testServer) dial(ctx context.Context) (*socket, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.server.URL+s.socketPath, nil)
	if err != nil {
		return nil, err
	}
	s.authenticate(request)
	header := request.Header.Clone()
	if header.Get("Origin") == "" {
		header.Set("Origin", s.server.URL)
	}
	address := "ws" + strings.TrimPrefix(s.server.URL, "http") + s.socketPath
	connection, response, err := websocket.DefaultDialer.DialContext(ctx, address, header)
	if response != nil {
		_ = response.Body.Close() // the handshake's body is empty or an error body; the status says enough
	}
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("the socket handshake answered %d: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("the socket handshake: %w", err)
	}
	opened := &socket{
		connection: connection, frames: make(chan []byte, 256), failed: make(chan error, 1), closed: make(chan struct{}),
		responses: map[string]livewire.ResponseFrame{}, pushes: map[string][]livewire.PushFrame{},
	}
	go opened.receive()
	return opened, nil
}

// receive hands every frame to the gate until the socket ends; it ends with the socket, which
// close ends, so it outlives no gate.
func (s *socket) receive() {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.failed <- fmt.Errorf("the socket reader panicked: %v", recovered)
		}
	}()
	for {
		_, frame, err := s.connection.ReadMessage()
		if err != nil {
			s.failed <- err
			return
		}
		select {
		case s.frames <- frame:
		case <-s.closed:
			return
		}
	}
}

func (s *socket) close() {
	close(s.closed)
	_ = s.connection.Close() // the gate is done with the socket; a failed close changes nothing
}

// next takes the next frame off the socket and files it; it fails when the socket ended or
// the deadline passed.
func (s *socket) next(deadline time.Time) error {
	select {
	case frame := <-s.frames: // a frame that arrived is filed even past the deadline
		return s.file(frame)
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case frame := <-s.frames:
		return s.file(frame)
	case err := <-s.failed:
		s.failed <- err // every later wait fails the same way
		return fmt.Errorf("the socket ended: %w", err)
	case <-timer.C:
		return errWaitedTooLong
	}
}

// errWaitedTooLong is a wait that passed its deadline.
var errWaitedTooLong = errors.New("no answer in time")

// file sorts a frame into the responses or the pushes of its subscription.
func (s *socket) file(frame []byte) error {
	var kind struct {
		ID           string `json:"id"`
		Subscription string `json:"sub"`
	}
	if err := json.Unmarshal(frame, &kind); err != nil {
		return fmt.Errorf("a socket frame is no JSON object: %w", err)
	}
	if kind.Subscription != "" {
		var push livewire.PushFrame
		if err := json.Unmarshal(frame, &push); err != nil {
			return fmt.Errorf("a push frame: %w", err)
		}
		s.pushes[push.Subscription] = append(s.pushes[push.Subscription], push)
		return nil
	}
	var response livewire.ResponseFrame
	if err := json.Unmarshal(frame, &response); err != nil {
		return fmt.Errorf("a response frame: %w", err)
	}
	s.responses[response.ID] = response
	return nil
}

// request sends one request and waits for its response.
func (s *socket) request(operation livewire.Operation, target string, parameters map[string]string) (livewire.ResponseFrame, error) {
	s.sequence++
	id := strconv.Itoa(s.sequence)
	frame, err := json.Marshal(livewire.RequestFrame{ID: id, Operation: operation, Target: target, Parameters: parameters})
	if err != nil {
		return livewire.ResponseFrame{}, err
	}
	if err := s.connection.WriteMessage(websocket.TextMessage, frame); err != nil {
		return livewire.ResponseFrame{}, fmt.Errorf("send the %s request: %w", operation, err)
	}
	deadline := time.Now().Add(requestTimeout)
	for {
		if response, answered := s.responses[id]; answered {
			delete(s.responses, id)
			return response, nil
		}
		if err := s.next(deadline); err != nil {
			return livewire.ResponseFrame{}, fmt.Errorf("the %s of %s: %w", operation, target, err)
		}
	}
}

// payload runs a query or a subscribe and answers the response, failing on a refusal.
func (s *socket) payload(operation livewire.Operation, read resolvedRead) (livewire.ResponseFrame, error) {
	response, err := s.request(operation, read.target.name(), read.parameters)
	if err != nil {
		return response, err
	}
	if !response.OK {
		body, _ := json.Marshal(response.Error) // an ErrorBody always marshals
		return response, fmt.Errorf("the socket's %s answered %d %s", operation, response.Status, body)
	}
	return response, nil
}

// push waits for the next push of a subscription.
func (s *socket) push(subscription string, deadline time.Time) (livewire.PushFrame, error) {
	for {
		if waiting := s.pushes[subscription]; len(waiting) > 0 {
			s.pushes[subscription] = waiting[1:]
			return waiting[0], nil
		}
		if err := s.next(deadline); err != nil {
			return livewire.PushFrame{}, err
		}
	}
}
