package chat

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rohilrs/Hive/internal/codeintel"
)

// codebase-grounding tool-result caps. Tool output flows over CC's HTTP MCP,
// which spills oversized results to a file (LLM sees only a small preview), so
// each field is bounded well under that limit (cf. readDocCapBytes).
const (
	searchSnippetCap = 200  // runes per search hit snippet
	capsuleFieldCap  = 4096 // runes per capsule field (callers/callees/body)
)

// capStr truncates s to at most max runes, returning the (possibly truncated)
// string and whether truncation occurred. Rune-safe (never splits a codepoint).
func capStr(s string, max int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]) + "…", true
}

// PlannerCodeTools bundles the read-only codebase-grounding tools. grounder may
// be nil (no project repo / grounding disabled) → the handlers return a clean
// "unavailable" payload instead of panicking.
type PlannerCodeTools struct {
	grounder *codeintel.Grounder
}

// NewPlannerCodeTools constructs the bundle. g may be nil.
func NewPlannerCodeTools(g *codeintel.Grounder) *PlannerCodeTools {
	return &PlannerCodeTools{grounder: g}
}

// SearchCode runs git grep against the project's grounding (target) branch.
func (p *PlannerCodeTools) SearchCode(ctx context.Context, input json.RawMessage) ToolResult {
	if p.grounder == nil {
		return ToolResult{Content: `{"error":"codebase search unavailable for this project"}`, IsError: true}
	}
	var args struct {
		Query string   `json:"query"`
		Globs []string `json:"globs"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return jsonErr(err)
	}
	if args.Query == "" {
		return ToolResult{Content: `{"error":"query is required"}`, IsError: true}
	}
	hits, err := p.grounder.Search(ctx, args.Query, 50, args.Globs...)
	if err != nil {
		return jsonErr(err)
	}
	for i := range hits {
		hits[i].Snippet, _ = capStr(hits[i].Snippet, searchSnippetCap)
	}
	b, _ := json.Marshal(map[string]any{"ref": p.grounder.Ref(), "count": len(hits), "hits": hits})
	return ToolResult{Content: string(b)}
}

// QueryCapsule returns scavenger callers/callees/body for a symbol. Degrades to
// available:false (not an error) when scavenger is unavailable.
func (p *PlannerCodeTools) QueryCapsule(ctx context.Context, input json.RawMessage) ToolResult {
	if p.grounder == nil {
		return ToolResult{Content: `{"error":"capsule lookup unavailable for this project"}`, IsError: true}
	}
	var args struct {
		File   string `json:"file"`
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return jsonErr(err)
	}
	if args.File == "" {
		return ToolResult{Content: `{"error":"file is required"}`, IsError: true}
	}
	caps, err := p.grounder.Capsule(ctx, args.File, args.Symbol)
	if err != nil {
		if errors.Is(err, codeintel.ErrScavengerUnavailable) {
			return ToolResult{Content: `{"available":false,"note":"scavenger capsule unavailable; use hive_search_code + hive_read_doc"}`}
		}
		return jsonErr(err)
	}
	callers, t1 := capStr(caps.Callers, capsuleFieldCap)
	callees, t2 := capStr(caps.Callees, capsuleFieldCap)
	body, t3 := capStr(caps.Body, capsuleFieldCap)
	// Target is short (one symbol). Context/Annotations are intentionally omitted
	// in v1 to keep the result focused on the blast-radius lens and within the cap.
	b, _ := json.Marshal(map[string]any{
		"available": true,
		"target":    caps.Target,
		"callers":   callers,
		"callees":   callees,
		"body":      body,
		"truncated": t1 || t2 || t3,
	})
	return ToolResult{Content: string(b)}
}
