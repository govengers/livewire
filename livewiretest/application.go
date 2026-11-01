package livewiretest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/govengers/livewire"
)

// requestTimeout bounds every request a gate sends and every answer it waits for.
const requestTimeout = 10 * time.Second

// Application is the application under test as its clients reach it: Handler is its router,
// as its main builds it, with the authentication, the REST routes ginadapter.RegisterRoutes
// mounts below the registry's BasePath and the socket at SocketPath ("/api/v1/socket"), all
// in front of Engine, whose registry names the reads the gates run.
type Application struct {
	Engine     *livewire.Engine
	Handler    http.Handler
	SocketPath string
}

// Read is one read a gate runs: the target as the socket names it (D46: "todos",
// "todos.item", "todos.distinct", "todos.history", "todos.presence") and its parameters as both
// transports carry them, one text per name; a path parameter fills the route's placeholder,
// every other one is a query parameter on REST.
type Read struct {
	Target     string
	Parameters map[string]string
}

// readTarget is one read kind of a declaration: its socket target, its REST route below the
// BasePath, whether a request needs parameters to name it, and whether it is live.
type readTarget struct {
	declaration livewire.Declaration
	kind        livewire.ReadKind
	route       string
	needsInput  bool
	live        bool
}

func (r readTarget) name() string {
	if r.kind == livewire.ReadMain {
		return r.declaration.Name()
	}
	return r.declaration.Name() + "." + string(r.kind)
}

// readTargets lists every read of the engine's registry in declaration order: a resource's
// list, item, distinct, history and presence reads as far as it declares them (presence only
// when the engine serves it), a list's and an object's read and an object's presence read.
func readTargets(engine *livewire.Engine) []readTarget {
	var targets []readTarget
	presence := engine.ServesPresence()
	for _, declaration := range engine.Registry().Declarations() {
		live := len(declaration.Tables()) > 0
		switch typed := declaration.(type) {
		case *livewire.Resource:
			targets = append(targets, readTarget{declaration: typed, kind: livewire.ReadMain, route: typed.Path(), live: live})
			for _, extra := range []struct {
				kind  livewire.ReadKind
				route string
			}{
				{livewire.ReadItem, typed.ItemPath()},
				{livewire.ReadDistinct, typed.DistinctPath()},
				{livewire.ReadHistory, typed.HistoryPath()},
				{livewire.ReadPresence, presencePath(presence, typed.PresencePath())},
			} {
				if extra.route != "" {
					targets = append(targets, readTarget{declaration: typed, kind: extra.kind, route: extra.route, needsInput: true, live: live || extra.kind == livewire.ReadPresence})
				}
			}
		case *livewire.List:
			targets = append(targets, readTarget{declaration: typed, kind: livewire.ReadMain, route: typed.Path(), needsInput: needsParameters(typed), live: live})
		case *livewire.Object:
			targets = append(targets, readTarget{declaration: typed, kind: livewire.ReadMain, route: typed.Path(), needsInput: needsParameters(typed), live: live})
			if route := presencePath(presence, typed.PresencePath()); route != "" {
				targets = append(targets, readTarget{declaration: typed, kind: livewire.ReadPresence, route: route, needsInput: true, live: true})
			}
		default: // mutations and uploads are writes
		}
	}
	return targets
}

func presencePath(served bool, route string) string {
	if !served {
		return ""
	}
	return route
}

func needsParameters(declaration livewire.Declaration) bool {
	return slices.ContainsFunc(declaration.Parameters(), livewire.Parameter.Required)
}

// resolvedRead is one read a gate runs: its target and the parameters it runs with.
type resolvedRead struct {
	target     readTarget
	parameters map[string]string
}

// resolveReads pairs every target the gate covers with the reads the caller gave for it, or,
// for a target that needs no parameters, with a read without any. A target that needs
// parameters but has no read, and a read whose target is not one of the targets, fail the
// gate, which names them.
func resolveReads(tb testing.TB, gate string, targets []readTarget, reads []Read) []resolvedRead {
	tb.Helper()
	var resolved []resolvedRead
	for _, read := range reads {
		if !slices.ContainsFunc(targets, func(target readTarget) bool { return target.name() == read.Target }) {
			tb.Errorf("livewiretest: %s: the read %q names no read target of the registry this gate covers", gate, read.Target)
		}
	}
	for _, target := range targets {
		given := false
		for _, read := range reads {
			if read.Target == target.name() {
				given = true
				resolved = append(resolved, resolvedRead{target: target, parameters: read.Parameters})
			}
		}
		switch {
		case given:
		case target.needsInput:
			tb.Errorf("livewiretest: %s: %s needs parameters: add a Read for it with the parameters of a fixture row", gate, target.name())
		default:
			resolved = append(resolved, resolvedRead{target: target})
		}
	}
	return resolved
}

// testServer serves the application for one gate and sends its requests as one identity.
type testServer struct {
	server       *httptest.Server
	basePath     string
	socketPath   string
	authenticate func(request *http.Request)
}

// startServer serves application on a local test server until the gate ends.
func startServer(tb testing.TB, gate string, application *Application, authenticate func(request *http.Request)) *testServer {
	tb.Helper()
	switch {
	case application.Engine == nil || application.Handler == nil:
		tb.Fatalf("livewiretest: %s: Application needs the Engine and the Handler", gate)
	case !strings.HasPrefix(application.SocketPath, "/"):
		tb.Fatalf("livewiretest: %s: Application.SocketPath %q must start with a slash", gate, application.SocketPath)
	case authenticate == nil:
		tb.Fatalf("livewiretest: %s: the options need an Authenticate function", gate)
	}
	return &testServer{
		server:       httptest.NewServer(application.Handler),
		basePath:     application.Engine.Registry().BasePath(),
		socketPath:   application.SocketPath,
		authenticate: authenticate,
	}
}

func (s *testServer) close() { s.server.Close() }

// as is the server sending as another identity.
func (s *testServer) as(authenticate func(request *http.Request)) *testServer {
	other := *s
	other.authenticate = authenticate
	return &other
}

// restURL is the REST URL of a route with parameters: path placeholders filled in, every other
// parameter in the query string.
func (s *testServer) restURL(route string, parameters map[string]string) string {
	queryValues := url.Values{}
	for name, value := range parameters {
		placeholder := "{" + name + "}"
		if strings.Contains(route, placeholder) {
			route = strings.ReplaceAll(route, placeholder, url.PathEscape(value))
			continue
		}
		queryValues.Set(name, value)
	}
	address := s.server.URL + s.basePath + route
	if len(queryValues) > 0 {
		address += "?" + queryValues.Encode()
	}
	return address
}

// get sends GET address as the server's identity and answers the status and the body.
func (s *testServer) get(ctx context.Context, address string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, nil, err
	}
	s.authenticate(request)
	response, err := s.server.Client().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }() // the body was read or the read failed; closing adds nothing
	body, err := io.ReadAll(response.Body)
	return response.StatusCode, body, err
}

// read runs a read over REST and requires 200.
func (s *testServer) read(ctx context.Context, read resolvedRead) ([]byte, error) {
	status, body, err := s.get(ctx, s.restURL(read.target.route, read.parameters))
	if err != nil {
		return nil, fmt.Errorf("REST read: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("REST answered %d %s", status, abbreviate(body))
	}
	return body, nil
}

// abbreviate is a body short enough for a failure message.
func abbreviate(body []byte) string {
	const maximum = 300
	if len(body) <= maximum {
		return string(body)
	}
	return string(body[:maximum]) + "..."
}
