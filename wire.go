package livewire

import "encoding/json"

// Operation is a socket request's operation (D2); the JSON field is "op".
type Operation string

// The socket operations. Uploads are REST only.
const (
	OperationQuery       Operation = "query"       // one read, answered in the response
	OperationSubscribe   Operation = "subscribe"   // a live read; the response names the subscription, pushes follow
	OperationUnsubscribe Operation = "unsubscribe" // target is the subscription
	OperationCall        Operation = "call"        // a mutation
	OperationBatch       Operation = "batch"       // several mutations in one transaction (D33)
	OperationJoin        Operation = "join"        // presence (D37)
	OperationLeave       Operation = "leave"
	OperationPing        Operation = "ping"
)

// RequestFrame is one client request on the socket. Target names the declaration of a read
// with the read kind after a dot ("parts", "parts.item", "parts.distinct", "parts.presence";
// ReadKind), the mutation of a call, the declaration of a join or leave, and the
// subscription of an unsubscribe. Parameters are the target's parameters by name as a REST
// query string would carry them (a list parameter is one comma-separated value); Body is the
// mutation body of a call, the calls of a batch (a JSON array of BatchCall) or the state of
// a join; IdempotencyKey is the Idempotency-Key of a call or batch (D16).
type RequestFrame struct {
	ID             string            `json:"id"`
	Operation      Operation         `json:"op"`
	Target         string            `json:"target,omitempty"`
	Parameters     map[string]string `json:"params,omitempty"`
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
	PushSnapshot   PushKind = "snapshot" // a list's full ListResponse body: the first push and after a resync
	PushListChange PushKind = "diff"     // a ListChange against the last pushed list
	PushReplace    PushKind = "replace"  // an object's full body
	PushError      PushKind = "error"    // an ErrorBody; the subscription has ended
)

// PushFrame is one change of a subscription; Sequence counts from 1 per subscription.
type PushFrame struct {
	Subscription string          `json:"sub"`
	Sequence     int64           `json:"seq"`
	Kind         PushKind        `json:"kind"`
	Payload      json.RawMessage `json:"payload"`
}

// ListChange is a list change (the wire's "diff"): replace or insert the upserted items by
// id, drop the removed ids, re-sort to Order when it is present (the full id list, sent when
// membership or order changed) and take Total when it is present (D14). Upserts and Removes
// are never null.
type ListChange struct {
	Upserts []json.RawMessage `json:"upserts"`
	Removes []string          `json:"removes"`
	Order   []string          `json:"order,omitempty"`
	Total   *int              `json:"total,omitempty"`
}

// The close codes of a socket.
const (
	CloseNormal       = 1000 // the client ended the socket
	CloseGoingAway    = 1001 // Engine.Close ends every socket with it (section 5)
	CloseTooSlow      = 1013 // the client fell behind the per-socket queue limit (D10)
	CloseIdentityGone = 4401 // the identity is gone: no session, expired, revoked (D7)
)
