package livewire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/govengers/auth/authorization"

	"github.com/govengers/livewire/query"
)

// NoBody is the request type of a mutation without a body; a request that sends one is
// refused.
type NoBody struct{}

// NoContent is the response type of a mutation that answers without a body (204 unless
// Status says otherwise).
type NoContent struct{}

// ClientIDVersion says which client-generated `id` a create accepts (D18).
type ClientIDVersion int

// The client id versions. Without one (the zero value) a create accepts no `id` in the body.
const (
	ClientIDNone ClientIDVersion = iota
	UUIDv7
	UUIDv4
)

// EffectKind is what an optimistic effect does to a live list (D36).
type EffectKind string

// The effect kinds.
const (
	EffectUpserts EffectKind = "upserts" // the response is upserted into the list
	EffectRemoves EffectKind = "removes" // the item whose id is the request's `id` path parameter is removed
)

// Effect is the optimistic effect of a mutation on the live lists of Target (D36); the zero
// value is no effect.
type Effect struct {
	Kind   EffectKind
	Target string
}

// Upserts is the effect that upserts the response into the live lists of target; the
// response type must carry a JSON field "id".
func Upserts(target string) Effect { return Effect{Kind: EffectUpserts, Target: target} }

// Removes is the effect that removes the item named by the `id` path parameter from the live
// lists of target; the mutation's parameters must carry `path:"id"`.
func Removes(target string) Effect { return Effect{Kind: EffectRemoves, Target: target} }

// MutationOptions declares a write. Check and Handle are required. Status is the success
// status (default 200; a NoContent response defaults to 204 and may set another 2xx, 204 with
// a response body is refused). ClientID accepts a client-generated `id` in the body of a
// create (POST, D18); the body of any other method may carry an id and the handler judges
// it. Resource names a registered resource whose field Write checks apply to the body by
// JSON name (D39); FieldChecks adds a check per JSON field of Request. Effect is the
// optimistic effect on live lists (D36). The body is validated with `validate` tags
// (go-playground/validator, D44) after the structural checks; a tag the validator cannot
// parse, on any struct reachable from Request, is a declaration error.
type MutationOptions[Parameters, Request, Response any] struct {
	Check       authorization.Check
	Status      int
	ClientID    ClientIDVersion
	Resource    string
	FieldChecks map[string]authorization.Check
	Effect      Effect
	Handle      func(ctx context.Context, parameters *Parameters, body *Request) (*Response, error)
}

// Mutation is a write: a JSON body decoded with unknown fields refused (D40), checked field
// by field (D39), validated (D44), handled in the engine's transaction (D31).
type Mutation struct {
	signature
	parametersType reflect.Type
	requestType    reflect.Type
	responseType   reflect.Type
	status         int
	clientID       ClientIDVersion
	resource       string
	fieldChecks    map[string]authorization.Check
	writeChecks    map[string]authorization.Check
	effect         Effect
	validate       *validator.Validate
	fields         fieldIndex
	handle         func(ctx context.Context, parameters, body any) (any, error)
}

