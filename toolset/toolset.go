// Package toolset runs the tool catalogue of a registry (generate.Catalogue, D85) in process:
// an agent's tool call becomes Engine.Read, Engine.Call or Engine.Upload under the identity in
// the context, so authorization, scope, column checks, audit and idempotency are exactly the
// API's. A Toolset selects the tools one agent gets (include and exclude lists, read-only
// mode), asks a confirmation hook before a call that changes data, caps the answer's size and
// marks every call as the agent's on behalf of the identity, which the audit records as the
// real actor (D86). Its refusals are *Error values with a closed code, never internal text.
//
// The adapters register a Toolset with an agent framework: aikidoadapter (govengers/aikido
// tools) and mcpadapter (a Model Context Protocol server), each a module of its own.
package toolset

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/auth"

	"github.com/govengers/livewire"
	"github.com/govengers/livewire/generate"
)

// AgentProvider is the provider of the identity a Toolset puts behind the caller's (D86): the
// audit's real actor of an agent's call is "agent:<Options.Agent>".
const AgentProvider = "agent"

// DefaultMaximumResultBytes is the answer size cap when Options.MaximumResultBytes is zero.
const DefaultMaximumResultBytes = 64 << 10

// Codes of a Toolset's own refusals, beside livewire's closed set (D86).
const (
	// CodeDeclined: the confirmation hook declined a call that changes data; nothing ran.
	CodeDeclined livewire.Code = "declined"
	// CodeTooLarge: a read's answer is larger than the cap; the agent asks for less.
	CodeTooLarge livewire.Code = "too_large"
)

// Options configures a Toolset. Engine and Catalogue are required, the catalogue of the
// engine's registry, and so is Agent, the name the audit shows as the real actor of every
// call (letters, digits, dots, dashes and underscores). Include lists the tools the agent
// gets (empty: every tool), Exclude the tools it does not get; a name either lists that is no
// tool of the catalogue is refused. ExcludeReads drops the reads of these kinds without
// listing their names (livewire.ReadDistinct, ReadHistory and ReadPresence answer what a
// person rarely asks an agent for and cost the most schema). ReadOnly drops every tool that
// changes data. Confirm, when
// set, runs before every call that changes data: an error declines the call (CodeDeclined)
// and nothing runs. MaximumResultBytes caps an answer (0: DefaultMaximumResultBytes, negative:
// no cap): a read past it is refused with CodeTooLarge, a write past it, which committed,
// answers a notice instead of its body.
type Options struct {
	Engine             *livewire.Engine
	Catalogue          *generate.Catalogue
	Agent              string
	Include            []string
	Exclude            []string
	ExcludeReads       []livewire.ReadKind
	ReadOnly           bool
	Confirm            func(ctx context.Context, call *Call) error
	MaximumResultBytes int
}

// Call is one tool call as the confirmation hook sees it: the tool and the model's arguments.
type Call struct {
	Tool      generate.Tool
	Arguments json.RawMessage
}

// Toolset is the tools one agent may call. It is immutable and safe for concurrent use.
type Toolset struct {
	options Options
	tools   []generate.Tool
	members map[string]map[string]bool // the argument members each tool accepts
}

// New validates and copies options (DES-001) and selects the tools.
func New(options *Options) (*Toolset, error) {
	switch {
	case options == nil || options.Engine == nil || options.Catalogue == nil:
		return nil, errors.New("toolset: Options needs an Engine and a Catalogue")
	case options.Catalogue.Registry() != options.Engine.Registry():
		return nil, errors.New("toolset: the Catalogue describes another registry than the Engine's")
	case !validAgent(options.Agent):
		return nil, fmt.Errorf("toolset: Options.Agent %q must be a name of letters, digits, dots, dashes and underscores", options.Agent)
	}
	resolved := *options
	resolved.Include, resolved.Exclude = slices.Clone(options.Include), slices.Clone(options.Exclude)
	resolved.ExcludeReads = slices.Clone(options.ExcludeReads)
	for _, kind := range resolved.ExcludeReads {
		if !slices.Contains([]livewire.ReadKind{livewire.ReadMain, livewire.ReadItem, livewire.ReadDistinct, livewire.ReadHistory, livewire.ReadPresence}, kind) {
			return nil, fmt.Errorf("toolset: Options.ExcludeReads has %q, which is no read kind", kind)
		}
	}
	if resolved.MaximumResultBytes == 0 {
		resolved.MaximumResultBytes = DefaultMaximumResultBytes
	}
	for _, name := range slices.Concat(resolved.Include, resolved.Exclude) {
		if _, ok := resolved.Catalogue.Lookup(name); !ok {
			return nil, fmt.Errorf("toolset: %q is no tool of the catalogue", name)
		}
	}
	toolset := &Toolset{options: resolved, members: map[string]map[string]bool{}}
	for _, tool := range resolved.Catalogue.Tools() {
		switch {
		case len(resolved.Include) > 0 && !slices.Contains(resolved.Include, tool.Name),
			slices.Contains(resolved.Exclude, tool.Name),
			tool.Kind == generate.ToolRead && slices.Contains(resolved.ExcludeReads, tool.Read),
			resolved.ReadOnly && tool.Mutating:
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			return nil, fmt.Errorf("toolset: the schema of %s: %w", tool.Name, err)
		}
		toolset.tools = append(toolset.tools, tool)
		toolset.members[tool.Name] = map[string]bool{}
		for member := range schema.Properties {
			toolset.members[tool.Name][member] = true
		}
	}
	return toolset, nil
}

