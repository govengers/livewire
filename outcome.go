package livewire

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/govengers/auth"
)

// observe reports an outcome to the registry's Observer (D49). The Observer is application
// code: a panic in it loses that outcome and nothing else.
func (e *Engine) observe(ctx context.Context, outcome Outcome) {
	_ = recovering(func() error { // the panic of an Observer has nobody left to report to
		e.registry.Observe(ctx, outcome)
		return nil
	})
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
