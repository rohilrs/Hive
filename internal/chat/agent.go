package chat

import (
	"context"
	"encoding/json"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// turnRunner is the seam over *anthropic.SDK for offline testing. The agent
// and router depend on this interface, not on the concrete SDK, so tests
// inject a scripted fake.
type turnRunner interface {
	RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error)
}

// Frame is one streamed event emitted during Send.
type Frame struct {
	Kind    string // "text" | "tool_result" | "turn_done" | "error"
	Text    string
	Tool    string
	Result  string
	Model   string
	CostUSD float64
}

// Config configures the agent loop.
type Config struct {
	DefaultModel   string
	ReasoningModel string
	MaxIters       int    // safety cap on tool-use loop steps (default 8)
	SystemPrefix   string // tool/state context prefix (cached); caller supplies
}

// Conversation holds the running message history + accumulated cost for a
// session. The agent appends to Messages and accumulates CostUSD in place.
type Conversation struct {
	Messages []anth.MessageParam
	CostUSD  float64

	// SessionID is the provider-specific session/resume handle. The
	// Claude-Code provider uses it for `--resume` continuity; the SDK agent
	// ignores it (history lives in Messages).
	SessionID string
}

// Agent runs one user message through a provider-specific loop, emitting Frames.
type Agent interface {
	Send(ctx context.Context, conv *Conversation, userMsg string, emit func(Frame)) error
}

// SDKAgent runs user messages through a bounded tool-use loop against the
// Anthropic API SDK. It is the default ("api") chat provider.
type SDKAgent struct {
	runner      turnRunner
	registry    *Registry
	router      *Router
	cfg         Config
	costFn      func(model string, in, out int64) float64
	gate        ConfirmGate     // nil → NoopConfirmGate (approve all)
	autoConfirm map[string]bool // tool names that bypass the gate
}

// SDKAgent satisfies the Agent interface.
var _ Agent = (*SDKAgent)(nil)

const defaultMaxIters = 8

// NewSDKAgent constructs an SDKAgent. costFn maps token usage to USD (the
// daemon supplies it, reusing store/pricing); MaxIters defaults to 8 when <= 0.
func NewSDKAgent(runner turnRunner, registry *Registry, router *Router, cfg Config, costFn func(string, int64, int64) float64) *SDKAgent {
	if cfg.MaxIters <= 0 {
		cfg.MaxIters = defaultMaxIters
	}
	return &SDKAgent{runner: runner, registry: registry, router: router, cfg: cfg, costFn: costFn, gate: NoopConfirmGate{}}
}

// SetConfirmGate injects the gate the agent consults before Mutating tools.
// A nil gate defaults to NoopConfirmGate (approve-all), preserving the
// 6.1a behavior where mutating tools didn't exist.
func (a *SDKAgent) SetConfirmGate(g ConfirmGate) {
	if g == nil {
		g = NoopConfirmGate{}
	}
	a.gate = g
}

// SystemPrefixForTest exposes the configured system prompt prefix for
// tests that need to verify per-session prompt routing (Phase 8.A T6).
// Not for production use.
func (a *SDKAgent) SystemPrefixForTest() string { return a.cfg.SystemPrefix }

// RegistryForTest exposes the agent's registry for tests that need to
// verify which tools the agent advertises (Phase 8.A T6).
// Not for production use.
func (a *SDKAgent) RegistryForTest() *Registry { return a.registry }

// RouterForTest exposes the agent's router for tests that need to
// verify routing behavior (Phase 8.A T6). Not for production use.
func (a *SDKAgent) RouterForTest() *Router { return a.router }

// SetAutoConfirm injects the set of tool names that skip the gate.
func (a *SDKAgent) SetAutoConfirm(names []string) {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	a.autoConfirm = m
}