var agentName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validAgent(name string) bool { return agentName.MatchString(name) }

// Tools returns the selected tools in the catalogue's order, as copies.
func (t *Toolset) Tools() []generate.Tool {
	tools := make([]generate.Tool, 0, len(t.tools))
	for _, tool := range t.tools {
		copied, _ := t.options.Catalogue.Lookup(tool.Name)
		tools = append(tools, copied)
	}
	return tools
}

// CallOptions are what a transport knows about one call besides its arguments.
// IdempotencyKey is the call's Idempotency-Key (D16), a UUID; "" uses a new UUID v7, so
// every call is its own write and a caller that retries one call passes the same key.
// RequestID is what the audit and the history record as the call's request id (D31), so an
// agent's writes join the request or run that caused them; "" lets the engine make one.
type CallOptions struct {
	IdempotencyKey string
	RequestID      string
}

// Call runs the tool of the name with the model's arguments (a JSON object; empty or null is
// {}) for the identity in ctx, as the agent on its behalf (D86), and answers the REST body: a
// read's payload, a mutation's or upload's response (JSON null for none). A write runs on a
// context the caller's cancellation does not reach once it started, as the REST adapter's do
// (D60), so a started write commits and is audited as what it did. Every failure is an
// *Error; a panic of a check, the confirmation hook or anything else the call runs is
// answered as CodeInternal without its text and reported to the registry's Observer, since a
// transport like an MCP server runs the call where no recovery of the application reaches.
func (t *Toolset) Call(ctx context.Context, name string, arguments json.RawMessage, options CallOptions) (answer json.RawMessage, err error) {
	members, ok := t.members[name]
	if !ok {
		return nil, &Error{Code: livewire.CodeNotFound, Message: "There is no tool of this name."}
	}
	tool, _ := t.options.Catalogue.Lookup(name)
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			answer, err = nil, t.panicked(ctx, tool, started, fmt.Errorf("toolset: %s panicked: %v\n%s", tool.Name, recovered, debug.Stack()))
		}
	}()
	parsed, err := parseArguments(arguments, members, tool.Kind)
	if err != nil {
		return nil, err
	}
	idempotencyKey := options.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = uuid.Must(uuid.NewV7()).String()
	} else if _, err := uuid.Parse(idempotencyKey); err != nil {
		return nil, &Error{Code: livewire.CodeInvalidRequest, Message: "The idempotency key is not a UUID.", Fields: map[string]string{"idempotencyKey": "not a UUID"}}
	}
	if tool.Mutating && t.options.Confirm != nil {
		if err := t.options.Confirm(ctx, &Call{Tool: tool, Arguments: bytes.Clone(arguments)}); err != nil {
			return nil, &Error{Code: CodeDeclined, Message: "The call was declined; nothing ran."}
		}
	}
	if err := ctx.Err(); err != nil { // a caller that gave up starts nothing
		return nil, errorOf(err)
	}
	ctx = onBehalf(ctx, t.options.Agent)
	var payload json.RawMessage
	switch tool.Kind {
	case generate.ToolRead:
		payload, err = t.options.Engine.Read(ctx, livewire.ReadRequest{Target: tool.Target, Kind: tool.Read, Source: parsed.source})
	case generate.ToolMutation:
		_, payload, err = t.options.Engine.Call(context.WithoutCancel(ctx), livewire.CallRequest{
			Target: tool.Target, Source: parsed.source, Body: parsed.body, IdempotencyKey: idempotencyKey, RequestID: options.RequestID,
		})
	case generate.ToolUpload:
		var file *livewire.File
		if file, err = parsed.file(); err == nil {
			_, payload, err = t.options.Engine.Upload(context.WithoutCancel(ctx), livewire.UploadRequest{
				Target: tool.Target, Source: parsed.source, File: file, IdempotencyKey: idempotencyKey, RequestID: options.RequestID,
			})
		}
	}
	if err != nil {
		return nil, errorOf(err)
	}
	return t.bounded(tool, payload)
}

