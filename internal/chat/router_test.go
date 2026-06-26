package chat

import (
	"context"
	"testing"

	"github.com/rohilrs/Hive/internal/anthropic"
)

func TestRouterComplexVsSimple(t *testing.T) {
	tests := []struct {
		name     string
		out      *anthropic.TurnOutput
		err      bool
		wantUsed string
	}{
		{name: "complex", out: &anthropic.TurnOutput{Text: "complex"}, wantUsed: "reasoning-model"},
		{name: "simple", out: &anthropic.TurnOutput{Text: "simple"}, wantUsed: "default-model"},
		{name: "complex with surrounding text", out: &anthropic.TurnOutput{Text: "This is complex."}, wantUsed: "reasoning-model"},
		{name: "error falls back", err: true, wantUsed: "default-model"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			if tc.err {
				runner.lastErr = errBoom
			} else {
				runner.always = tc.out
			}
			rt := NewRouter(runner, "default-model", "reasoning-model")
			got, _, _ := rt.Route(context.Background(), "do a thing")
			if got != tc.wantUsed {
				t.Errorf("Route = %q, want %q", got, tc.wantUsed)
			}
		})
	}
}

func TestRouterReturnsClassifyUsage(t *testing.T) {
	runner := &fakeRunner{
		always: &anthropic.TurnOutput{Text: "simple", TokensIn: 42, TokensOut: 3},
	}
	rt := NewRouter(runner, "default-model", "reasoning-model")
	model, usage, err := rt.Route(context.Background(), "what is 2+2?")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if model != "default-model" {
		t.Errorf("model = %q, want default-model", model)
	}
	if usage.Model != "default-model" {
		t.Errorf("usage.Model = %q, want default-model", usage.Model)
	}
	if usage.TokensIn != 42 {
		t.Errorf("usage.TokensIn = %d, want 42", usage.TokensIn)
	}
	if usage.TokensOut != 3 {
		t.Errorf("usage.TokensOut = %d, want 3", usage.TokensOut)
	}
}

func TestRouterUsageZeroOnError(t *testing.T) {
	runner := &fakeRunner{lastErr: errBoom}
	rt := NewRouter(runner, "default-model", "reasoning-model")
	model, usage, _ := rt.Route(context.Background(), "anything")
	if model != "default-model" {
		t.Errorf("model = %q, want default-model", model)
	}
	if usage.TokensIn != 0 || usage.TokensOut != 0 {
		t.Errorf("expected zero usage on error, got %+v", usage)
	}
}

// TestRouterForceSonnetSkipsClassify pins planner-mode behavior: when
// ForceSonnet is on, Route returns the reasoning model without consulting
// the classifier (so even a "simple"-classified query goes to Sonnet) and
// reports zero classify usage (no classify call billed).
func TestRouterForceSonnetSkipsClassify(t *testing.T) {
	// A runner that would classify as "simple" if asked. With ForceSonnet
	// on, this runner must not be invoked.
	runner := &fakeRunner{always: &anthropic.TurnOutput{Text: "simple", TokensIn: 99, TokensOut: 99}}
	rt := NewRouter(runner, "default-model", "reasoning-model")
	rt.ForceSonnet = true

	model, usage, err := rt.Route(context.Background(), "any query at all")
	if err != nil {
		t.Fatalf("Route returned error: %v", err)
	}
	if model != "reasoning-model" {
		t.Errorf("model = %q, want reasoning-model", model)
	}
	if usage.TokensIn != 0 || usage.TokensOut != 0 || usage.Model != "" {
		t.Errorf("expected zero ClassifyUsage with ForceSonnet, got %+v", usage)
	}
	if runner.calls != 0 {
		t.Errorf("classifier runner was invoked %d times with ForceSonnet on; want 0", runner.calls)
	}
}

// TestRouterForceSonnetDefaultFalse pins that ForceSonnet defaults to off
// and the existing classify path runs unchanged.
func TestRouterForceSonnetDefaultFalse(t *testing.T) {
	runner := &fakeRunner{always: &anthropic.TurnOutput{Text: "simple", TokensIn: 5, TokensOut: 2}}
	rt := NewRouter(runner, "default-model", "reasoning-model")
	if rt.ForceSonnet {
		t.Fatalf("ForceSonnet should default to false")
	}
	model, usage, _ := rt.Route(context.Background(), "anything")
	if model != "default-model" {
		t.Errorf("model = %q, want default-model (classifier-driven)", model)
	}
	if usage.TokensIn != 5 || usage.TokensOut != 2 {
		t.Errorf("expected classifier usage to flow through, got %+v", usage)
	}
}
