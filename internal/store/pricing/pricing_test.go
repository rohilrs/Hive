package pricing

import (
	"math"
	"testing"
)

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestLookupKnownModel(t *testing.T) {
	m, ok := Lookup("claude-sonnet-4-6")
	if !ok {
		t.Fatal("expected sonnet to be known")
	}
	if m.InputPerMtok != 3.00 || m.OutputPerMtok != 15.00 {
		t.Errorf("sonnet pricing wrong: %+v", m)
	}
}

func TestLookupUnknownModel(t *testing.T) {
	if _, ok := Lookup("claude-future-99"); ok {
		t.Error("expected unknown to be !ok")
	}
}

func TestLookupOpus48AndContextWindowMarker(t *testing.T) {
	base, ok := Lookup("claude-opus-4-8")
	if !ok {
		t.Fatal("expected claude-opus-4-8 to be known")
	}
	if base.InputPerMtok != 15.00 || base.OutputPerMtok != 75.00 {
		t.Errorf("opus-4-8 pricing wrong: %+v", base)
	}
	// The long-context variant must resolve to the same price.
	marked, ok := Lookup("claude-opus-4-8[1m]")
	if !ok {
		t.Fatal("expected claude-opus-4-8[1m] to resolve via suffix strip")
	}
	if marked != base {
		t.Errorf("[1m] variant priced differently: %+v vs %+v", marked, base)
	}
}

func TestCostBasic(t *testing.T) {
	// 1M input @ $3 + 1M output @ $15 = $18 for sonnet.
	m, _ := Lookup("claude-sonnet-4-6")
	got := Cost(1_000_000, 1_000_000, 0, m)
	if !approxEqual(got, 18.0) {
		t.Errorf("cost = %v want 18.0", got)
	}
}

func TestCostWithCacheHit(t *testing.T) {
	// Sonnet: input $3/Mtok, output $15/Mtok, cache $0.30/Mtok.
	// 500k input + 200k output + 100k cache hit = 0.5*3 + 0.2*15 + 0.1*0.30
	// = 1.50 + 3.00 + 0.03 = 4.53.
	m, _ := Lookup("claude-sonnet-4-6")
	got := Cost(500_000, 200_000, 100_000, m)
	want := 0.5*3.0 + 0.2*15.0 + 0.1*0.30
	if !approxEqual(got, want) {
		t.Errorf("cost = %v want %v", got, want)
	}
}

func TestCostZeroTokens(t *testing.T) {
	m, _ := Lookup("claude-haiku-4-5")
	if Cost(0, 0, 0, m) != 0.0 {
		t.Error("zero tokens should produce zero cost")
	}
}

func TestKnownModelsCoverage(t *testing.T) {
	// All three currently-in-use models must be in the registry.
	for _, name := range []string{"claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-7"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("missing pricing for %s", name)
		}
	}
}
