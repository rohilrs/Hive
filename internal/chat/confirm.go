package chat

import (
	"context"
	"encoding/json"
)

// ConfirmDecision is what a ConfirmGate returns from a Propose call. The
// EditedInput field lets a user (via a TUI/UI) tweak args before approval;
// when nil the agent uses the original input.
type ConfirmDecision struct {
	Approve     bool
	EditedInput json.RawMessage // nil → use the original input
	DenyReason  string          // optional human reason (model gets it on deny)
}

// ConfirmGate gates the execution of a Mutating tool on user (or policy)
// approval. Implementations may be in-process (NoopConfirmGate / DenyAll for
// tests) or daemon-backed (block on a pending channel resolved by a
// chat.confirm RPC).
//
// sessionID is the chat.send session id; toolCallID uniquely identifies
// this specific tool invocation within that session (so the RPC can resolve
// the right one); tool is the registered tool name; input is the model's
// proposed args JSON.
type ConfirmGate interface {
	Propose(ctx context.Context, sessionID, toolCallID, tool string, input json.RawMessage) (ConfirmDecision, error)
}

// NoopConfirmGate approves every proposal. Used as the default when the
// daemon doesn't inject a gate (so existing tests keep their behavior).
type NoopConfirmGate struct{}

func (NoopConfirmGate) Propose(_ context.Context, _, _, _ string, _ json.RawMessage) (ConfirmDecision, error) {
	return ConfirmDecision{Approve: true}, nil
}

// DenyAllConfirmGate denies every proposal with the given Reason. Used in
// tests to assert the SDKAgent's deny-path (synthetic ToolResult + loop
// continues).
type DenyAllConfirmGate struct{ Reason string }

func (g DenyAllConfirmGate) Propose(_ context.Context, _, _, _ string, _ json.RawMessage) (ConfirmDecision, error) {
	return ConfirmDecision{Approve: false, DenyReason: g.Reason}, nil
}
