package livewire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"

	"github.com/google/uuid"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire/query"
)

// NoBody is the request type of a mutation without a body.
type NoBody struct{}

// NoContent is the response type of a mutation that answers without a body (204 unless
// Status says otherwise).
type NoContent struct{}

// ClientIDVersion says which client-generated `id` a create accepts (D18).
type ClientIDVersion int

// The client id versions. Without one (the zero value) no `id` is accepted in the body.
const (
	ClientIDNone ClientIDVersion = iota
	UUIDv7
	UUIDv4
)

var uuidVersions = map[ClientIDVersion]uuid.Version{UUIDv7: 7, UUIDv4: 4}

// EffectKind is what an optimistic effect does to a live list (D36).
type EffectKind string

// The effect kinds.
const (
	EffectUpserts EffectKind = "upserts" // the response is upserted into the list
	EffectRemoves EffectKind = "removes" // the item with the body's or path's id is removed
)

// Effect is the optimistic effect of a mutation on the live lists of Target (D36); the zero
// value is no effect.
type Effect struct {
	Kind   EffectKind
	Target string
}

// Upserts is the effect that upserts the response into the live lists of target.
func Upserts(target string) Effect { return Effect{Kind: EffectUpserts, Target: target} }

// Removes is the effect that removes the mutated id from the live lists of target.
func Removes(target string) Effect { return Effect{Kind: EffectRemoves, Target: target} }

// MutationOptions declares a write. Check and Handle are required. Status is the success
// status (default 200, or 204 for a NoContent response). ClientID accepts a client-generated
// `id` in the body (D18). Resource names a registered resource whose field Write checks
// apply to the body by JSON name (D39); FieldChecks adds a check per JSON field of Request.
// Effect is the optimistic effect on live lists (D36).
type MutationOptions[Params, Request, Response any] struct {
	Check       authorization.Check
	Status      int
	ClientID    ClientIDVersion
	Resource    string
	FieldChecks map[string]authorization.Check
	Effect      Effect
	Handle      func(ctx context.Context, parameters *Params, body *Request) (*Response, error)
}

// Mutation is a write: a JSON body decoded with unknown fields refused (D40), checked field
// by field (D39), handled in the engine's transaction (D31).
type Mutation struct {
	Signature
	parametersType reflect.Type
	requestType    reflect.Type
	responseType   reflect.Type
	status         int
	clientID       ClientIDVersion
	resource       string
	fieldChecks    map[string]authorization.Check
	writeChecks    map[string]authorization.Check
	effect         Effect
	handle         func(ctx context.Context, parameters, body any) (any, error)
}

// NewMutation declares a write with method (POST, PUT, PATCH or DELETE) at path, whose
// `{name}` placeholders are the `path:"name"` fields of Params. Request is the body type
// (a struct, or NoBody), Response the response type (NoContent for none). A mistake is
// reported by Registry.Add.
func NewMutation[Params, Request, Response any](name, method, path string, options *MutationOptions[Params, Request, Response]) *Mutation {
	if options == nil {
		options = &MutationOptions[Params, Request, Response]{}
	}
	mutation := &Mutation{
		Signature:      newSignature(name, method, path, nil, options.Check),
		parametersType: reflect.TypeFor[Params](),
		requestType:    reflect.TypeFor[Request](),
		responseType:   reflect.TypeFor[Response](),
		status:         options.Status,
		clientID:       options.ClientID,
		resource:       options.Resource,
		fieldChecks:    cloneChecks(options.FieldChecks),
		writeChecks:    cloneChecks(options.FieldChecks),
		effect:         options.Effect,
	}
	if options.Handle != nil {
		mutation.handle = func(ctx context.Context, parameters, body any) (any, error) {
			return options.Handle(ctx, parameters.(*Params), body.(*Request))
		}
	}
	mutation.compileParameters(mutation.parametersType)
	mutation.validate(options.Handle == nil)
	return mutation
}

func (m *Mutation) validate(handleMissing bool) {
	switch {
	case handleMissing:
		m.fail("Handle is required")
	case !slices.Contains(writeMethods, m.method):
		m.fail("the method %q is not a write method (POST, PUT, PATCH, DELETE)", m.method)
	case m.status != 0 && (m.status < http.StatusOK || m.status > 299):
		m.fail("Status %d is not a success status", m.status)
	case m.HasBody() && m.requestType.Kind() != reflect.Struct:
		m.fail("the request type %s is not a struct", m.requestType)
	case m.clientID < ClientIDNone || m.clientID > UUIDv4:
		m.fail("ClientID %d is not a ClientIDVersion", m.clientID)
	case m.clientID != ClientIDNone && !m.HasBody():
		m.fail("ClientID needs a body")
	case m.effect.Kind != "" && m.effect.Kind != EffectUpserts && m.effect.Kind != EffectRemoves:
		m.fail("Effect kind %q is unknown", m.effect.Kind)
	case (m.effect.Kind == "") != (m.effect.Target == ""):
		m.fail("Effect needs Kind and Target")
	}
	if m.err != nil {
		return
	}
	if m.status == 0 {
		m.status = http.StatusOK
		if !m.HasContent() {
			m.status = http.StatusNoContent
		}
	}
	if m.clientID != ClientIDNone {
		m.failWith(requireJSONField(m.requestType, keyFieldJSON, "ClientID accepts it"))
	}
	for _, name := range sortedKeys(m.fieldChecks) {
		if m.fieldChecks[name] == nil {
			m.fail("FieldChecks[%q] is nil", name)
			return
		}
		m.failWith(requireJSONField(m.requestType, name, "FieldChecks names it"))
	}
}

