// Package pricing maps Anthropic model names to USD-per-million-token
// prices. Values are hardcoded (Anthropic updates pricing infrequently
// enough that committing them is fine for v1; revisit if it becomes
// painful). Used by the pipeline to populate stages.cost_usd.
//
// Unknown models return ok=false from Lookup; callers should record
// NULL in cost_usd rather than guessing.
package pricing

import "strings"

// Model is the per-model price triple (in USD per million tokens).
type Model struct {
	InputPerMtok     float64
	OutputPerMtok    float64
	CacheReadPerMtok float64
}

// Current Anthropic model pricing as of 2026-05-23. Source:
// anthropic.com/pricing. Update when prices change; cost_usd in old
// stages rows reflects the price at-recording-time (cost is computed
// at stage end, not lazily on read).
var table = map[string]Model{
	"claude-haiku-4-5":  {InputPerMtok: 1.00, OutputPerMtok: 5.00, CacheReadPerMtok: 0.10},
	"claude-sonnet-4-6": {InputPerMtok: 3.00, OutputPerMtok: 15.00, CacheReadPerMtok: 0.30},
	"claude-opus-4-7":   {InputPerMtok: 15.00, OutputPerMtok: 75.00, CacheReadPerMtok: 1.50},
	"claude-opus-4-8":   {InputPerMtok: 15.00, OutputPerMtok: 75.00, CacheReadPerMtok: 1.50},
}

// Lookup returns the price triple for model. ok=false for unknown
// models; callers should record cost_usd as NULL in that case.
//
// A trailing context-window marker (e.g. the "[1m]" in
// "claude-opus-4-8[1m]") is stripped before lookup — the long-context
// variant is the same model at the same per-token price.
func Lookup(model string) (Model, bool) {
	if i := strings.IndexByte(model, '['); i >= 0 {
		model = model[:i]
	}
	m, ok := table[model]
	return m, ok
}

// Cost returns the USD cost of (tokensIn, tokensOut, cacheHit) tokens
// at model m's prices. Linear formula; no minimum charges.
func Cost(tokensIn, tokensOut, cacheHit int, m Model) float64 {
	mtokIn := float64(tokensIn) / 1_000_000.0
	mtokOut := float64(tokensOut) / 1_000_000.0
	mtokCache := float64(cacheHit) / 1_000_000.0
	return mtokIn*m.InputPerMtok + mtokOut*m.OutputPerMtok + mtokCache*m.CacheReadPerMtok
}
