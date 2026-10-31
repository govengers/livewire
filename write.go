package livewire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/govengers/auth"
)

// CallRequest is one mutation call as a transport hands it to Engine.Call: the mutation's
// name, the request's parameters, the raw JSON body (empty for none), the Idempotency-Key
// (D16, empty for none) and the request id the transaction records (D31; a new one when
// empty, which is what the socket does: frame ids are the client's and repeat).
type CallRequest struct {
	Target         string
	Source         ParameterSource
	Body           []byte
	IdempotencyKey string
	RequestID      string
}

// UploadRequest is one upload as the REST adapter hands it to Engine.Upload: the upload's
// name, the parameters, the file it read (content type, size and body) and, as for a call,
// the Idempotency-Key and the request id.
type UploadRequest struct {
	Target         string
	Source         ParameterSource
	File           *File
	IdempotencyKey string
	RequestID      string
}

// BatchRequest is one batch (D33) as a transport hands it to Engine.Batch: the body, a JSON
// array of BatchCall, the batch's one Idempotency-Key and the request id.
type BatchRequest struct {
	Body           json.RawMessage
	IdempotencyKey string
	RequestID      string
}

// BatchCall is one call of a batch: a mutation's name, its parameters as a socket frame
// carries them, and its body.
type BatchCall struct {
	Target     string            `json:"target"`
	Parameters map[string]string `json:"params,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
}

// BatchResult is the answer of one call of a committed batch, in the order of the calls: the
// status the call answers on its own and its body (absent for NoContent).
type BatchResult struct {
	Status  int             `json:"status"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

const (
	// maximumBatchCalls bounds the calls of one batch.
	maximumBatchCalls = 100
	// batchMutation is the name a batch's Idempotency-Key is reserved under.
	batchMutation = "batch"
)

// Call runs one mutation for the identity in ctx (section 7), the pipeline the socket's call
// op runs too: the static Check (D8), the parameters, the tenant (D42), the body (unknown
// fields refused, D40; field write checks, D39; validation, D44), then the engine
// transaction (D31): the Idempotency-Key reserved through the IdempotencyStore (a finished
// key answers its recorded response, one in flight 409 in_progress, D16), the handler, the
// idempotency record, the commit. It answers the mutation's status and the marshaled
// response (nil for NoContent). An upload is REST only and refused here. Call reports an
// OutcomeCall and, after the commit, an OutcomeAfterCommit (D41); a replay is not committed
// again and reports none.
func (e *Engine) Call(ctx context.Context, request CallRequest) (status int, payload json.RawMessage, err error) {
	return e.call(ctx, "", request)
}

func (e *Engine) call(ctx context.Context, operation Operation, request CallRequest) (int, json.RawMessage, error) {
	unit := startUnit(OutcomeCall, operation, request.Target)
	prepared, err := e.prepareCall(ctx, request.Target, request.Source, request.Body)
	if err != nil {
		e.registry.Observe(ctx, unit.outcome(ctx, err))
		return 0, nil, err
	}
	ctx = WithTenant(ctx, prepared.tenant)
	plan := writePlan{mutation: request.Target, transactionMutation: request.Target, parameters: prepared.parameters, idempotencyKey: request.IdempotencyKey, requestID: request.RequestID}
	answer, err := e.write(ctx, plan, prepared.handle)
	e.observeWrite(ctx, unit, prepared.parameters, answer, err)
	if err != nil {
		return 0, nil, err
	}
	return answer.status, answer.payload, nil
}

// preparedCall is a call past its checks: the mutation with its decoded parameters, body
// and tenant.
type preparedCall struct {
	mutation   *Mutation
	parameters any
	body       any
	tenant     string
}

// prepareCall runs everything before the transaction, in the order of D8: the static check
// before the parameters and the tenant, both before the body.
func (e *Engine) prepareCall(ctx context.Context, target string, source ParameterSource, rawBody []byte) (*preparedCall, error) {
	declaration, ok := e.registry.Lookup(target)
	if !ok {
		return nil, NotFound()
	}
	mutation, ok := declaration.(*Mutation)
	switch {
	case declaration.Kind() == KindUpload:
		return nil, invalidParameter("target", "an upload is REST only")
	case !ok:
		return nil, NotFound()
	}
	if err := mutation.Check()(ctx); err != nil {
		return nil, err
	}
	parameters, err := mutation.DecodeParameters(sourceOrEmpty(source))
	if err != nil {
		return nil, err
	}
	tenant, err := e.registry.Tenant(ctx, parameters)
	if err != nil {
		return nil, err
	}
	body, err := mutation.DecodeBody(WithTenant(ctx, tenant), rawBody)
	if err != nil {
		return nil, err
	}
	return &preparedCall{mutation: mutation, parameters: parameters, body: body, tenant: tenant}, nil
}

// handle runs the handler in the transaction of ctx and marshals its response.
func (p *preparedCall) handle(ctx context.Context) (int, json.RawMessage, error) {
	response, err := p.mutation.Handle(ctx, p.parameters, p.body)
	if err != nil {
		return 0, nil, err
	}
	return marshalResponse(p.mutation.Name(), p.mutation.Status(), p.mutation.HasContent(), response)
}

func marshalResponse(name string, status int, hasContent bool, response any) (int, json.RawMessage, error) {
	if !hasContent {
		return status, nil, nil
	}
	payload, err := json.Marshal(response)
	if err != nil {
		return 0, nil, fmt.Errorf("livewire: marshal the response of %q: %w", name, err)
	}
	return status, payload, nil
}

// Batch runs several mutations in one engine transaction, all or nothing (D33): every call is
// checked and decoded first (a refusal answers before any transaction begins), the calls
// must share one tenant, the batch's Idempotency-Key is reserved once, the handlers run in
// order, and the first failure rolls everything back. The payload is a JSON array of
// BatchResult. A refusal names the failing call: its fields are prefixed with
// "calls.<index>." and "calls.<index>" carries its code. Uploads are excluded. Batch reports
// an OutcomeCall per call that ran, an OutcomeBatch, and an OutcomeAfterCommit per call after
// the commit.
func (e *Engine) Batch(ctx context.Context, request BatchRequest) (json.RawMessage, error) {
	return e.batch(ctx, "", request)
}

func (e *Engine) batch(ctx context.Context, operation Operation, request BatchRequest) (json.RawMessage, error) {
	unit := startUnit(OutcomeBatch, operation, batchMutation)
	prepared, err := e.prepareBatch(ctx, request.Body)
	if err != nil {
		e.registry.Observe(ctx, unit.outcome(ctx, err))
		return nil, err
	}
	ctx = WithTenant(ctx, prepared[0].tenant)
	plan := writePlan{mutation: batchMutation, idempotencyKey: request.IdempotencyKey, requestID: request.RequestID}
	answer, err := e.write(ctx, plan, func(ctx context.Context) (int, json.RawMessage, error) {
		return e.runBatch(ctx, operation, prepared)
	})
	e.registry.Observe(ctx, unit.outcome(ctx, err))
	if err != nil {
		return nil, err
	}
	if !answer.replayed {
		for _, call := range prepared {
			e.registry.Observe(ctx, Outcome{Kind: OutcomeAfterCommit, Target: call.mutation.Name(), Tenant: call.tenant, Identity: auth.IdentityFrom(ctx), Parameters: call.parameters, Status: call.mutation.Status()})
		}
	}
	return answer.payload, nil
}

// prepareBatch decodes the batch and prepares every call.
func (e *Engine) prepareBatch(ctx context.Context, body json.RawMessage) ([]*preparedCall, error) {
	var calls []BatchCall
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&calls); err != nil || decoder.More() {
		return nil, invalidParameter("calls", "must be a JSON array of calls {target, params, body}")
	}
	switch {
	case len(calls) == 0:
		return nil, invalidParameter("calls", "at least one")
	case len(calls) > maximumBatchCalls:
		return nil, invalidParameter("calls", "at most "+strconv.Itoa(maximumBatchCalls))
	}
	prepared := make([]*preparedCall, 0, len(calls))
	for index, call := range calls {
		preparedCall, err := e.prepareCall(ctx, call.Target, MapSource(call.Parameters), call.Body)
		if err != nil {
			return nil, batchFailure(index, err)
		}
		if len(prepared) > 0 && preparedCall.tenant != prepared[0].tenant {
			return nil, invalidParameter("calls", "the calls of a batch must share one tenant")
		}
		prepared = append(prepared, preparedCall)
	}
	return prepared, nil
}

