package approval

import (
	"context"
	"strings"
)

// Engine is the interface stage executors call to authorize tool use.
type Engine interface {
	Evaluate(ctx context.Context, req ToolUseRequest) (Decision, error)
}

// Stub approves every request unconditionally. Used when approvals are
// disabled ([approvals] enabled = false) to preserve allow-all behavior.
type Stub struct{}

func NewStub() *Stub { return &Stub{} }

func (Stub) Evaluate(_ context.Context, _ ToolUseRequest) (Decision, error) {
	return Decision{Kind: DecisionApprove, Reason: "approvals disabled (allow-all)"}, nil
}

// Rule is the engine-layer rule (mirrors store.ApprovalRule; keeps the
// engine free of an internal/store import).
type Rule struct {
	ID         int64
	Scope      string // "global" | "project:<slug>" | "stage:<name>"
	ToolName   string // or "*"
	ArgMatcher string // glob over canonical arg; "" = any
	Decision   string // "allow" | "deny"
}

// RuleStore is the engine's view onto persisted rules. The daemon
// supplies a *store.Store-backed implementation.
type RuleStore interface {
	ListApprovalRules(ctx context.Context) ([]Rule, error)
}

// RealEngine evaluates persisted rules + config-seeded defaults,
// fail-closed: deny on rule-store error and on no matching allow rule.
type RealEngine struct {
	rules    RuleStore
	defaults []Rule
}

// NewRealEngine builds the engine. defaults are seeded from config at
// daemon startup (per-stage allow-lists).
func NewRealEngine(rules RuleStore, defaults []Rule) *RealEngine {
	return &RealEngine{rules: rules, defaults: defaults}
}

func (e *RealEngine) Evaluate(ctx context.Context, req ToolUseRequest) (Decision, error) {
	// Orchestration-internal MCP tools (the verdict tool, scavenger, the
	// permission tool itself) are never gated — they're not worker shell
	// actions, and gating the verdict tool would break the pipeline.
	if strings.HasPrefix(req.ToolName, "mcp__") {
		return Decision{Kind: DecisionApprove, Reason: "mcp tool (orchestration-internal)", RuleID: "mcp_allow"}, nil
	}
	persisted, err := e.rules.ListApprovalRules(ctx)
	if err != nil {
		return Decision{Kind: DecisionDeny, Reason: "rule store error: " + err.Error(), RuleID: "fail_closed"}, nil
	}
	all := append(append([]Rule{}, e.defaults...), persisted...)
	arg := canonicalArg(req)

	// 1. explicit deny wins.
	for _, r := range all {
		if r.Decision == "deny" && ruleMatches(r, req, arg) {
			return Decision{Kind: DecisionDeny, Reason: "matched deny rule", RuleID: ruleID(r)}, nil
		}
	}
	// 2. explicit allow.
	for _, r := range all {
		if r.Decision == "allow" && ruleMatches(r, req, arg) {
			return Decision{Kind: DecisionApprove, Reason: "matched allow rule", RuleID: ruleID(r)}, nil
		}
	}
	// 3. fail-closed.
	return Decision{Kind: DecisionDeny, Reason: "no matching allow rule (fail-closed)", RuleID: "fail_closed"}, nil
}
