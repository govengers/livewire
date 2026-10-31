package livewire_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/govengers/livewire"
)

func TestFrameRoundTrips(t *testing.T) {
	testCases := []struct {
		name  string
		frame any
		want  string
	}{
		{
			name:  "request",
			frame: &livewire.RequestFrame{ID: "1", Op: livewire.OpCall, Target: "createPart", Params: map[string]string{"id": "7"}, Body: json.RawMessage(`{"name":"bolt"}`), IdempotencyKey: "key"},
			want:  `{"id":"1","op":"call","target":"createPart","params":{"id":"7"},"body":{"name":"bolt"},"idempotencyKey":"key"}`,
		},
		{
			name:  "request minimal",
			frame: &livewire.RequestFrame{ID: "2", Op: livewire.OpPing},
			want:  `{"id":"2","op":"ping"}`,
		},
		{
			name:  "response success",
			frame: &livewire.ResponseFrame{ID: "1", OK: true, Status: 200, Payload: json.RawMessage(`{"items":[]}`), Subscription: "s1", FilterID: "f1"},
			want:  `{"id":"1","ok":true,"status":200,"payload":{"items":[]},"subscription":"s1","filterId":"f1"}`,
		},
		{
			name:  "response error",
			frame: &livewire.ResponseFrame{ID: "1", Status: 404, Error: &livewire.ErrorBody{Code: livewire.CodeNotFound, Message: "not found"}},
			want:  `{"id":"1","ok":false,"status":404,"error":{"code":"not_found","message":"not found"}}`,
		},
		{
			name:  "push",
			frame: &livewire.PushFrame{Subscription: "s1", Sequence: 3, Kind: livewire.PushDiff, Payload: json.RawMessage(`{"upserts":[],"removes":["a"]}`)},
			want:  `{"sub":"s1","seq":3,"kind":"diff","payload":{"upserts":[],"removes":["a"]}}`,
		},
		{
			name:  "diff",
			frame: &livewire.Diff{Upserts: []json.RawMessage{json.RawMessage(`{"id":"a"}`)}, Removes: []string{}, Order: []string{"a"}, Total: new(1)},
			want:  `{"upserts":[{"id":"a"}],"removes":[],"order":["a"],"total":1}`,
		},
		{
			name:  "diff without order and total",
			frame: &livewire.Diff{Upserts: []json.RawMessage{}, Removes: []string{"b"}},
			want:  `{"upserts":[],"removes":["b"]}`,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			raw, err := json.Marshal(testCase.frame)
			require.NoError(t, err)
			require.JSONEq(t, testCase.want, string(raw))

			decoded := newOf(testCase.frame)
			require.NoError(t, json.Unmarshal(raw, decoded))
			require.Equal(t, testCase.frame, decoded)
		})
	}
}

func newOf(frame any) any {
	switch frame.(type) {
	case *livewire.RequestFrame:
		return &livewire.RequestFrame{}
	case *livewire.ResponseFrame:
		return &livewire.ResponseFrame{}
	case *livewire.PushFrame:
		return &livewire.PushFrame{}
	default:
		return &livewire.Diff{}
	}
}

func TestWireConstants(t *testing.T) {
	require.Equal(t, 1000, livewire.CloseNormal)
	require.Equal(t, 1013, livewire.CloseTooSlow)
	require.Equal(t, 4401, livewire.CloseIdentityGone)
	require.Equal(t, []livewire.Op{"query", "subscribe", "unsubscribe", "call", "batch", "join", "leave", "ping"},
		[]livewire.Op{livewire.OpQuery, livewire.OpSubscribe, livewire.OpUnsubscribe, livewire.OpCall, livewire.OpBatch, livewire.OpJoin, livewire.OpLeave, livewire.OpPing})
	require.Equal(t, []livewire.PushKind{"snapshot", "diff", "replace", "error"},
		[]livewire.PushKind{livewire.PushSnapshot, livewire.PushDiff, livewire.PushReplace, livewire.PushError})
}