// NewMutation declares a write with method (POST, PUT, PATCH or DELETE) at path, whose
// `{name}` placeholders are the `path:"name"` fields of Parameters. Request is the body type
// (a struct, or NoBody), Response the response type (NoContent for none). The options are
// copied. A mistake is reported by Registry.Add (D43).
func NewMutation[Parameters, Request, Response any](name, method, path string, options *MutationOptions[Parameters, Request, Response]) *Mutation {
	if options == nil {
		options = &MutationOptions[Parameters, Request, Response]{}
	}
	mutation := &Mutation{
		signature:      newSignature(name, method, path, nil, options.Check),
		parametersType: reflect.TypeFor[Parameters](),
		requestType:    reflect.TypeFor[Request](),
		responseType:   reflect.TypeFor[Response](),
		clientID:       options.ClientID,
		resource:       options.Resource,
		fieldChecks:    maps.Clone(options.FieldChecks),
		writeChecks:    maps.Clone(options.FieldChecks),
		effect:         options.Effect,
	}
	fallback := http.StatusOK
	if !mutation.HasContent() {
		fallback = http.StatusNoContent
	}
	mutation.status = intOrDefault(options.Status, fallback)
	if handle := options.Handle; handle != nil {
		mutation.handle = func(ctx context.Context, parameters, body any) (any, error) {
			return handle(ctx, parameters.(*Parameters), body.(*Request))
		}
	}
	if mutation.HasBody() {
		mutation.validate = validator.New(validator.WithRequiredStructEnabled())
		mutation.fields = indexFields(mutation.requestType)
	}
	mutation.compileParameters(mutation.parametersType)
	mutation.failWith(mutation.validateDeclaration(options.Status))
	mutation.failWith(refusePointerLoops(mutation.requestType))
	if mutation.err == nil {
		mutation.failWith(mutation.preflightValidateTags())
	}
	return mutation
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

// DecodeBody decodes raw into a new body value (a *Request) under the identity in ctx. The
// body is read once as a token stream alongside the request type: a body that is not a JSON
// object, an unknown member at any depth (struct fields, array elements and map values
// included) and a member that repeats within one object are 400 invalid_request naming the
// member by its path (D40); a client id a create does not accept, or of the wrong form or
// version, is 400 naming "id" (D18); a field whose write check refuses is 403
// forbidden_field (D39); a body that fails its `validate` tags is 400 naming each field with
// the failed tag (D44); a value of the wrong type is 400 naming it by the same path. Every
// text in the response is fixed; the cause of a decoding failure stays on the server. A
// validator fault the declaration's preflight cannot reach is answered as an internal
// error, never as a panic. A mutation without a body refuses a non-empty raw.
func (m *Mutation) DecodeBody(ctx context.Context, raw []byte) (any, error) {
	body := reflect.New(m.requestType).Interface()
	if !m.HasBody() {
		if len(bytes.TrimSpace(raw)) > 0 {
			return nil, invalidParameter("body", "not accepted")
		}
		return body, nil
	}
	present, err := inspectBody(raw, m.requestType, m.fields)
	if err != nil {
		return nil, err
	}
	if err := m.checkClientID(present); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		return nil, m.bodyError(raw, err)
	}
	if err := m.checkWrites(ctx, present); err != nil {
		return nil, err
	}
	if err := m.validateBody(ctx, body); err != nil {
		return nil, err
	}
	return body, nil
}

// Handle runs the handler with the decoded parameters (a *Parameters) and body (a *Request);
// the response is a *Response.
func (m *Mutation) Handle(ctx context.Context, parameters, body any) (any, error) {
	return m.handle(ctx, parameters, body)
}

func (m *Mutation) declaration() Declaration { return m }

func (m *Mutation) routes() []route { return []route{{m.method, m.path}} }

func (m *Mutation) validateDeclaration(status int) error {
	bodylessClientID := m.clientID != ClientIDNone && (!m.HasBody() || m.method != http.MethodPost)
	effectHalfSet := (m.effect.Kind == "") != (m.effect.Target == "")
	unknownEffect := m.effect.Kind != "" && m.effect.Kind != EffectUpserts && m.effect.Kind != EffectRemoves
	switch {
	case m.handle == nil:
		return errors.New("Handle is required")
	case !slices.Contains(writeMethods, m.method):
		return fmt.Errorf("the method %q is not a write method (POST, PUT, PATCH, DELETE)", m.method)
	case status != 0 && !isSuccessStatus(status):
		return fmt.Errorf("Status %d is not a success status", status)
	case status == http.StatusNoContent && m.HasContent():
		return errors.New("Status 204 cannot carry the response body")
	case m.HasBody() && m.requestType.Kind() != reflect.Struct:
		return fmt.Errorf("the request type %s is not a struct", m.requestType)
	case m.clientID < ClientIDNone || m.clientID > UUIDv4:
		return fmt.Errorf("ClientID %d is not a ClientIDVersion", m.clientID)
	case bodylessClientID:
		return errors.New("ClientID needs a POST with a body: it is a create's option (D18)")
	case unknownEffect:
		return fmt.Errorf("Effect kind %q is unknown", m.effect.Kind)
	case effectHalfSet:
		return errors.New("Effect needs Kind and Target")
	}
	if m.clientID != ClientIDNone {
		if err := requireJSONField(m.requestType, keyFieldJSON, "ClientID accepts it"); err != nil {
			return err
		}
	}
	return m.validateFieldChecks()
}

