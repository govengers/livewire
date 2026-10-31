package livewire

import "encoding/json"

// Op is a socket request's operation (D2).
type Op string

// The socket operations. Uploads are REST only.
const (
	OpQuery       Op = "query"       // one read, answered in the response
	OpSubscribe   Op = "subscribe"   // a live read; the response names the subscription, pushes follow
	OpUnsubscribe Op = "unsubscribe" // target is the subscription
	OpCall        Op = "call"        // a mutation
	OpBatch       Op = "batch"       // several mutations in one transaction (D33)
	OpJoin        Op = "join"        // presence (D37)
	OpLeave       Op = "leave"
	OpPing        Op = "ping"
)

// RequestFrame is one client request on the socket. Params are the target's parameters by
// name as a REST query string would carry them (a list parameter is one comma-separated
// value); Body is the mutation body of a call or the calls of a batch; IdempotencyKey is the
// Idempotency-Key of a call or batch (D16).
type RequestFrame struct {
	ID             string            `json:"id"`
	Op             Op                `json:"op"`
	Target         string            `json:"target,omitempty"`
	Params         map[string]string `json:"params,omitempty"`
	Body           json.RawMessage   `json:"body,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
}

// ResponseFrame answers one request by its ID. Status is the HTTP status the same request
// gets on REST; Payload is byte-for-byte the REST body on success, Error the body of a
// refusal. Subscription names the subscription a subscribe opened, FilterID the stored
// filter an inline filter was registered as (D13).
type ResponseFrame struct {
	ID           string          `json:"id"`
	OK           bool            `json:"ok"`
	Status       int             `json:"status"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Error        *ErrorBody      `json:"error,omitempty"`
	Subscription string          `json:"subscription,omitempty"`
	FilterID     string          `json:"filterId,omitempty"`
}

// PushKind says what a push carries.
type PushKind string

// The push kinds.
const (
	PushSnapshot PushKind = "snapshot" // a list's full ListResponse body: the first push and after a resync
	PushDiff     PushKind = "diff"     // a Diff against the last pushed list
	PushReplace  PushKind = "replace"  // an object's full body
	PushError    PushKind = "error"    // an ErrorBody; the subscription has ended
)

// PushFrame is one change of a subscription; Sequence counts from 1 per subscription.
type PushFrame struct {
	Subscription string          `json:"sub"`
	Sequence     int64           `json:"seq"`
	Kind         PushKind        `json:"kind"`
	Payload      json.RawMessage `json:"payload"`
}

// Diff is a list change: replace or insert the upserted items by id, drop the removed ids,
// re-sort to Order when it is present (the full id list, sent when membership or order
// changed) and take Total when it is present (D14). Upserts and Removes are never null.
type Diff struct {
	Upserts []json.RawMessage `json:"upserts"`
	Removes []string          `json:"removes"`
	Order   []string          `json:"order,omitempty"`
	Total   *int              `json:"total,omitempty"`
}

// The close codes of a socket.
const (
	CloseNormal       = 1000 // the client or Engine.Close ended the socket
	CloseTooSlow      = 1013 // the client fell behind the per-socket queue limit (D10)
	CloseIdentityGone = 4401 // the identity is gone: no session, expired, revoked (D7)
)
