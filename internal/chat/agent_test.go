package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// fakeRunner is a turnRunner backed by a scripted queue of outputs. Each
// RunTurn call pops the next output; if the queue is empty it returns the
// last output repeatedly (so always-tool-use tests can rely on it).
type fakeRunner struct {
	queue   []*anthropic.TurnOutput
	always  *anthropic.TurnOutput // returned when queue empty (nil => error)
	calls   int
	lastErr error
}

func (f *fakeRunner) RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error) {
	f.calls++
	if f.lastErr != nil {
		return nil, f.lastErr
	}
	if len(f.queue) > 0 {
		out := f.queue[0]
		f.queue = f.queue[1:]
		return out, nil
	}
	if f.always != nil {
		return f.always, nil
	}
	// Default terminal turn.
	return &anthropic.TurnOutput{StopReason: "end_turn", Assistant: anth.NewAssistantMessage(anth.NewTextBlock(""))}, nil
}

func zeroCost(model string, in, out int64) float64 { return 0 }

// perTokenCost returns a simple cost function that charges $1/Mtok in + $1/Mtok out
// for every model, making assertions on accumulated CostUSD easy.
func perTokenCost(model string, in, out int64) float64 {
	return float64(in+out) / 1_000_000.0
}

func TestAgentToolUseLoop(t *testing.T) {
	runner := &fakeRunner{
		queue: []*anthropic.TurnOutput{
			{
				Text:       "let me check",
				ToolCalls:  []anthropic.ToolCall{{ID: "t1", Name: "hive_status", Input: json.RawMessage(`{}`)}},
				StopReason: "tool_use",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("let me check")),
			},
			{
				Text:       "all good",
				StopReason: "end_turn",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("all good")),
			},
		},
	}

	handlerCalls := 0
	reg := NewRegistry()
	reg.Register(Tool{
		Def: anthropic.ToolDef{Name: "hive_status"},
		Handler: func(ctx context.Context, input json.RawMessage) ToolResult {
			handlerCalls++
			return ToolResult{Content: `{"ok":true}`}
		},
	})

	// Router runner is separate so it does not consume the agent runner's queue.
	router := NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "default-model", "reasoning-model")
	agent := NewSDKAgent(runner, reg, router, Config{DefaultModel: "default-model", ReasoningModel: "reasoning-model"}, zeroCost)

	var frames []Frame
	conv := &Conversation{}
	if err := agent.Send(context.Background(), conv, "what's the status?", func(f Frame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if handlerCalls != 1 {
		t.Fatalf("handler calls = %d, want 1", handlerCalls)
	}

	wantKinds := []string{"text", "tool_result", "text", "turn_done"}
	if len(frames) != len(wantKinds) {
		t.Fatalf("frames = %d, want %d: %+v", len(frames), len(wantKinds), frames)
	}
	for i, want := range wantKinds {
		if frames[i].Kind != want {
			t.Fatalf("frame[%d].Kind = %q, want %q", i, frames[i].Kind, want)
		}
	}
	if frames[0].Text != "let me check" {
		t.Errorf("frame[0].Text = %q", frames[0].Text)
	}
	if frames[1].Tool != "hive_status" || frames[1].Result != `{"ok":true}` {
		t.Errorf("frame[1] = %+v", frames[1])
	}
	if frames[2].Text != "all good" {
		t.Errorf("frame[2].Text = %q", frames[2].Text)
	}
	if frames[3].Model != "default-model" {
		t.Errorf("frame[3].Model = %q, want default-model", frames[3].Model)
	}

	// Conversation should contain: user msg, assistant1, tool_result user msg, assistant2.
	if len(conv.Messages) != 4 {
		t.Errorf("conv.Messages = %d, want 4", len(conv.Messages))
	}
}

func TestAgentMaxIters(t *testing.T) {
	// Always wants a tool; loop must terminate at MaxIters.
	runner := &fakeRunner{
		always: &anthropic.TurnOutput{
			Text:       "thinking",
			ToolCalls:  []anthropic.ToolCall{{ID: "t1", Name: "hive_status", Input: json.RawMessage(`{}`)}},
			StopReason: "tool_use",
			Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("thinking")),
		},
	}
	reg := NewRegistry()
	reg.Register(Tool{
		Def:     anthropic.ToolDef{Name: "hive_status"},
		Handler: func(ctx context.Context, input json.RawMessage) ToolResult { return ToolResult{Content: `{}`} },
	})
	router := NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "default-model", "reasoning-model")
	agent := NewSDKAgent(runner, reg, router, Config{DefaultModel: "default-model", ReasoningModel: "reasoning-model", MaxIters: 3}, zeroCost)

	var sawDone bool
	conv := &Conversation{}
	if err := agent.Send(context.Background(), conv, "go", func(f Frame) {
		if f.Kind == "turn_done" {
			sawDone = true
		}
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if runner.calls != 3 {
		t.Errorf("RunTurn calls = %d, want 3", runner.calls)
	}
	if !sawDone {
		t.Error("expected turn_done frame after MaxIters")
	}
}

func TestAgentUnknownTool(t *testing.T) {
	runner := &fakeRunner{
		queue: []*anthropic.TurnOutput{
			{
				ToolCalls:  []anthropic.ToolCall{{ID: "t1", Name: "does_not_exist", Input: json.RawMessage(`{}`)}},
				StopReason: "tool_use",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("")),
			},
			{
				Text:       "ok",
				StopReason: "end_turn",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("ok")),
			},
		},
	}
	reg := NewRegistry() // empty
	router := NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "default-model", "reasoning-model")
	agent := NewSDKAgent(runner, reg, router, Config{DefaultModel: "default-model", ReasoningModel: "reasoning-model"}, zeroCost)

	var toolResult *Frame
	var sawDone bool
	conv := &Conversation{}
	if err := agent.Send(context.Background(), conv, "go", func(f Frame) {
		switch f.Kind {
		case "tool_result":
			ff := f
			toolResult = &ff
		case "turn_done":
			sawDone = true
		}
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if toolResult == nil {
		t.Fatal("expected a tool_result frame for the unknown tool")
	}
	if toolResult.Tool != "does_not_exist" {
		t.Errorf("tool_result.Tool = %q", toolResult.Tool)
	}
	if toolResult.Result == "" || toolResult.Result == `{"ok":true}` {
		t.Errorf("expected error content, got %q", toolResult.Result)
	}
	if !sawDone {
		t.Error("loop should terminate gracefully with turn_done")
	}
}

func TestAgentRunnerError(t *testing.T) {
	runner := &fakeRunner{}
	// Set error via a wrapper: easiest is a dedicated field.
	runner.queue = nil
	runner.always = nil
	runner.lastErr = errBoom

	reg := NewRegistry()
	router := NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "default-model", "reasoning-model")
	agent := NewSDKAgent(runner, reg, router, Config{DefaultModel: "default-model", ReasoningModel: "reasoning-model"}, zeroCost)

	var sawError bool
	conv := &Conversation{}
	err := agent.Send(context.Background(), conv, "go", func(f Frame) {
		if f.Kind == "error" {
			sawError = true
		}
	})
	if err == nil {
		t.Fatal("expected error from Send")
	}
	if !sawError {
		t.Error("expected an error frame")
	}
}

