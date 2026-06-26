package pipeline

import (
	"context"

	"github.com/rohilrs/Hive/internal/verdict"
)

// Iteration is the per-iter input to the loop detector. The diff is
// the worktree state at the end of the implement stage (cumulative
// diff vs base branch, since BuildPipeline doesn't commit per iter).
// FileRefs is the reviewer's CHANGES_REQUESTED feedback for that iter
// (empty for APPROVE iters, but APPROVE iters never trigger loop
// detection so this is moot).
type Iteration struct {
	Diff     string
	FileRefs []verdict.FileRef
}

// LoopDetector judges whether two consecutive iters represent the same
// situation (model going in circles). Returns a similarity score in
// [0.0, 1.0] where higher = more similar. Implementations are
// fail-safe: errors yield (0, error) and caller treats as "no loop."
//
// Method name matches the claudecli/anthropic Client convention
// (ClassifyVerdict, PredictFiles, ClassifyLoopSimilarity) so a
// *claudecli.Client satisfies this structurally without a wrapper.
type LoopDetector interface {
	ClassifyLoopSimilarity(ctx context.Context, prev, curr Iteration) (float64, error)
}

// escalateLadderIdx implements the "advance one tier" policy. Returns
// the new index and an `escalated` flag.
//
// escalated=true: we successfully advanced (the new index may or may
// not be at top of ladder — caller gives the new tier a chance).
// escalated=false: we were ALREADY at top before the call — nowhere
// to advance — caller should bail (e.g., mark needs_attention).
//
// A single-entry ladder is always at-top (escalated=false on first
// call).
func escalateLadderIdx(currentIdx, ladderLen int) (newIdx int, escalated bool) {
	if ladderLen <= 1 {
		return 0, false
	}
	if currentIdx >= ladderLen-1 {
		return ladderLen - 1, false
	}
	return currentIdx + 1, true
}
