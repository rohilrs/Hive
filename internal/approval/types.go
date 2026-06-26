// Package approval owns Hive's tool-use approval routing. Phase 1b ships
// an allow-all stub; Phase 4.5 replaces it with the real rule engine
// (RealEngine) reached via Claude Code's --permission-prompt-tool.
package approval

type ToolUseRequest struct {
	RunID, Stage, ToolName string
	Project                string // project slug, for project-scoped rules
	ToolInput              map[string]any
	Reasoning              string
}

type DecisionKind string

const (
	DecisionApprove DecisionKind = "approve"
	DecisionDeny    DecisionKind = "deny"
)

type Decision struct {
	Kind   DecisionKind
	Reason string
	RuleID string
}
