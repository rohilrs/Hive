package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestPendingConfirmResolveRoundTrip(t *testing.T) {
	d := &Daemon{}
	ch := d.RegisterPendingConfirm("call-1")
	go func() {
		time.Sleep(20 * time.Millisecond)
		d.ResolveConfirm("call-1", confirmDecision{Approve: true})
	}()
	select {
	case got := <-ch:
		if !got.Approve {
			t.Errorf("Approve=false, want true")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for resolve")
	}
}

func TestResolveConfirmAfterClearIsNoop(t *testing.T) {
	d := &Daemon{}
	_ = d.RegisterPendingConfirm("call-x")
	d.ClearPendingConfirm("call-x")
	// resolving a cleared id must not panic and must not block
	done := make(chan struct{})
	go func() { d.ResolveConfirm("call-x", confirmDecision{Approve: true}); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ResolveConfirm blocked after Clear")
	}
}

func TestRegisterChatStreamLookup(t *testing.T) {
	d := &Daemon{}
	type fakeConn struct{ name string }
	c := &fakeConn{name: "x"}
	d.RegisterChatStream("sess-1", c)
	got, ok := d.lookupChatStream("sess-1")
	if !ok || got != c {
		t.Errorf("lookupChatStream=%v ok=%v, want %v", got, ok, c)
	}
	d.UnregisterChatStream("sess-1")
	if _, ok := d.lookupChatStream("sess-1"); ok {
		t.Errorf("lookupChatStream after Unregister: still present")
	}
}

func TestHandleChatConfirmRejectsNonObjectEditedInput(t *testing.T) {
	// edited_input must be a JSON object. A string, number, array,
	// boolean, or null should all be rejected with -32602.
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	cases := []struct {
		name    string
		payload string
	}{
		{"string", `"abc"`},
		{"number", `42`},
		{"array", `[1,2,3]`},
		{"bool", `true`},
		{"null", `null`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := `{"session_id":"s","tool_call_id":"tc","approve":true,"edited_input":` + c.payload + `}`
			params := json.RawMessage(raw)
			_, rpcErr := srv.handleChatConfirm(context.Background(), params)
			if rpcErr == nil {
				t.Fatalf("expected -32602 invalid params for edited_input=%s, got nil", c.payload)
			}
			if rpcErr.Code != rpc.ErrInvalidParams {
				t.Errorf("code=%d, want %d", rpcErr.Code, rpc.ErrInvalidParams)
			}
		})
	}
}

func TestHandleChatConfirmAcceptsObjectEditedInput(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params := json.RawMessage(`{"session_id":"s","tool_call_id":"tc","approve":true,"edited_input":{"k":"v"}}`)
	_, rpcErr := srv.handleChatConfirm(context.Background(), params)
	if rpcErr != nil {
		t.Errorf("valid object edited_input should not error: %v", rpcErr)
	}
}

func TestHandleChatConfirmAcceptsAbsentEditedInput(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params := json.RawMessage(`{"session_id":"s","tool_call_id":"tc","approve":false}`)
	_, rpcErr := srv.handleChatConfirm(context.Background(), params)
	if rpcErr != nil {
		t.Errorf("absent edited_input should not error: %v", rpcErr)
	}
}

func TestHandleChatConfirmAcceptsEmptyObjectEditedInput(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params := json.RawMessage(`{"session_id":"s","tool_call_id":"tc","approve":true,"edited_input":{}}`)
	_, rpcErr := srv.handleChatConfirm(context.Background(), params)
	if rpcErr != nil {
		t.Errorf("empty object {} should be a valid edited_input: %v", rpcErr)
	}
}
