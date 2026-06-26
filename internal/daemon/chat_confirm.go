package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// confirmDecision is the internal payload carried over a pendingConfirms
// channel. The daemonConfirmGate translates it to a chat.ConfirmDecision.
type confirmDecision struct {
	Approve     bool
	EditedInput json.RawMessage
	DenyReason  string
}

// RegisterPendingConfirm allocates a 1-buffered channel keyed by toolCallID
// and returns it. The agent's confirm gate blocks on the channel; a
// chat.confirm RPC resolves it. Buffered so ResolveConfirm doesn't block
// if the receiver gave up early.
func (d *Daemon) RegisterPendingConfirm(toolCallID string) <-chan confirmDecision {
	d.pendingConfirmsMu.Lock()
	defer d.pendingConfirmsMu.Unlock()
	if d.pendingConfirms == nil {
		d.pendingConfirms = map[string]chan confirmDecision{}
	}
	ch := make(chan confirmDecision, 1)
	d.pendingConfirms[toolCallID] = ch
	return ch
}

// ResolveConfirm delivers a decision to the channel registered for toolCallID,
// then deletes the entry. Resolving an unknown/cleared id is a no-op.
func (d *Daemon) ResolveConfirm(toolCallID string, dec confirmDecision) {
	d.pendingConfirmsMu.Lock()
	ch, ok := d.pendingConfirms[toolCallID]
	if ok {
		delete(d.pendingConfirms, toolCallID)
	}
	d.pendingConfirmsMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- dec:
	default: // 1-buffered + receiver may have given up; non-blocking send
	}
}

// ClearPendingConfirm removes a pending entry without sending. Used on
// timeout / context cancel so a late ResolveConfirm becomes a no-op.
func (d *Daemon) ClearPendingConfirm(toolCallID string) {
	d.pendingConfirmsMu.Lock()
	delete(d.pendingConfirms, toolCallID)
	d.pendingConfirmsMu.Unlock()
}

// RegisterChatStream associates an active chat.send conn with its session id
// so handleChatTool (CC provider) and the SDKAgent's gate can emit
// tool_proposed frames on the right stream.
func (d *Daemon) RegisterChatStream(sessionID string, conn any) {
	d.chatStreamsMu.Lock()
	if d.chatStreams == nil {
		d.chatStreams = map[string]any{}
	}
	d.chatStreams[sessionID] = conn
	d.chatStreamsMu.Unlock()
}

// UnregisterChatStream removes the binding when the chat.send stream ends.
func (d *Daemon) UnregisterChatStream(sessionID string) {
	d.chatStreamsMu.Lock()
	delete(d.chatStreams, sessionID)
	d.chatStreamsMu.Unlock()
}

// lookupChatStream is package-internal so tests can assert presence without
// exposing the registry to other packages.
func (d *Daemon) lookupChatStream(sessionID string) (any, bool) {
	d.chatStreamsMu.Lock()
	defer d.chatStreamsMu.Unlock()
	c, ok := d.chatStreams[sessionID]
	return c, ok
}

// daemonConfirmGate implements chat.ConfirmGate by emitting a tool_proposed
// frame on the session's chat.send stream and blocking on a pendingConfirms
// channel until a chat.confirm RPC resolves it, or the timeout elapses (deny).
type daemonConfirmGate struct {
	d              *Daemon
	timeoutSeconds int
}

// Compile-time assertion that daemonConfirmGate satisfies chat.ConfirmGate.
var _ chat.ConfirmGate = (*daemonConfirmGate)(nil)

func newDaemonConfirmGate(d *Daemon, timeoutSeconds int) *daemonConfirmGate {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 300
	}
	return &daemonConfirmGate{d: d, timeoutSeconds: timeoutSeconds}
}

func (g *daemonConfirmGate) Propose(ctx context.Context, sessionID, toolCallID, tool string, input json.RawMessage) (chat.ConfirmDecision, error) {
	// 1. Look up the stream conn for this chat session. If it's gone (the
	//    client disconnected) we must deny — there's no one to ask.
	connAny, ok := g.d.lookupChatStream(sessionID)
	if !ok {
		return chat.ConfirmDecision{Approve: false, DenyReason: "no active chat stream"}, nil
	}
	conn, ok := connAny.(interface {
		Write(b []byte) (int, error)
	})
	if !ok {
		return chat.ConfirmDecision{Approve: false, DenyReason: "stream not writable"}, nil
	}

	// 2. Register the pending channel BEFORE emitting the frame so a
	//    fast client can't race the channel registration.
	ch := g.d.RegisterPendingConfirm(toolCallID)

	// 3. Emit tool_proposed. Same envelope shape as streamChat. reqID empty:
	//    the client correlates by tool_call_id from the frame body.
	resultBytes, _ := json.Marshal(map[string]any{
		"tool_call_id": toolCallID,
		"input":        json.RawMessage(input),
	})
	frame := chat.Frame{
		Kind:   "tool_proposed",
		Tool:   tool,
		Result: string(resultBytes),
	}
	payload, _ := json.Marshal(rpc.Response[chat.Frame]{ID: "", Result: &frame})
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		g.d.ClearPendingConfirm(toolCallID)
		return chat.ConfirmDecision{Approve: false, DenyReason: "stream write failed: " + err.Error()}, nil
	}

	// 4. Block on the channel with timeout / ctx cancellation.
	select {
	case dec := <-ch:
		return chat.ConfirmDecision{Approve: dec.Approve, EditedInput: dec.EditedInput, DenyReason: dec.DenyReason}, nil
	case <-time.After(time.Duration(g.timeoutSeconds) * time.Second):
		g.d.ClearPendingConfirm(toolCallID)
		return chat.ConfirmDecision{Approve: false, DenyReason: "confirm timeout"}, nil
	case <-ctx.Done():
		g.d.ClearPendingConfirm(toolCallID)
		return chat.ConfirmDecision{Approve: false, DenyReason: "context cancelled"}, ctx.Err()
	}
}

// ChatConfirmParams is the params envelope for the chat.confirm RPC.
type ChatConfirmParams struct {
	SessionID   string          `json:"session_id"`
	ToolCallID  string          `json:"tool_call_id"`
	Approve     bool            `json:"approve"`
	EditedInput json.RawMessage `json:"edited_input,omitempty"`
	Reason      string          `json:"reason,omitempty"`
}

// handleChatConfirm resolves a pending confirm by tool_call_id.
func (s *RPCServer) handleChatConfirm(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p ChatConfirmParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ToolCallID == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "tool_call_id required"}
	}
	if len(p.EditedInput) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(p.EditedInput, &probe); err != nil || probe == nil {
			return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "edited_input must be a JSON object"}
		}
	}
	s.d.ResolveConfirm(p.ToolCallID, confirmDecision{
		Approve:     p.Approve,
		EditedInput: p.EditedInput,
		DenyReason:  p.Reason,
	})
	out, _ := json.Marshal(map[string]any{"ok": true})
	return out, nil
}
