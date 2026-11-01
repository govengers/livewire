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
// safe for concurrent use. Every stored row carries the tenant of D42 and every lookup is
// scoped by it: with a single tenant the tenant is "" everywhere and changes nothing.

// Owner scopes a stored row to who stored it: the identity key "provider:subject" and the
// tenant of the request (D42, "" in a single-tenant registry). Two owners are the same only
// when both parts are equal; a key of another owner is a fresh key (section 6).
type Owner struct {
	Key    string
	Tenant string
}

// OwnerOf is the owner of the identity in ctx under the tenant in ctx (TenantFrom). Without
// an identity there is no owner (false): the engine then skips idempotency and stored
// filters, so anonymous callers never share a namespace (D16).
func OwnerOf(ctx context.Context) (Owner, bool) {
	identity := auth.IdentityFrom(ctx)
	if identity == nil {
		return Owner{}, false
	}
	return Owner{Key: OwnerKey(identity), Tenant: TenantFrom(ctx)}, true
}

// OwnerKey is the key an identity owns rows, sockets and stored filters under:
// "provider:subject". Engine.RevalidateOwner and SocketsPerOwner take the same key; a nil
// identity has the key "".
func OwnerKey(identity *auth.Identity) string {
	if identity == nil {
		return ""
	}
	return identity.Provider + ":" + identity.Subject
}

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
// upgrade request on every subscribe, call, re-run and tick. The identity is gone, and the
// socket closes with CloseIdentityGone, when the source returns an error matching
// auth.ErrUnauthenticated (no session, expired, revoked: what session.Guard.Identify
// returns) or auth.ErrForbidden (a refused refresh), or a nil identity with a nil error. Any
// other error is a failure of the source: the engine keeps the last identity and retries.
type IdentitySource interface {
	Identify(ctx context.Context, request *http.Request) (*auth.Identity, error)
}

// Transaction is an engine transaction (D31, D42, D45) as the core sees it. Only the engine
// holds it: exactly one of Commit and Rollback is called, once. Handle is the query-only
// value the engine puts into the unit of work's context (WithTransaction): a value that can
// run statements in the transaction but has no Commit or Rollback (postgresadapter hands
// out sqlc's DBTX method set, never the pgx.Tx), so a handler or loader cannot end the
// engine's transaction.
type Transaction interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
	Handle() any
}

// TransactionDetails is what a transaction records for the triggers and policies (D31, D32,
// D42): the tenant (SET LOCAL livewire.tenant, "" with a single tenant), the effective
// identity (its Real() is the real actor), the mutation's name and decoded parameters and
// the request id (SET LOCAL livewire.actor). ReadOnly marks the REPEATABLE READ READ ONLY
// transaction of a read (D45: every engine unit of work has one): it carries the tenant and
// no actor; Mutation is empty then and for a batch's outer transaction.
type TransactionDetails struct {
	Tenant     string
	Identity   *auth.Identity
	Mutation   string
	Parameters any
	RequestID  string
	ReadOnly   bool
}

// CallDescriber is the optional interface of a Transaction that runs several calls (D58): the
// engine calls DescribeCall before each call of a batch (D33) with that call's details, so
// the triggers attribute each call's writes to its own mutation and request id (D34); the
// postgresadapter sets livewire.actor (actor, real actor, mutation, request id) again (SET
// LOCAL). The call's parameters reach its audit entry only. Without it a batch's history rows
// carry the outer transaction's details, whose Mutation is empty. A failing DescribeCall
// fails the batch.
type CallDescriber interface {
	DescribeCall(ctx context.Context, transactionDetails TransactionDetails) error
}

// Transactor begins an engine transaction (D31, D42). The implementation sets the tenant and,
// for a write, the actor of transactionDetails (SET LOCAL) before it returns, so triggers and
// Row Level Security policies see them.
type Transactor interface {
	Begin(ctx context.Context, transactionDetails TransactionDetails) (Transaction, error)
}

