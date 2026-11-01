package livewire

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/govengers/auth"
)

// observe reports an outcome to the registry's Observer (D49), its target one of the closed
// set reportedTarget allows. The Observer is application code: a panic in it loses that
// outcome and nothing else.
func (e *Engine) observe(ctx context.Context, outcome Outcome) {
	outcome.Target = e.reportedTarget(outcome.Target)
	outcome.Operation = reportedOperation(outcome.Operation)
	_ = recovering(func() error { // the panic of an Observer has nobody left to report to
		e.registry.Observe(ctx, outcome)
		return nil
	})
}

// readKinds are the read kinds a target may name after its dot.
var readKinds = []ReadKind{ReadMain, ReadItem, ReadDistinct, ReadHistory, ReadPresence}

// reportedTarget is the target an outcome reports (D59): a registered declaration's name,
// with a read kind after a dot, "batch", "filters" or "". A target the request named that is
// none of them, an unknown declaration or kind, or a subscription's id, is "", so a metric
// label on it stays a closed set (MET-004).
func (e *Engine) reportedTarget(target string) string {
	if target == "" || target == batchMutation || target == filtersTarget {
		return target
	}
	name, kind := parseTarget(target)
	if _, ok := e.registry.Lookup(name); !ok || !slices.Contains(readKinds, kind) {
		return ""
	}
	return target
}

// operations are the socket operations an outcome reports by name; unknownOperation stands
// for every other a frame named.
var operations = []Operation{
	OperationQuery, OperationSubscribe, OperationUnsubscribe, OperationCall, OperationBatch,
	OperationJoin, OperationLeave, OperationPing,
}

const unknownOperation = "other"

// reportedOperation is the operation an outcome reports: a socket operation, "" on REST, or
// "other" for an operation the socket does not know, so a metric label on it stays a closed
// set (MET-004).
func reportedOperation(operation Operation) Operation {
	if operation == "" || slices.Contains(operations, operation) {
		return operation
	}
	return Operation(unknownOperation)
}

// unitOfWork carries what an outcome reports from the start of a frame, request, re-run or
// tick to its end (D4, D30): the kind, the socket op, the target, the subscription.
type unitOfWork struct {
	kind         OutcomeKind
	operation    Operation
	target       string
	subscription string
	started      time.Time
}

func startUnit(kind OutcomeKind, operation Operation, target string) unitOfWork {
	return unitOfWork{kind: kind, operation: operation, target: target, started: time.Now()}
}

// outcome reports the unit with the identity and tenant of ctx and the status err answers.
func (u unitOfWork) outcome(ctx context.Context, err error) Outcome {
	return Outcome{
		Kind: u.kind, Operation: u.operation, Target: u.target, Subscription: u.subscription,
		Identity: auth.IdentityFrom(ctx), Tenant: TenantFrom(ctx), Status: statusOf(err), Err: err, Duration: time.Since(u.started),
		QueryWait: queryWaitOf(ctx),
	}
}

// readOutcome is outcome with what the read resolved; resolved is nil when it failed before.
func (u unitOfWork) readOutcome(ctx context.Context, resolved *resolvedRead, err error) Outcome {
	outcome := u.outcome(ctx, err)
	if resolved != nil {
		outcome.Tenant, outcome.Parameters = resolved.tenant, resolved.parameters
	}
	return outcome
}

// statusOf is the status an outcome reports: 200 without an error, 401 for a gone identity
// (its socket closed 4401, nothing was answered), else what ResponseFor answers.
func statusOf(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, errIdentityGone):
		return http.StatusUnauthorized
	}
	status, _ := ResponseFor(err)
	return status
}
