package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// stubGate implements ConfirmGate with a controllable decision delivered on a
// channel. Production uses daemonConfirmGate (pending channel + chat.confirm
// RPC); this stub exercises the same Propose interface contract.
type stubGate struct {
	mu       sync.Mutex
	decision ConfirmDecision
	calls    int
	delay    time.Duration
}

func (g *stubGate) Propose(ctx context.Context, sessionID, toolCallID, tool string, input json.RawMessage) (ConfirmDecision, error) {
	g.mu.Lock()
	g.calls++
	d := g.delay
	dec := g.decision
	g.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ConfirmDecision{Approve: false, DenyReason: "ctx"}, ctx.Err()
		}
	}
	return dec, nil
}

func TestAgentMutatingToolDeniedThenLoopContinues(t *testing.T) {
	handlerRan := false
	registry := NewRegistry()
	registry.Register(Tool{
		Def:      anthropic.ToolDef{Name: "mutate_thing", Description: "", InputSchema: map[string]any{"type": "object"}},
		Mutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) ToolResult {
			handlerRan = true
			return ToolResult{Content: `{"ok":true}`}
		},
	})

	runner := &fakeRunner{queue: []*anthropic.TurnOutput{
		{
			StopReason: "tool_use",
			Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("calling")),
			ToolCalls:  []anthropic.ToolCall{{ID: "tu_1", Name: "mutate_thing", Input: json.RawMessage(`{"x":1}`)}},
		},
		{StopReason: "end_turn", Text: "ok, skipping it", Assistant: anth.NewAssistantMessage(anth.NewTextBlock("ok, skipping it"))},
	}}

	router := NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "m", "m")
	agent := NewSDKAgent(runner, registry, router, Config{DefaultModel: "m", MaxIters: 4}, nil)
	gate := &stubGate{decision: ConfirmDecision{Approve: false, DenyReason: "user said no"}}
	agent.SetConfirmGate(gate)

	frames := []Frame{}
	if err := agent.Send(context.Background(), &Conversation{SessionID: "sess-1"}, "do it", func(f Frame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if handlerRan {
		t.Errorf("Handler ran despite deny")
	}
	if gate.calls != 1 {
		t.Errorf("gate.Propose calls=%d, want 1", gate.calls)
	}
	if runner.calls != 2 {
		t.Errorf("runner.RunTurn calls=%d, want 2 (tool_use turn + post-deny turn)", runner.calls)
	}
	var sawDeny, sawFinalText bool
	for _, f := range frames {
		if f.Kind == "tool_result" && strings.Contains(f.Result, "user said no") {
			sawDeny = true
		}
		if f.Kind == "text" && strings.Contains(f.Text, "skipping") {
			sawFinalText = true
		}
	}
	if !sawDeny {
		t.Errorf("no synthetic deny tool_result frame emitted; frames=%+v", frames)
	}
	if !sawFinalText {
		t.Errorf("model's post-deny final text missing; frames=%+v", frames)
	}
}

func TestAgentMutatingToolApprovedRunsHandler(t *testing.T) {
	var got json.RawMessage
	registry := NewRegistry()
	registry.Register(Tool{
		Def:      anthropic.ToolDef{Name: "mutate_thing", Description: "", InputSchema: map[string]any{"type": "object"}},
		Mutating: true,
		Handler: func(_ context.Context, in json.RawMessage) ToolResult {
			got = in
			return ToolResult{Content: `{"ok":true}`}
		},
	})

	runner := &fakeRunner{queue: []*anthropic.TurnOutput{
		{
			StopReason: "tool_use",
			Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("calling")),
			ToolCalls:  []anthropic.ToolCall{{ID: "tu_1", Name: "mutate_thing", Input: json.RawMessage(`{"x":1}`)}},
		},
		{StopReason: "end_turn", Text: "done", Assistant: anth.NewAssistantMessage(anth.NewTextBlock("done"))},
	}}

	agent := NewSDKAgent(runner, registry, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "s"}}, "m", "m"), Config{DefaultModel: "m", MaxIters: 4}, nil)
	agent.SetConfirmGate(&stubGate{decision: ConfirmDecision{Approve: true}})

	if err := agent.Send(context.Background(), &Conversation{SessionID: "s"}, "go", func(Frame) {}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(got) != `{"x":1}` {
		t.Errorf("Handler got input %q, want %q", string(got), `{"x":1}`)
	}
}

func TestAgentMutatingToolEditedInputForwardedToHandler(t *testing.T) {
	var got json.RawMessage
	registry := NewRegistry()
	registry.Register(Tool{
		Def:      anthropic.ToolDef{Name: "mutate_thing", Description: "", InputSchema: map[string]any{"type": "object"}},
		Mutating: true,
		Handler: func(_ context.Context, in json.RawMessage) ToolResult {
			got = in
			return ToolResult{Content: `{"ok":true}`}
		},
	})

	runner := &fakeRunner{queue: []*anthropic.TurnOutput{
		{
			StopReason: "tool_use",
			Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("calling")),
			ToolCalls:  []anthropic.ToolCall{{ID: "tu_1", Name: "mutate_thing", Input: json.RawMessage(`{"x":1}`)}},
		},
		{StopReason: "end_turn", Text: "done", Assistant: anth.NewAssistantMessage(anth.NewTextBlock("done"))},
	}}

	agent := NewSDKAgent(runner, registry, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "s"}}, "m", "m"), Config{DefaultModel: "m", MaxIters: 4}, nil)
	agent.SetConfirmGate(&stubGate{decision: ConfirmDecision{
		Approve:     true,
		EditedInput: json.RawMessage(`{"x":99,"note":"edited"}`),
	}})

	if err := agent.Send(context.Background(), &Conversation{SessionID: "s"}, "go", func(Frame) {}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(string(got), `"x":99`) || !strings.Contains(string(got), `"edited"`) {
		t.Errorf("Handler got input %q, want edited args present", string(got))
	}
}