// preflightValidateTags makes a validate tag the validator cannot use, on any struct type
// reachable from the request type (through pointers, slices, arrays and maps), an Add error
// (D44), never a request-time panic. The validator parses a tag when it first meets the
// struct and a rule's arguments only when the rule runs, so two passes run the tags: over a
// zero value of every struct (the tags as written), then every rule on its own
// (preflightRules), so a rule behind a short-circuit (`required,min=abc` or
// `omitempty,min=abc` on a zero value) runs too. The samples' own validation failures are
// not the question here.
func (m *Mutation) preflightValidateTags() error {
	if m.validate == nil || m.requestType.Kind() != reflect.Struct {
		return nil
	}
	structs := reachableStructs(m.requestType)
	for _, structType := range structs {
		if err := m.preflight(reflect.New(structType), structType.String()); err != nil {
			return err
		}
	}
	for _, structType := range structs {
		for position := range structType.NumField() {
			field := structType.Field(position)
			tag := field.Tag.Get("validate")
			validated := tag != "" && tag != "-" && field.IsExported()
			if !validated {
				continue
			}
			if err := m.preflightRules(structType, field, field.Type, strings.Split(tag, ",")); err != nil {
				return err
			}
		}
	}
	return nil
}

// preflightRules runs each rule of a field's tag, and each alternative of a rule, on its own
// on a value of the type the rule applies to: the field's, after `dive` the element's, between
// `keys` and `endkeys` the map key's.
func (m *Mutation) preflightRules(owner reflect.Type, field reflect.StructField, target reflect.Type, rules []string) error {
	for position, rule := range rules {
		if rule == "dive" {
			return m.preflightDive(owner, field, target, rules[position+1:])
		}
		for _, alternative := range strings.Split(rule, "|") {
			if err := m.preflightRule(owner, field, target, alternative); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Mutation) preflightDive(owner reflect.Type, field reflect.StructField, target reflect.Type, rules []string) error {
	container := unwrapPointer(target)
	if !hasElements(container.Kind()) {
		return fmt.Errorf("the validate tags of %s.%s cannot be used: dive on %s, which has no elements", owner, field.Name, target)
	}
	divesIntoKeys := container.Kind() == reflect.Map && len(rules) > 0 && rules[0] == "keys"
	if divesIntoKeys {
		keyRules := rules[1:]
		rules = nil
		if end := slices.Index(keyRules, "endkeys"); end >= 0 {
			keyRules, rules = keyRules[:end], keyRules[end+1:]
		}
		if err := m.preflightRules(owner, field, container.Key(), keyRules); err != nil {
			return err
		}
	}
	return m.preflightRules(owner, field, container.Elem(), rules)
}

// preflightRule runs one rule on a holder struct whose field of the field's name and the
// rule's type carries the rule alone, beside the owner's other exported fields, which the
// rule's cross-field arguments name. Pointers are allocated, so the rule runs instead of
// stopping at nil.
func (m *Mutation) preflightRule(owner reflect.Type, field reflect.StructField, target reflect.Type, rule string) error {
	holderFields := []reflect.StructField{{Name: field.Name, Type: target, Tag: reflect.StructTag("validate:" + strconv.Quote(rule))}}
	for position := range owner.NumField() {
		sibling := owner.Field(position)
		if sibling.IsExported() && sibling.Name != field.Name {
			holderFields = append(holderFields, reflect.StructField{Name: sibling.Name, Type: sibling.Type})
		}
	}
	holder := reflect.New(reflect.StructOf(holderFields))
	for value := holder.Elem().Field(0); value.Kind() == reflect.Pointer; value = value.Elem() {
		value.Set(reflect.New(value.Type().Elem()))
	}
	return m.preflight(holder, owner.String()+"."+field.Name)
}

// preflight validates value (a pointer to a struct) and turns a panic of the validator, or an
// error that is no validation failure, into the declaration error of what.
func (m *Mutation) preflight(value reflect.Value, what string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("the validate tags of %s cannot be parsed: %v", what, recovered)
		}
	}()
	if err := m.validate.Struct(value.Interface()); isUnexpectedValidationError(err) {
		return fmt.Errorf("the validate tags of %s cannot be used: %w", what, err)
	}
	return nil
}

