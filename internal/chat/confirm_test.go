package chat

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNoopConfirmGateApprovesEverything(t *testing.T) {
	g := NoopConfirmGate{}
	got, err := g.Propose(context.Background(), "sess-1", "call-1", "hive_add_task", json.RawMessage(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !got.Approve {
		t.Errorf("Approve=false, want true")
	}
	if got.DenyReason != "" {
		t.Errorf("DenyReason=%q, want empty", got.DenyReason)
	}
}

func TestDenyAllConfirmGateDeniesEverything(t *testing.T) {
	g := DenyAllConfirmGate{Reason: "policy"}
	got, err := g.Propose(context.Background(), "sess-1", "call-1", "hive_run_now", json.RawMessage(`{"task_id":"t1"}`))
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Approve {
		t.Errorf("Approve=true, want false")
	}
	if got.DenyReason != "policy" {
		t.Errorf("DenyReason=%q, want policy", got.DenyReason)
	}
}
