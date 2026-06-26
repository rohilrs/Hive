// Package decompose breaks a Hive task into a sequence of independently-
// shippable sub-tasks via a single Claude tool-use turn. Stateless;
// produces a validated proposal but does no DB writes.
package decompose

import (
	"context"

	"github.com/rohilrs/Hive/internal/anthropic"
)

// ProposedSubtask is one sub-task in a decomposition proposal.
// Mirrors store.SubtaskInput field-for-field so the daemon can pass
// it through to InsertSubtasks (after the user confirms).
type ProposedSubtask struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Priority string `json:"priority"`           // "P0" | "P1" | "P2" | "P3"
	Pipeline string `json:"pipeline,omitempty"` // "build" | "debug" | "plan" | "finish-branch"
	// MergeFrom, when set, is the ref of an existing item this sub-task subsumes
	// ("hive:<taskID>" or "linear:<uuid>"). Apply rewrites/pulls that item rather
	// than creating a duplicate. Empty = a brand-new task.
	MergeFrom string `json:"merge_from,omitempty"`
	// DependsOn lists the 0-based indices of EARLIER sibling sub-tasks (in this
	// same proposal) whose output this sub-task builds on. The sequenced
	// dispatcher holds this task until those deps have merged. Only backward
	// references are valid (an index < this sub-task's own index).
	DependsOn     []int    `json:"depends_on,omitempty"`
	RelevantFiles []string `json:"relevant_files,omitempty"` // repo-relative paths this sub-task is expected to touch
}

// Result is the full Decompose return value.
type Result struct {
	Subtasks     []ProposedSubtask
	Model        string
	CostUSD      float64
	InputTokens  int
	OutputTokens int
}

// Runner is the narrow interface Decompose needs from internal/anthropic.
// *anthropic.SDK satisfies this. Tests inject stubs.
type Runner interface {
	RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error)
}