// isUnexpectedValidationError reports an error of the validator that is neither a validation
// failure nor a value it does not validate (time.Time and the like: nothing to parse).
func isUnexpectedValidationError(err error) bool {
	var failures validator.ValidationErrors
	var notValidatable *validator.InvalidValidationError
	return err != nil && !errors.As(err, &failures) && !errors.As(err, &notValidatable)
}

// validateBody runs the validate tags; a fault the preflight cannot reach (a rule on an
// interface value, whose type shows only at run time) is an internal error, not a panic.
func (m *Mutation) validateBody(ctx context.Context, body any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("livewire: the validate tags of %s failed at run time: %v", m.requestType, recovered)
		}
	}()
	if err := m.validate.StructCtx(ctx, body); err != nil {
		return m.validationError(err)
	}
	return nil
}

func (m *Mutation) validateFieldChecks() error {
	for _, name := range sortedKeys(m.fieldChecks) {
		if m.fieldChecks[name] == nil {
			return fmt.Errorf("FieldChecks[%q] is nil", name)
		}
		if err := requireJSONField(m.requestType, name, "FieldChecks names it"); err != nil {
			return err
		}
	}
	return nil
}

// writeChecksFor builds the body write checks from the resource's fields: Write, or Read
// when Write is nil, plus FieldChecks; a field with both runs both. The registry applies the
// result once every declaration of its Add passed.
func (m *Mutation) writeChecksFor(fields query.Fields) map[string]authorization.Check {
	checks := make(map[string]authorization.Check, len(fields)+len(m.fieldChecks))
	maps.Copy(checks, m.fieldChecks)
	for _, field := range fields {
		check := field.Write
		if check == nil {
			check = field.Read
		}
		if check == nil {
			continue
		}
		if existing, ok := checks[field.Name]; ok {
			checks[field.Name] = authorization.All(authorization.Check(check), existing)
			continue
		}
		checks[field.Name] = authorization.Check(check)
	}
	return checks
}

func (m *Mutation) checkClientID(present map[string]json.RawMessage) error {
	rawID, ok := present[keyFieldJSON]
	if !ok || m.method != http.MethodPost {
		return nil
	}
	if m.clientID == ClientIDNone {
		return invalidParameter(keyFieldJSON, "a client id is not accepted")
	}
	var text string
	if err := json.Unmarshal(rawID, &text); err != nil {
		return invalidParameter(keyFieldJSON, "must be a uuid string")
	}
	if !isClientID(text, uuidVersions[m.clientID]) {
		return invalidParameter(keyFieldJSON, fmt.Sprintf("must be a canonical UUID version %d", uuidVersions[m.clientID]))
	}
	return nil
}

var uuidVersions = map[ClientIDVersion]uuid.Version{UUIDv7: 7, UUIDv4: 4}

// isClientID accepts the 36-character canonical form of an RFC 4122 uuid of the version.
func isClientID(text string, version uuid.Version) bool {
	const canonicalLength = 36
	if len(text) != canonicalLength {
		return false
	}
	id, err := uuid.Parse(text)
	return err == nil && id.Variant() == uuid.RFC4122 && id.Version() == version
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

// bodyError maps a decoder failure to a fixed text; the decoder's own message, which may
// repeat a custom unmarshaler's text, never reaches the client. A value of the wrong type is
// named by the path the body walk writes (pathOfTypeError).
func (m *Mutation) bodyError(raw []byte, err error) *Error {
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		return invalidParameter(pathOfTypeError(raw, m.requestType, m.fields, typeError), "has the wrong type")
	}
	return invalidParameter("body", "could not be decoded as the request type")
}