// StoredFilter is a filter document a client stored for a resource (D13). Context is the
// app's dimension of the dedup key (a view or page name, "" for none); Name is empty for an
// unnamed filter, which is swept after 30 days without use; Hash is query.Hash of Document.
// Dedup is per owner, resource, context and hash.
type StoredFilter struct {
	ID         string
	Owner      Owner
	Resource   string
	Context    string
	Name       string
	Document   json.RawMessage
	Hash       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// FilterStore keeps the stored filters of D13. Filters are visible to their owner only, the
// tenant included: every method takes the owner and treats another owner's filter as absent.
type FilterStore interface {
	// Save stores filter and returns the stored one with its ID; an unnamed filter whose
	// Hash the owner already stored for the resource and context returns that filter.
	Save(ctx context.Context, filter *StoredFilter) (*StoredFilter, error)
	// Get returns the owner's filter and marks it used; it returns NotFound when there is
	// none.
	Get(ctx context.Context, owner Owner, id string) (*StoredFilter, error)
	// List returns the owner's named filters of a resource, every resource when resource is
	// empty; unnamed filters (inline subscribe registrations) are not listed.
	List(ctx context.Context, owner Owner, resource string) ([]StoredFilter, error)
	// Delete removes the owner's filter; a filter that is not there is no error.
	Delete(ctx context.Context, owner Owner, id string) error
}

// IdempotencyRecord is the recorded answer of a mutation call (D16): the owner, the key the
// client sent, the mutation it was sent for, and the status and body that were answered.
// Status is 0 while the call is in flight.
type IdempotencyRecord struct {
	Owner    Owner
	Key      string
	Mutation string
	Status   int
	Response json.RawMessage
}

// IdempotencyStore keeps the records of D16. Both methods run inside the mutation's
// transaction (TransactionFrom(ctx)), so a rolled-back call frees its key. Reserve must not
// block on another transaction's reservation of the same key: the postgresadapter takes
// pg_try_advisory_xact_lock over a hash of the owner and key, answers InProgress when the
// lock is held, replays a committed record, and inserts the reservation otherwise.
type IdempotencyStore interface {
	// Reserve claims the key for the owner and mutation. A fresh key returns replay false;
	// a key whose call finished returns the record and replay true; a key whose call is
	// still in flight returns InProgress; a key recorded for another mutation returns
	// Conflict.
	Reserve(ctx context.Context, owner Owner, key, mutation string) (record *IdempotencyRecord, replay bool, err error)
	// Record stores the answer of a reserved key.
	Record(ctx context.Context, record *IdempotencyRecord) error
}

// AuditEntry is one mutation call as the action audit of D32 records it: the tenant, the
// effective identity (its Real() is the real actor), the mutation, its decoded parameters, the
// request id, the Idempotency-Key ("" for none), the answered status and how long the call
// took.
type AuditEntry struct {
	Tenant         string
	Identity       *auth.Identity
	Mutation       string
	Parameters     any
	RequestID      string
	IdempotencyKey string
	Status         int
	Duration       time.Duration
}

// AuditStore keeps the action audit of D32. The engine writes one entry per call whose
// handler ran: a mutation, an upload and every call of a batch. For a call that commits it
// writes the entry in the mutation's transaction (TransactionFrom(ctx)) between the handler
// and the idempotency record, so the entry commits and rolls back with the call (section 7);
// a failing Write fails the call. For a call that failed it writes the entry after the
// rollback, without a transaction in ctx and on a context the caller's cancellation does not
// reach (bounded), so the store writes it on its own: the failed
// calls of a batch each with the status the batch answered. A refusal before the handler (a
// check, a decoding, a replayed or in-flight key) writes none.
type AuditStore interface {
	Write(ctx context.Context, entry *AuditEntry) error
}

// Presence is one socket's presence on a target record (D37): the socket, its owner and
// display name, the target and key that name the record, and the client's state.
type Presence struct {
	Socket    string
	Owner     Owner
	Display   string
	Target    string
	Key       string
	State     json.RawMessage
	UpdatedAt time.Time
}

// PresenceStore keeps the rows of D37; a change to them reaches the engine through the
// ChangeFeed like any other table. A record is named by tenant, target and key.
type PresenceStore interface {
	// Join inserts or updates the socket's presence on the record.
	Join(ctx context.Context, presence *Presence) error
	// Leave removes the socket's presence on the tenant's record; an absent one is no error.
	Leave(ctx context.Context, tenant, socket, target, key string) error
	// Heartbeat refreshes UpdatedAt of every presence of the socket.
	Heartbeat(ctx context.Context, socket string) error
	// List returns the presences on a record of the tenant; a store may bound how many it
	// returns (postgresadapter: 1000, by socket).
	List(ctx context.Context, tenant, target, key string) ([]Presence, error)
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
	OutcomeOpen        OutcomeKind = "open"         // a socket was upgraded
	OutcomeClose       OutcomeKind = "close"        // a socket ended; Status is its close code (1000, 1001, 1013, 4401, ...)
	OutcomeRevalidate  OutcomeKind = "revalidate"   // a socket's identity re-validated on a tick or a Revalidate call (D6)
	OutcomeFeed        OutcomeKind = "feed"         // the change feed failed; the engine restarts it after a backoff
	OutcomeHeartbeat   OutcomeKind = "heartbeat"    // the presence heartbeat and sweep of a tick (D37)
	OutcomeExport      OutcomeKind = "export"       // one export (D35); Rows is how many rows it wrote
	OutcomeEnd         OutcomeKind = "end"          // a subscription a subscribe opened ended: unsubscribed, by an error push or with its socket
)

// Outcome is one unit of work the engine or an adapter finished (D4, D30). Target is the
// declaration's name, with the read kind after a dot for a read other than the main one
// (D46), "batch" for a batch, "filters" for the stored filter routes, and "" when the request
// named no declaration (D59); Operation is set on the socket ("other" for an operation the
// socket does not know), empty on REST; Subscription is set for
// subscribe and rerun; Tenant is the resolved tenant (D42); Parameters is the decoded
// parameters value (a *Parameters) when they were decoded; Status is the HTTP status answered
// (the close code for OutcomeClose); Err is the refusal or failure, with the server-side
// Reason of an *auth.Error, and nil on success; Rows is the rows an export wrote; QueryWait is
// how long the unit waited for the engine's query slots (D52), so it is zero on REST.
type Outcome struct {
	Kind         OutcomeKind
	Operation    Operation
	Target       string
	Subscription string
	Tenant       string
	Identity     *auth.Identity
	Parameters   any
	Status       int
	Err          error
	Duration     time.Duration
	Rows         int
	QueryWait    time.Duration
}

// Observer receives outcomes synchronously with the context of the unit of work. It must
// not block and must be safe for concurrent use; a nil Observer means no reporting.
type Observer func(ctx context.Context, outcome Outcome)