// panicked reports a recovered panic to the registry's Observer, as the engine reports the
// panics it recovers (a read under the engine's target, "<declaration>.<kind>" for every read
// but the main one, D46), and answers it as CodeInternal.
func (t *Toolset) panicked(ctx context.Context, tool generate.Tool, started time.Time, err error) *Error {
	kind, target := livewire.OutcomeRequest, tool.Target
	if tool.Mutating {
		kind = livewire.OutcomeCall
	} else if tool.Read != livewire.ReadMain {
		target += "." + string(tool.Read)
	}
	func() {
		defer func() { _ = recover() }() // the panic of an Observer has nobody left to report to
		t.options.Engine.Registry().Observe(ctx, livewire.Outcome{
			Kind: kind, Target: target, Identity: auth.IdentityFrom(ctx), Tenant: livewire.TenantFrom(ctx),
			Status: http.StatusInternalServerError, Err: err, Duration: time.Since(started),
		})
	}()
	return errorOf(err)
}

// bounded answers the payload within the cap.
func (t *Toolset) bounded(tool generate.Tool, payload json.RawMessage) (json.RawMessage, error) {
	if payload == nil {
		payload = json.RawMessage("null")
	}
	if t.options.MaximumResultBytes < 0 || len(payload) <= t.options.MaximumResultBytes {
		return payload, nil
	}
	if !tool.Mutating {
		return nil, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("The answer has %d bytes, more than the %d this agent reads; ask for less: a smaller limit, a narrower filter.", len(payload), t.options.MaximumResultBytes)}
	}
	// The write committed: say so instead of refusing, so the agent does not run it again.
	notice, err := json.Marshal(committedNotice{
		Committed: true, AnswerBytes: len(payload), Notice: "The call succeeded; its answer is larger than this agent reads and was left out.",
	})
	if err != nil {
		return nil, errorOf(err)
	}
	return notice, nil
}

// committedNotice stands in for the answer of a write past the cap.
type committedNotice struct {
	Committed   bool   `json:"committed"`
	AnswerBytes int    `json:"answerBytes"`
	Notice      string `json:"notice"`
}

// onBehalf marks the identity in ctx as acting through the agent (D86): the identity
// stays the effective one, so every check, scope and column check is its own, and the agent
// becomes its Impersonator, the real actor the audit, the history and the Observer record. An
// identity that is already impersonated keeps its impersonator in that real actor, whose
// subject is then "<agent>@<the impersonator's owner key>" (an agent name has no "@"), and the
// agent is active exactly when the impersonator is, so the audit names the human behind the
// call and a deactivated impersonator stays refused.
func onBehalf(ctx context.Context, agent string) context.Context {
	identity := auth.IdentityFrom(ctx)
	if identity == nil {
		return ctx
	}
	acting := identity.Clone()
	subject := agent
	if acting.Impersonator != nil {
		subject = agent + "@" + livewire.OwnerKey(acting.Impersonator)
	}
	acting.Impersonator = &auth.Identity{
		Provider: AgentProvider, Subject: subject, Username: agent, Name: agent, Active: identity.Real().Active, Impersonator: acting.Impersonator,
	}
	return auth.WithIdentity(ctx, acting)
}