// validationError maps the failed tags to the fields by JSON path.
func (m *Mutation) validationError(err error) error {
	var failures validator.ValidationErrors
	if !errors.As(err, &failures) {
		return fmt.Errorf("livewire: validate the body: %w", err)
	}
	fields := make(map[string]string, len(failures))
	for _, failure := range failures {
		fields[m.jsonPathOf(failure.StructNamespace())] = failure.Tag()
	}
	return Invalid(fields)
}

// jsonPathOf translates the validator's Go namespace ("createPart.Nested.Note",
// "audited.createPart.Secret", "order.Lines[1].SKU", "settings.Values[a.b]") to the body's
// JSON path with the field index: the root type is dropped, untagged embedded structs
// contribute no segment, every other field its JSON name, and every key or position in
// brackets consumes one level of a collection or map of the type, so a recursive type
// (`type Tree map[string]Tree`) is followed only as deep as the namespace goes. A namespace
// the type cannot explain (an unbalanced key, a segment that is no field, a key where the
// type has no elements) is reported as "body", never as it is.
func (m *Mutation) jsonPathOf(structNamespace string) string {
	rootType := unwrapPointer(m.requestType)
	remainder, found := strings.CutPrefix(structNamespace, rootType.Name()+".")
	if !found {
		return "body"
	}
	segments, parsed := parseNamespace(remainder)
	if !parsed {
		return "body"
	}
	path := make([]pathSegment, 0, len(segments))
	currentType := rootType
	for _, segment := range segments {
		currentType = unwrapPointer(currentType)
		if segment.index {
			if !hasElements(currentType.Kind()) {
				return "body"
			}
			path = append(path, segment)
			currentType = currentType.Elem()
			continue
		}
		if currentType.Kind() != reflect.Struct {
			return "body"
		}
		field, ok := currentType.FieldByName(segment.text)
		if !ok {
			return "body"
		}
		if name, named := m.fields.jsonNameOf(currentType, field); named {
			path = append(path, pathSegment{text: name})
		} else if !field.Anonymous {
			return "body"
		}
		currentType = field.Type
	}
	return writePath(path)
}

// parseNamespace splits a namespace below the root ("Lines[1].SKU", "Values[a.b]",
// "Grid[0][1]") into field names and the keys or positions in brackets. A key may hold
// balanced brackets ("Values[c[d]]"); a namespace that does not read back as names, each
// followed by bracketed keys and a dot or the end, is not parsed. An empty name is kept: it
// is no field of any type.
func parseNamespace(namespace string) ([]pathSegment, bool) {
	var segments []pathSegment
	for {
		nameEnd := strings.IndexAny(namespace, ".[")
		if nameEnd < 0 {
			nameEnd = len(namespace)
		}
		segments = append(segments, pathSegment{text: namespace[:nameEnd]})
		namespace = namespace[nameEnd:]
		for strings.HasPrefix(namespace, "[") {
			keyEnd, closed := closingBracket(namespace)
			if !closed {
				return nil, false
			}
			segments = append(segments, pathSegment{text: namespace[1:keyEnd], index: true})
			namespace = namespace[keyEnd+1:]
		}
		if namespace == "" {
			return segments, true
		}
		rest, dotted := strings.CutPrefix(namespace, ".")
		if !dotted {
			return nil, false
		}
		namespace = rest
	}
}

// closingBracket returns the position of the bracket that closes the one text opens with.
func closingBracket(text string) (int, bool) {
	depth := 0
	for position := range len(text) {
		switch text[position] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return position, true
			}
		}
	}
	return 0, false
}

func sortedKeys[Value any](values map[string]Value) []string {
	return slices.Sorted(maps.Keys(values))
}