// TestAgentClassifyCostFoldedIntoCostUSD verifies that the classify call's
// token usage (returned by router.Route) is added to conv.CostUSD before the
// routed turn runs, so the per-turn cost figure isn't understated.
func TestAgentClassifyCostFoldedIntoCostUSD(t *testing.T) {
	// Router runner returns a "simple" response with known token counts.
	// classify: TokensIn=100, TokensOut=5 → perTokenCost = 105/1e6
	routerRunner := &fakeRunner{
		always: &anthropic.TurnOutput{Text: "simple", TokensIn: 100, TokensOut: 5},
	}
	// Agent runner returns a single terminal turn with known token counts.
	// turn: TokensIn=200, TokensOut=10 → perTokenCost = 210/1e6
	agentRunner := &fakeRunner{
		always: &anthropic.TurnOutput{
			Text:       "done",
			StopReason: "end_turn",
			Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("done")),
			TokensIn:   200,
			TokensOut:  10,
		},
	}

	router := NewRouter(routerRunner, "default-model", "reasoning-model")
	agent := NewSDKAgent(agentRunner, NewRegistry(), router,
		Config{DefaultModel: "default-model", ReasoningModel: "reasoning-model"},
		perTokenCost,
	)

	conv := &Conversation{}
	if err := agent.Send(context.Background(), conv, "hello", func(Frame) {}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// classify: (100+5)/1e6 = 0.000105
	// turn:     (200+10)/1e6 = 0.000210
	// total:    0.000315
	const wantCost = float64(100+5+200+10) / 1_000_000.0
	if conv.CostUSD != wantCost {
		t.Errorf("conv.CostUSD = %g, want %g (classify+turn costs not summed correctly)", conv.CostUSD, wantCost)
	}
}