// Error is a refusal of a tool call: a code of livewire's closed set or CodeDeclined or
// CodeTooLarge, a message safe to show (fixed per code, or the Toolset's own), and the
// offending parameters or body fields. Match it with errors.As.
type Error struct {
	Code    livewire.Code     `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Error is the code, the message and the fields in name order, as a model reads it.
func (e *Error) Error() string {
	var text strings.Builder
	text.WriteString(string(e.Code) + ": " + e.Message)
	for _, name := range slices.Sorted(maps.Keys(e.Fields)) {
		text.WriteString(" " + name + ": " + e.Fields[name] + ";")
	}
	return strings.TrimSuffix(text.String(), ";")
}

// errorOf maps an engine error to its refusal: the closed code and fixed message of
// livewire.ResponseFor, a 500 without its text.
func errorOf(err error) *Error {
	if refused, ok := errors.AsType[*Error](err); ok {
		return refused
	}
	_, body := livewire.ResponseFor(err)
	return &Error{Code: body.Code, Message: body.Message, Fields: body.Fields}
}

// arguments are a call's arguments sorted for the engine.
type arguments struct {
	source   source
	body     []byte
	content  *string
	encoding string
	typ      string
}

// file is an upload's content as the engine reads it.
func (a *arguments) file() (*livewire.File, error) {
	if a.content == nil {
		return nil, invalidArgument(generate.ArgumentContent, "required")
	}
	content := []byte(*a.content)
	// base64 text stored as an image's bytes is a silent corruption; a type the engine cannot
	// parse (none included) is the engine's to refuse, under its own member
	if mediaType, _, err := mime.ParseMediaType(a.typ); err == nil && a.encoding == "" && !strings.HasPrefix(mediaType, "text/") {
		return nil, invalidArgument(generate.ArgumentContentEncoding, "required for a content type that is not text/*")
	}
	switch a.encoding {
	case "", generate.ContentText:
	case generate.ContentBase64:
		decoded, err := base64.StdEncoding.DecodeString(*a.content)
		if err != nil {
			return nil, invalidArgument(generate.ArgumentContent, "not base64")
		}
		content = decoded
	default:
		return nil, invalidArgument(generate.ArgumentContentEncoding, "text or base64")
	}
	return &livewire.File{ContentType: a.typ, Size: int64(len(content)), Body: bytes.NewReader(content)}, nil
}

// parseArguments reads the model's arguments: a JSON object whose members are the tool's
// (an unknown one is refused, as an unknown body field is, D40). A mutation's body and an
// upload's content members stay apart; every other member, every member of a read included,
// is a parameter: a string as it is, a number or a boolean as its JSON text, an object (the
// filter) as its JSON, an array as one value per element, null as absent.
func parseArguments(raw json.RawMessage, members map[string]bool, kind generate.ToolKind) (*arguments, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || string(trimmed) == "null" {
		raw = json.RawMessage("{}") // no arguments, as some clients send them
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, &Error{Code: livewire.CodeInvalidRequest, Message: "The arguments must be one JSON object."}
	}
	parsed := &arguments{source: source{}}
	for name, value := range fields {
		if !members[name] {
			return nil, invalidArgument(name, "unknown argument")
		}
		var err error
		switch {
		case kind == generate.ToolMutation && name == generate.ArgumentBody:
			parsed.body = value
		case kind == generate.ToolUpload && name == generate.ArgumentContent:
			parsed.content, err = textArgument(value)
		case kind == generate.ToolUpload && name == generate.ArgumentContentEncoding:
			parsed.encoding, err = optionalText(value)
		case kind == generate.ToolUpload && name == generate.ArgumentContentType:
			parsed.typ, err = optionalText(value)
		default:
			err = parsed.source.add(name, value)
		}
		if err != nil {
			return nil, invalidArgument(name, err.Error())
		}
	}
	return parsed, nil
}

func textArgument(value json.RawMessage) (*string, error) {
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return nil, errors.New("must be a string")
	}
	return &text, nil
}

func optionalText(value json.RawMessage) (string, error) {
	text, err := textArgument(value)
	if err != nil {
		return "", err
	}
	return *text, nil
}

func invalidArgument(name, reason string) *Error {
	return &Error{Code: livewire.CodeInvalidRequest, Message: "The arguments are invalid.", Fields: map[string]string{name: reason}}
}

// source is the parameters of a call as the engine decodes them: path and query values by
// name, a list parameter with one value per element.
type source map[string][]string

func (s source) Path(name string) (string, bool) {
	values, ok := s[name]
	if !ok || len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func (s source) Query(name string) ([]string, bool) {
	values, ok := s[name]
	return values, ok
}

func (s source) add(name string, value json.RawMessage) error {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	if value[0] != '[' {
		text, err := parameterText(value)
		s[name] = []string{text}
		return err
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(value, &elements); err != nil {
		return errors.New("not a JSON array")
	}
	values := make([]string, 0, len(elements))
	for _, element := range elements {
		if element = bytes.TrimSpace(element); len(element) > 0 && (element[0] == '[' || element[0] == '{') {
			return errors.New("a list holds scalars")
		}
		text, err := parameterText(element)
		if err != nil {
			return err
		}
		// The engine splits a list's values on commas (REST's form), so one element would become two.
		if strings.Contains(text, ",") {
			return errors.New("a list value holds no comma")
		}
		values = append(values, text)
	}
	s[name] = values
	return nil
}

// parameterText is one value as a query string carries it: a string unquoted, anything else
// (a number, a boolean, a filter document) as its JSON.
func parameterText(value json.RawMessage) (string, error) {
	if value[0] != '"' {
		return string(value), nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", errors.New("not a JSON string")
	}
	return text, nil
}