// runBatch runs the prepared calls in the transaction of ctx; the first failure ends it.
func (e *Engine) runBatch(ctx context.Context, operation Operation, prepared []*preparedCall) (int, json.RawMessage, error) {
	results := make([]BatchResult, 0, len(prepared))
	for index, call := range prepared {
		unit := startUnit(OutcomeCall, operation, call.mutation.Name())
		status, payload, err := callRecovering(ctx, call)
		outcome := unit.outcome(ctx, err)
		outcome.Parameters = call.parameters
		if err == nil {
			outcome.Status = status
		}
		e.registry.Observe(ctx, outcome)
		if err != nil {
			return 0, nil, batchFailure(index, err)
		}
		results = append(results, BatchResult{Status: status, Payload: payload})
	}
	payload, err := json.Marshal(results)
	if err != nil {
		return 0, nil, fmt.Errorf("livewire: marshal the batch results: %w", err)
	}
	return http.StatusOK, payload, nil
}

func callRecovering(ctx context.Context, call *preparedCall) (status int, payload json.RawMessage, err error) {
	err = recovering(func() error {
		var handleError error
		status, payload, handleError = call.handle(ctx)
		return handleError
	})
	return status, payload, err
}

// batchCallError is a failed call of a batch: refusal is what the batch answers (the call's
// body under "calls.<index>"), cause the call's own error for errors.Is and the Observer.
type batchCallError struct {
	refusal *Error
	cause   error
}