// Send runs ONE user message through the tool-use loop, emitting Frames via
// emit. The user message and each turn's assistant + tool-result messages are
// appended to conv.
//
// streamChat emits a "session" frame on the wire before invoking Send.
// When a Mutating tool needs confirmation, the injected ConfirmGate (in
// production: daemonConfirmGate) emits a "tool_proposed" frame on the
// stream and blocks until chat.confirm resolves it. The agent's loop
// merely calls gate.Propose — the frame emission happens inside the
// gate, not here. The client already knows the session id from the
// earlier session frame, so tool_proposed only needs tool_call_id.
//
// In 6.1a all registered tools were read-only and executed immediately.
// In 6.1b-i Mutating tools are gated through ConfirmGate (NoopConfirmGate
// by default). On deny the agent fabricates a synthetic tool_result with
// an "error" field fed back to the
// model so it can adjust. When the gate sets DenyReason, it becomes the
// "error" value; otherwise the agent uses "declined by user".
func (a *SDKAgent) Send(ctx context.Context, conv *Conversation, userMsg string, emit func(Frame)) error {
	model, classifyUsage, _ := a.router.Route(ctx, userMsg)
	if a.costFn != nil && (classifyUsage.TokensIn > 0 || classifyUsage.TokensOut > 0) {
		conv.CostUSD += a.costFn(classifyUsage.Model, classifyUsage.TokensIn, classifyUsage.TokensOut)
	}

	conv.Messages = append(conv.Messages, anth.NewUserMessage(anth.NewTextBlock(userMsg)))

	for i := 0; i < a.cfg.MaxIters; i++ {
		out, err := a.runner.RunTurn(ctx, anthropic.TurnInput{
			Model:     model,
			System:    a.cfg.SystemPrefix,
			Messages:  conv.Messages,
			Tools:     a.registry.Defs(),
			MaxTokens: 2048,
		})
		if err != nil {
			emit(Frame{Kind: "error", Text: err.Error()})
			return err
		}

		if a.costFn != nil {
			conv.CostUSD += a.costFn(model, out.TokensIn, out.TokensOut)
		}

		if out.Text != "" {
			emit(Frame{Kind: "text", Text: out.Text})
		}

		conv.Messages = append(conv.Messages, out.Assistant)

		if len(out.ToolCalls) == 0 || out.StopReason != "tool_use" {
			break
		}

		resultBlocks := make([]anth.ContentBlockParamUnion, 0, len(out.ToolCalls))
		for _, tc := range out.ToolCalls {
			var res ToolResult
			tool, ok := a.registry.Get(tc.Name)
			switch {
			case !ok:
				res = ToolResult{Content: `{"error":"unknown tool"}`, IsError: true}
			case tool.Mutating && !a.autoConfirm[tc.Name]:
				dec, gerr := a.gate.Propose(ctx, conv.SessionID, tc.ID, tc.Name, tc.Input)
				if gerr != nil {
					body, _ := json.Marshal(map[string]string{"error": "confirm: " + gerr.Error()})
					res = ToolResult{Content: string(body), IsError: true}
					break
				}
				if !dec.Approve {
					errMsg := dec.DenyReason
					if errMsg == "" {
						errMsg = "declined by user"
					}
					body, _ := json.Marshal(map[string]string{"error": errMsg})
					res = ToolResult{Content: string(body), IsError: true}
					break
				}
				input := tc.Input
				edited := dec.EditedInput != nil
				if edited {
					input = dec.EditedInput
				}
				res = tool.Handler(ctx, input)
				if edited {
					res.Content = "[user edited args before running] " + res.Content
				}
			default:
				res = tool.Handler(ctx, tc.Input)
			}
			emit(Frame{Kind: "tool_result", Tool: tc.Name, Result: res.Content})
			resultBlocks = append(resultBlocks, anth.NewToolResultBlock(tc.ID, res.Content, res.IsError))
		}
		conv.Messages = append(conv.Messages, anth.NewUserMessage(resultBlocks...))

		// If this was the last allowed iteration and the model still wants
		// tools, surface the cap so the turn doesn't end silently on a
		// tool_result with no assistant reply.
		if i == a.cfg.MaxIters-1 {
			emit(Frame{Kind: "text", Text: "(reached the tool-step limit for this turn)"})
		}
	}

	emit(Frame{Kind: "turn_done", Model: model, CostUSD: conv.CostUSD})
	return nil
}