var errBoom = boomError("boom")

type boomError string

func (e boomError) Error() string { return string(e) }

func TestSDKAgentMutatingToolDeniedByGate(t *testing.T) {
	registry := NewRegistry()
	registry.Register(Tool{
		Def:      anthropic.ToolDef{Name: "hive_add_task", Description: "add a task", InputSchema: map[string]any{"type": "object"}},
		Mutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) ToolResult {
			t.Fatal("Handler should NOT run when gate denies")
			return ToolResult{}
		},
	})

	// Turn 1: tool_use; Turn 2: final text after deny feedback.
	runner := &fakeRunner{
		queue: []*anthropic.TurnOutput{
			{
				StopReason: "tool_use",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("calling add_task")),
				ToolCalls: []anthropic.ToolCall{
					{ID: "tu_1", Name: "hive_add_task", Input: json.RawMessage(`{"title":"t"}`)},
				},
			},
			{
				StopReason: "end_turn",
				Text:       "okay, won't add it then",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("okay, won't add it then")),
			},
		},
	}

	agent := NewSDKAgent(runner, registry, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "m", "m"), Config{DefaultModel: "m", MaxIters: 4, SystemPrefix: ""}, nil)
	agent.SetConfirmGate(DenyAllConfirmGate{Reason: "policy"})

	var frames []Frame
	conv := &Conversation{SessionID: "sess-1"}
	if err := agent.Send(context.Background(), conv, "add a task called t", func(f Frame) { frames = append(frames, f) }); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var sawDeny bool
	for _, f := range frames {
		if f.Kind == "tool_result" && f.Tool == "hive_add_task" {
			if !strings.Contains(f.Result, "policy") {
				t.Errorf("expected synthetic deny result containing 'policy', got: %s", f.Result)
			}
			sawDeny = true
		}
	}
	if !sawDeny {
		t.Errorf("no tool_result frame emitted; frames=%+v", frames)
	}
	if runner.calls != 2 {
		t.Errorf("expected 2 runner turns (tool_use + end_turn), got %d", runner.calls)
	}
}

func TestSDKAgentMutatingToolInAutoConfirmSkipsGate(t *testing.T) {
	ran := false
	registry := NewRegistry()
	registry.Register(Tool{
		Def:      anthropic.ToolDef{Name: "hive_add_task", Description: "add a task", InputSchema: map[string]any{"type": "object"}},
		Mutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) ToolResult {
			ran = true
			return ToolResult{Content: `{"ok":true}`}
		},
	})

	runner := &fakeRunner{
		queue: []*anthropic.TurnOutput{
			{
				StopReason: "tool_use",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("calling")),
				ToolCalls:  []anthropic.ToolCall{{ID: "tu_1", Name: "hive_add_task", Input: json.RawMessage(`{}`)}},
			},
			{StopReason: "end_turn", Text: "done", Assistant: anth.NewAssistantMessage(anth.NewTextBlock("done"))},
		},
	}

	agent := NewSDKAgent(runner, registry, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "m", "m"), Config{DefaultModel: "m", MaxIters: 4}, nil)
	// gate would deny but auto-confirm should bypass it
	agent.SetConfirmGate(DenyAllConfirmGate{Reason: "should not run"})
	agent.SetAutoConfirm([]string{"hive_add_task"})

	if err := agent.Send(context.Background(), &Conversation{SessionID: "s"}, "go", func(Frame) {}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !ran {
		t.Errorf("Handler did NOT run despite auto-confirm")
	}
}

// newSingleToolCallSDK returns a fakeRunner scripted to emit one tool_use turn
// calling toolName with input `{}`, followed by a terminal end_turn. This
// provides a minimal SDK harness for gate/confirm-path tests that only need
// to inspect what happens to a single tool call.
func newSingleToolCallSDK(t *testing.T, toolName string) *fakeRunner {
	t.Helper()
	return &fakeRunner{
		queue: []*anthropic.TurnOutput{
			{
				StopReason: "tool_use",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("")),
				ToolCalls:  []anthropic.ToolCall{{ID: "tc_1", Name: toolName, Input: json.RawMessage(`{}`)}},
			},
			{
				StopReason: "end_turn",
				Text:       "done",
				Assistant:  anth.NewAssistantMessage(anth.NewTextBlock("done")),
			},
		},
	}
}