func (b *batchCallError) Error() string   { return b.refusal.Error() + ": " + b.cause.Error() }
func (b *batchCallError) Unwrap() []error { return []error{b.refusal, b.cause} }

func batchFailure(index int, err error) error {
	status, body := ResponseFor(err)
	prefix := "calls." + strconv.Itoa(index)
	fields := map[string]string{prefix: string(body.Code)}
	for name, text := range body.Fields {
		fields[prefix+"."+name] = text
	}
	body.Fields = fields
	return &batchCallError{refusal: &Error{status: status, body: body}, cause: err}
}

// Upload runs one upload for the identity in ctx (D17), REST only: the static Check, the
// parameters, the tenant, the file against the declared content types and size, then the
// engine transaction with the Idempotency-Key as Call has it. It answers the upload's status
// and the marshaled response, and reports like Call.
func (e *Engine) Upload(ctx context.Context, request UploadRequest) (status int, payload json.RawMessage, err error) {
	unit := startUnit(OutcomeCall, "", request.Target)
	prepared, err := e.prepareUpload(ctx, request)
	if err != nil {
		e.registry.Observe(ctx, unit.outcome(ctx, err))
		return 0, nil, err
	}
	ctx = WithTenant(ctx, prepared.tenant)
	plan := writePlan{mutation: request.Target, transactionMutation: request.Target, parameters: prepared.parameters, idempotencyKey: request.IdempotencyKey, requestID: request.RequestID}
	answer, err := e.write(ctx, plan, func(ctx context.Context) (int, json.RawMessage, error) {
		response, err := prepared.upload.Handle(ctx, prepared.parameters, request.File)
		if err != nil {
			return 0, nil, err
		}
		return marshalResponse(prepared.upload.Name(), prepared.upload.Status(), true, response)
	})
	e.observeWrite(ctx, unit, prepared.parameters, answer, err)
	if err != nil {
		return 0, nil, err
	}
	return answer.status, answer.payload, nil
}

// preparedUpload is an upload past its checks.
type preparedUpload struct {
	upload     *Upload
	parameters any
	tenant     string
}

func (e *Engine) prepareUpload(ctx context.Context, request UploadRequest) (*preparedUpload, error) {
	declaration, _ := e.registry.Lookup(request.Target)
	upload, ok := declaration.(*Upload)
	if !ok {
		return nil, NotFound()
	}
	if err := upload.Check()(ctx); err != nil {
		return nil, err
	}
	parameters, err := upload.DecodeParameters(sourceOrEmpty(request.Source))
	if err != nil {
		return nil, err
	}
	tenant, err := e.registry.Tenant(ctx, parameters)
	if err != nil {
		return nil, err
	}
	if err := checkFile(upload, request.File); err != nil {
		return nil, err
	}
	return &preparedUpload{upload: upload, parameters: parameters, tenant: tenant}, nil
}

