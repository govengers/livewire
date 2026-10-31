package livewire

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/govengers/auth"
)

// The interfaces the engine consumes and the adapters implement (D3). The core declares
// them so it imports no driver; postgresadapter implements the stores and the feed, auth's
// session.Guard or an API-key resolver the IdentitySource. Every implementation must be
// safe for concurrent use.

// ChangeOperation is what happened to a row.
type ChangeOperation string

// The change operations a trigger reports.
const (
	ChangeInsert ChangeOperation = "insert"
	ChangeUpdate ChangeOperation = "update"
	ChangeDelete ChangeOperation = "delete"
)

// Change is one row change a ChangeFeed reports: the table, the operation, the row's id as
// text and, for partitioned triggers, the value of the partition column (D25); Partition is
// empty otherwise.
type Change struct {
	Table     string
	Operation ChangeOperation
	ID        string
	Partition string
}

// ChangeFeed delivers row changes to the engine. Run blocks until ctx ends and returns
// ctx.Err() then, or the failure that ended the feed for good; it reconnects on its own in
// between. It calls onResync after every successful connect, the first included, because
// changes may have been missed, and onChange once per change; both are called from the
// feed's goroutine and must return quickly.
type ChangeFeed interface {
	Run(ctx context.Context, onChange func(Change), onResync func()) error
}

// IdentitySource re-validates who a socket acts as (D6): the engine calls Identify with the
// upgrade request on every subscribe, call, re-run and tick. A nil identity with a nil
// error means the identity is gone (no session, expired, revoked) and closes the socket
// with CloseIdentityGone; an error is a failure of the source and is retried.
type IdentitySource interface {
	Identify(ctx context.Context, request *http.Request) (*auth.Identity, error)
}

// Transaction is the engine's mutation transaction (D31) as the core sees it. Exactly one
// of Commit and Rollback is called, once.
type Transaction interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// TransactionInfo is what the transaction records as the actor of the mutation (D31, D32):
// the effective identity (its Real() is the real actor), the mutation's name and the
// request id. Mutation is empty and Parameters nil for a batch's outer transaction.
type TransactionInfo struct {
	Identity   *auth.Identity
	Mutation   string
	Parameters any
	RequestID  string
}

// Transactor begins the mutation transaction (D31). The implementation sets the actor of
// info for the triggers (SET LOCAL) before it returns.
type Transactor interface {
	Begin(ctx context.Context, info TransactionInfo) (Transaction, error)
}

// StoredFilter is a filter document a client stored for a resource (D13). Owner is the
// identity key "provider:subject"; Name is empty for an unnamed filter, which is swept
// after 30 days without use; Hash is query.Hash of Document.
type StoredFilter struct {
	ID         string
	Owner      string
	Resource   string
	Name       string
	Document   json.RawMessage
	Hash       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// FilterStore keeps the stored filters of D13. Filters are visible to their owner only:
// every method takes the owner and treats another owner's filter as absent.
type FilterStore interface {
	// Save stores filter and returns the stored one with its ID; an unnamed filter whose
	// Hash the owner already stored for the resource returns that filter instead of a copy.
	Save(ctx context.Context, filter *StoredFilter) (*StoredFilter, error)
	// Get returns the owner's filter and marks it used; it returns NotFound when there is
	// none.
	Get(ctx context.Context, owner, id string) (*StoredFilter, error)
	// List returns the owner's filters of a resource, every resource when resource is empty.
	List(ctx context.Context, owner, resource string) ([]StoredFilter, error)
	// Delete removes the owner's filter; a filter that is not there is no error.
	Delete(ctx context.Context, owner, id string) error
}

// IdempotencyRecord is the recorded answer of a mutation call (D16): the owner, the key the
// client sent, and the status and body that were answered. Status is 0 while the call is in
// flight.
type IdempotencyRecord struct {
	Owner    string
	Key      string
	Status   int
	Response json.RawMessage
}

// IdempotencyStore keeps the records of D16. Both methods run inside the mutation's
// transaction (TransactionFrom(ctx)), so a rolled-back call frees its key.
type IdempotencyStore interface {
	// Reserve claims the key for the owner. A fresh key returns replay false; a key whose
	// call finished returns the record and replay true; a key whose call is still in flight
	// returns InProgress.
	Reserve(ctx context.Context, owner, key string) (record *IdempotencyRecord, replay bool, err error)
	// Record stores the answer of a reserved key.
	Record(ctx context.Context, record *IdempotencyRecord) error
}

// Presence is one socket's presence on a target record (D37): the socket, its owner and
// display name, the target and key that name the record, and the client's state.
type Presence struct {
	Socket    string
	Owner     string
	Display   string
	Target    string
	Key       string
	State     json.RawMessage
	UpdatedAt time.Time
}

// PresenceStore keeps the rows of D37; a change to them reaches the engine through the
// ChangeFeed like any other table.
type PresenceStore interface {
	// Join inserts or updates the socket's presence on the record.
	Join(ctx context.Context, presence *Presence) error
	// Leave removes the socket's presence on the record; an absent one is no error.
	Leave(ctx context.Context, socket, target, key string) error
	// Heartbeat refreshes UpdatedAt of every presence of the socket.
	Heartbeat(ctx context.Context, socket string) error
	// List returns the presences on a record.
	List(ctx context.Context, target, key string) ([]Presence, error)
	// Sweep removes every presence older than maximumAge and returns how many.
	Sweep(ctx context.Context, maximumAge time.Duration) (int, error)
}

// OutcomeKind says which unit of work an Outcome reports.
type OutcomeKind string

// The outcome kinds.
const (
	OutcomeRequest     OutcomeKind = "request"      // one REST request
	OutcomeSubscribe   OutcomeKind = "subscribe"    // a subscribe op, its first load included
	OutcomeCall        OutcomeKind = "call"         // one mutation, on REST or the socket
	OutcomeBatch       OutcomeKind = "batch"        // one batch (D33)
	OutcomeRerun       OutcomeKind = "rerun"        // a subscription re-run after a change, resync or tick
	OutcomeAfterCommit OutcomeKind = "after_commit" // a mutation's transaction committed (D41)
)

// Outcome is one unit of work the engine or an adapter finished (D4, D30). Target is the
// declaration's name; Op is set on the socket, empty on REST; Subscription is set for
// subscribe and rerun; Parameters is the decoded parameters value (a *Params) when they
// were decoded; Status is the HTTP status answered; Err is the refusal or failure, with the
// server-side Reason of an *auth.Error, and nil on success.
type Outcome struct {
	Kind         OutcomeKind
	Op           Op
	Target       string
	Subscription string
	Identity     *auth.Identity
	Parameters   any
	Status       int
	Err          error
	Duration     time.Duration
}

// Observer receives outcomes synchronously with the context of the unit of work. It must
// not block and must be safe for concurrent use; a nil Observer means no reporting.
type Observer func(ctx context.Context, outcome Outcome)