func TestSDKAgentDenyContentUsesDenyReasonWhenSet(t *testing.T) {
	// When the gate denies with a DenyReason set, the synthesized
	// tool_result content should be {"error":"<reason>"} — NOT
	// {"error":"declined by user","reason":"<reason>"}. The reshape
	// matters because the `c` keybind sets DenyReason to
	// "user cancelled, do not retry" and we want the model to read
	// that as the primary error message.
	reg := NewRegistry()
	reg.Register(Tool{
		Def:      anthropic.ToolDef{Name: "hive_add_task"},
		Mutating: true,
		Handler:  func(_ context.Context, _ json.RawMessage) ToolResult { return ToolResult{} },
	})
	gate := &stubGate{decision: ConfirmDecision{Approve: false, DenyReason: "user cancelled, do not retry"}}
	sdk := newSingleToolCallSDK(t, "hive_add_task")
	agent := NewSDKAgent(sdk, reg, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "haiku", "haiku"), Config{DefaultModel: "haiku", MaxIters: 1}, perTokenCost)
	agent.SetConfirmGate(gate)

	var got string
	agent.Send(context.Background(), &Conversation{SessionID: "s1"}, "go", func(f Frame) {
		if f.Kind == "tool_result" {
			got = f.Result
		}
	})

	var body map[string]any
	if err := json.Unmarshal([]byte(got), &body); err != nil {
		t.Fatalf("tool_result not valid JSON: %s err=%v", got, err)
	}
	if body["error"] != "user cancelled, do not retry" {
		t.Errorf("error=%v, want 'user cancelled, do not retry'", body["error"])
	}
	if _, hasReason := body["reason"]; hasReason {
		t.Errorf("tool_result should not carry a separate 'reason' field anymore; got %s", got)
	}
}

func TestSDKAgentDenyContentDefaultsToDeclinedByUser(t *testing.T) {
	// When DenyReason is empty, fall back to the legacy default so
	// today's deny ('n' keybind) still reads as "declined by user".
	reg := NewRegistry()
	reg.Register(Tool{
		Def:      anthropic.ToolDef{Name: "hive_add_task"},
		Mutating: true,
		Handler:  func(_ context.Context, _ json.RawMessage) ToolResult { return ToolResult{} },
	})
	gate := &stubGate{decision: ConfirmDecision{Approve: false}}
	sdk := newSingleToolCallSDK(t, "hive_add_task")
	agent := NewSDKAgent(sdk, reg, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "haiku", "haiku"), Config{DefaultModel: "haiku", MaxIters: 1}, perTokenCost)
	agent.SetConfirmGate(gate)

	var got string
	agent.Send(context.Background(), &Conversation{SessionID: "s1"}, "go", func(f Frame) {
		if f.Kind == "tool_result" {
			got = f.Result
		}
	})

	var body map[string]any
	_ = json.Unmarshal([]byte(got), &body)
	if body["error"] != "declined by user" {
		t.Errorf("error=%v, want 'declined by user'", body["error"])
	}
}

func TestSDKAgentEditedInputResultGetsPrefix(t *testing.T) {
	// When the gate approves with EditedInput, the tool runs with
	// the edited args AND the resulting tool_result content carries
	// the [user edited args before running] prefix so the model's
	// conversation history reflects what actually executed.
	var seenInput json.RawMessage
	reg := NewRegistry()
	reg.Register(Tool{
		Def:      anthropic.ToolDef{Name: "hive_add_task"},
		Mutating: true,
		Handler: func(_ context.Context, in json.RawMessage) ToolResult {
			seenInput = in
			return ToolResult{Content: `{"task_id":"t-1"}`}
		},
	})
	gate := &stubGate{decision: ConfirmDecision{
		Approve:     true,
		EditedInput: json.RawMessage(`{"title":"edited"}`),
	}}
	sdk := newSingleToolCallSDK(t, "hive_add_task")
	agent := NewSDKAgent(sdk, reg, NewRouter(&fakeRunner{always: &anthropic.TurnOutput{Text: "simple"}}, "haiku", "haiku"), Config{DefaultModel: "haiku", MaxIters: 1}, perTokenCost)
	agent.SetConfirmGate(gate)

	var got string
	agent.Send(context.Background(), &Conversation{SessionID: "s1"}, "go", func(f Frame) {
		if f.Kind == "tool_result" {
			got = f.Result
		}
	})

	if string(seenInput) != `{"title":"edited"}` {
		t.Errorf("handler received %s, want edited input", seenInput)
	}
	wantPrefix := "[user edited args before running] "
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("tool_result missing edit prefix: %q", got)
	}
	if !strings.Contains(got, `"task_id":"t-1"`) {
		t.Errorf("tool_result missing original handler output: %q", got)
	}
}