// bindResource builds the body write checks from the resource's fields: Write, or Read when
// Write is nil, plus FieldChecks; a field with both runs both.
func (m *Mutation) bindResource(fields query.Fields) {
	m.writeChecks = cloneChecks(m.fieldChecks)
	for _, field := range fields {
		check := field.Write
		if check == nil {
			check = field.Read
		}
		if check == nil {
			continue
		}
		if existing, ok := m.writeChecks[field.Name]; ok {
			m.writeChecks[field.Name] = authorization.All(authorization.Check(check), existing)
			continue
		}
		m.writeChecks[field.Name] = authorization.Check(check)
	}
}

// Kind is KindMutation.
func (m *Mutation) Kind() Kind { return KindMutation }

// ParametersType is the Go type of the parameters struct.
func (m *Mutation) ParametersType() reflect.Type { return m.parametersType }

// RequestType is the Go type of the body; NoBody for none.
func (m *Mutation) RequestType() reflect.Type { return m.requestType }

// ResponseType is the Go type of the response; NoContent for none.
func (m *Mutation) ResponseType() reflect.Type { return m.responseType }

// HasBody reports whether the mutation reads a body.
func (m *Mutation) HasBody() bool { return m.requestType != reflect.TypeFor[NoBody]() }

// HasContent reports whether the mutation answers with a body.
func (m *Mutation) HasContent() bool { return m.responseType != reflect.TypeFor[NoContent]() }

// Status is the success status.
func (m *Mutation) Status() int { return m.status }

// ClientID is the accepted client id version (D18).
func (m *Mutation) ClientID() ClientIDVersion { return m.clientID }

// Resource names the resource whose field write checks apply to the body, or "".
func (m *Mutation) Resource() string { return m.resource }

// Effect is the optimistic effect (D36); the zero value is none.
func (m *Mutation) Effect() Effect { return m.effect }

func (m *Mutation) routes() []route { return []route{{m.method, m.path}} }

// DecodeBody decodes raw into a new body value (a *Request) under the identity in ctx. A
// body that is not valid JSON of the request type, or that carries an unknown field, is 400
// invalid_request naming the field (D40); a client id the mutation does not accept, or of
// the wrong version, is 400 naming "id" (D18); a field whose write check refuses is 403
// forbidden_field (D39). A mutation without a body ignores raw.
func (m *Mutation) DecodeBody(ctx context.Context, raw []byte) (any, error) {
	body := reflect.New(m.requestType).Interface()
	if !m.HasBody() {
		return body, nil
	}
	present, err := m.presentFields(raw)
	if err != nil {
		return nil, err
	}
	if err := m.checkClientID(present); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		return nil, bodyError(err)
	}
	if err := m.checkWrites(ctx, present); err != nil {
		return nil, err
	}
	return body, nil
}

// presentFields returns the body's top-level fields; an unknown one is refused by name,
// which the decoder cannot do (its message is text only).
func (m *Mutation) presentFields(raw []byte) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalidParameter("body", "required")
	}
	var present map[string]json.RawMessage
	var typeError *json.UnmarshalTypeError
	err := json.Unmarshal(raw, &present)
	switch {
	case errors.As(err, &typeError), err == nil && present == nil:
		return nil, invalidParameter("body", "must be an object")
	case err != nil:
		return nil, bodyError(err)
	}
	known := jsonFieldsOf(m.requestType)
	for _, name := range sortedKeys(present) {
		if _, ok := known[name]; !ok {
			return nil, invalidParameter(name, "unknown field")
		}
	}
	return present, nil
}

func (m *Mutation) checkClientID(present map[string]json.RawMessage) error {
	rawID, ok := present[keyFieldJSON]
	if !ok {
		return nil
	}
	if m.clientID == ClientIDNone {
		return invalidParameter(keyFieldJSON, "a client id is not accepted")
	}
	var text string
	if err := json.Unmarshal(rawID, &text); err != nil {
		return invalidParameter(keyFieldJSON, "must be a uuid string")
	}
	id, err := uuid.Parse(text)
	if err != nil || id.Version() != uuidVersions[m.clientID] {
		return invalidParameter(keyFieldJSON, fmt.Sprintf("must be a UUID version %d", uuidVersions[m.clientID]))
	}
	return nil
}

func (m *Mutation) checkWrites(ctx context.Context, present map[string]json.RawMessage) error {
	for _, name := range sortedKeys(present) {
		check, ok := m.writeChecks[name]
		if !ok {
			continue
		}
		if err := check(ctx); err != nil {
			return ForbiddenField(name)
		}
	}
	return nil
}

// Handle runs the handler with the decoded parameters (a *Params) and body (a *Request);
// the response is a *Response.
func (m *Mutation) Handle(ctx context.Context, parameters, body any) (any, error) {
	return m.handle(ctx, parameters, body)
}

func bodyError(err error) *Error {
	var typeError *json.UnmarshalTypeError
	switch {
	case errors.Is(err, io.EOF):
		return invalidParameter("body", "required")
	case errors.As(err, &typeError) && typeError.Field != "":
		return invalidParameter(typeError.Field, "must be a "+typeError.Type.String())
	default:
		return invalidParameter("body", "must be valid JSON of the request type: "+err.Error())
	}
}

func cloneChecks(checks map[string]authorization.Check) map[string]authorization.Check {
	cloned := make(map[string]authorization.Check, len(checks))
	maps.Copy(cloned, checks)
	return cloned
}

func sortedKeys[Value any](values map[string]Value) []string {
	return slices.Sorted(maps.Keys(values))
}