// checkFile holds the file against the declared content types and size (D17).
func checkFile(upload *Upload, file *File) error {
	switch {
	case file == nil || file.Body == nil:
		return invalidParameter("file", "required")
	case upload.Accepts(file.ContentType) == "":
		return invalidParameter("file", "the content type is not accepted")
	case file.Size > upload.MaximumBytes():
		return invalidParameter("file", "larger than "+strconv.FormatInt(upload.MaximumBytes(), 10)+" bytes")
	}
	return nil
}

// observeWrite reports a call or upload and, after a commit, its OutcomeAfterCommit.
func (e *Engine) observeWrite(ctx context.Context, unit unitOfWork, parameters any, answer *writeAnswer, err error) {
	outcome := unit.outcome(ctx, err)
	outcome.Parameters = parameters
	if err == nil {
		outcome.Status = answer.status
	}
	e.registry.Observe(ctx, outcome)
	if err == nil && !answer.replayed {
		committed := outcome
		committed.Kind, committed.Operation, committed.Err, committed.Duration = OutcomeAfterCommit, "", nil, 0
		e.registry.Observe(ctx, committed)
	}
}

// writePlan is what a write's transaction records: the name the key is reserved under, the
// mutation the transaction names ("" for a batch's outer transaction), the parameters, the
// Idempotency-Key and the request id.
type writePlan struct {
	mutation            string
	transactionMutation string
	parameters          any
	idempotencyKey      string
	requestID           string
}

// writeAnswer is a write's answer; replayed marks a recorded answer of an earlier call.
type writeAnswer struct {
	status   int
	payload  json.RawMessage
	replayed bool
}

// write runs one write in an engine transaction (D31): begin with the tenant and actor,
// reserve the key, run, record the answer, commit. Anything that fails rolls back, which
// frees the key.
func (e *Engine) write(ctx context.Context, plan writePlan, run func(ctx context.Context) (int, json.RawMessage, error)) (*writeAnswer, error) {
	if plan.idempotencyKey != "" {
		if _, err := uuid.Parse(plan.idempotencyKey); err != nil {
			return nil, invalidParameter("idempotencyKey", "must be a UUID")
		}
	}
	if plan.requestID == "" {
		plan.requestID = uuid.NewString()
	}
	transaction, err := e.options.Transactor.Begin(ctx, TransactionDetails{
		Tenant: TenantFrom(ctx), Identity: auth.IdentityFrom(ctx), Mutation: plan.transactionMutation,
		Parameters: plan.parameters, RequestID: plan.requestID,
	})
	if err != nil {
		return nil, fmt.Errorf("livewire: begin the transaction of %q: %w", plan.mutation, err)
	}
	ctx = WithIdempotencyKey(WithTransaction(ctx, transaction.Handle()), plan.idempotencyKey)
	answer, err := e.runReserved(ctx, plan, run)
	if err != nil || answer.replayed {
		if rollbackError := transaction.Rollback(ctx); rollbackError != nil {
			return nil, errors.Join(err, fmt.Errorf("livewire: roll back the transaction of %q: %w", plan.mutation, rollbackError))
		}
		return answer, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, fmt.Errorf("livewire: commit the transaction of %q: %w", plan.mutation, err)
	}
	return answer, nil
}

// runReserved reserves the key, runs and records the answer; a finished key replays.
func (e *Engine) runReserved(ctx context.Context, plan writePlan, run func(ctx context.Context) (int, json.RawMessage, error)) (*writeAnswer, error) {
	store := e.options.IdempotencyStore
	owner, hasOwner := OwnerOf(ctx)
	reserved := plan.idempotencyKey != "" && hasOwner && store != nil
	if reserved {
		record, replay, err := store.Reserve(ctx, owner, plan.idempotencyKey, plan.mutation)
		if err != nil {
			return nil, err
		}
		if replay {
			return &writeAnswer{status: record.Status, payload: record.Response, replayed: true}, nil
		}
	}
	var answer writeAnswer
	err := recovering(func() error {
		var runError error
		answer.status, answer.payload, runError = run(ctx)
		return runError
	})
	if err != nil {
		return nil, err
	}
	if reserved {
		record := &IdempotencyRecord{Owner: owner, Key: plan.idempotencyKey, Mutation: plan.mutation, Status: answer.status, Response: answer.payload}
		if err := store.Record(ctx, record); err != nil {
			return nil, fmt.Errorf("livewire: record the answer of %q: %w", plan.mutation, err)
		}
	}
	return &answer, nil
}
