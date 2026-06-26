package chat

import (
	"context"
	"strings"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
)

// Router picks the model for a turn: a quick classify promotes complex /
// multi-step queries to the reasoning model, else the default model.
//
// ForceSonnet, when true, skips the classify call entirely and always
// returns the reasoning model (used by planner-mode chat sessions where
// every Q&A turn benefits from depth and the classifier-driven economy
// is the wrong default).
type Router struct {
	runner         turnRunner
	defaultModel   string
	reasoningModel string
	ForceSonnet    bool
}

// NewRouter constructs a Router. The runner is used for the cheap classify
// turn; it is independent of the agent's runner (so classify never consumes
// the agent's scripted turns in tests, and can in principle use a cheaper
// model in production).
func NewRouter(runner turnRunner, defaultModel, reasoningModel string) *Router {
	return &Router{runner: runner, defaultModel: defaultModel, reasoningModel: reasoningModel}
}

const routerClassifyPrompt = `Reply with exactly 'complex' or 'simple': is this request multi-step / requires reasoning over multiple data points, or a simple lookup?`

// ClassifyUsage holds the token counts produced by the router's classify call.
// The caller should pass these to its costFn (with the classify model, not the
// routed model) and add the result to conv.CostUSD before running the actual
// turn.
type ClassifyUsage struct {
	Model     string
	TokensIn  int64
	TokensOut int64
}

// Route returns the model to use for this user message and the token usage
// incurred by the classify call. On ANY error it falls back to defaultModel
// and returns a zero ClassifyUsage; it never blocks the turn.
func (rt *Router) Route(ctx context.Context, message string) (string, ClassifyUsage, error) {
	// Planner-mode short-circuit: skip classify, always pick reasoning model.
	// No classify call is made, so ClassifyUsage is zero — nothing to bill.
	if rt.ForceSonnet {
		return rt.reasoningModel, ClassifyUsage{}, nil
	}
	out, err := rt.runner.RunTurn(ctx, anthropic.TurnInput{
		Model:     rt.defaultModel,
		System:    routerClassifyPrompt,
		Messages:  []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock(message))},
		MaxTokens: 16,
	})
	if err != nil || out == nil {
		return rt.defaultModel, ClassifyUsage{}, err
	}
	usage := ClassifyUsage{
		Model:     rt.defaultModel,
		TokensIn:  out.TokensIn,
		TokensOut: out.TokensOut,
	}
	if strings.Contains(strings.ToLower(out.Text), "complex") {
		return rt.reasoningModel, usage, nil
	}
	return rt.defaultModel, usage, nil
}
