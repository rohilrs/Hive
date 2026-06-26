package daemon

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/predictor/accuracy"
	"github.com/rohilrs/Hive/internal/store"
)

// computeAndPersistAccuracy reads predicted-files from pred + touched-files
// from `git diff --name-only main` in worktreePath, computes
// precision/recall via accuracy.Compute, persists one PredictorAccuracy
// row. Skips with a reason code when input data is missing (no
// prediction, no predictions files, no edits, no worktree).
//
// Safe to call concurrently for different runIDs. INSERT OR REPLACE
// makes per-run reinvocation idempotent. Errors are logged but not
// returned — accuracy is observability data, never critical-path.
//
// Wired from executePipeline in a goroutine after MarkRunEnded.
// Backfill CLI calls it directly with pred read from runs.prediction.
func computeAndPersistAccuracy(ctx context.Context, s *store.Store, runID string, pred *predictor.Result, worktreePath string) {
	a := &store.PredictorAccuracy{RunID: runID}

	if pred == nil {
		a.SkippedReason = "no_prediction"
		_ = s.InsertPredictorAccuracy(ctx, a)
		return
	}
	if len(pred.Files) == 0 {
		a.SkippedReason = "no_predictions_files"
		_ = s.InsertPredictorAccuracy(ctx, a)
		return
	}
	a.PredictedCount = len(pred.Files)

	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		a.SkippedReason = "no_worktree"
		_ = s.InsertPredictorAccuracy(ctx, a)
		return
	}

	touched, err := touchedFiles(ctx, worktreePath)
	if err != nil {
		log.Printf("accuracy_compute: git diff failed for run %s: %v", runID, err)
		a.SkippedReason = "git_diff_failed"
		_ = s.InsertPredictorAccuracy(ctx, a)
		return
	}
	if len(touched) == 0 {
		a.TouchedCount = 0
		a.SkippedReason = "no_edits"
		_ = s.InsertPredictorAccuracy(ctx, a)
		return
	}

	score := accuracy.Compute(pred.Files, touched)
	a.PredictedCount = score.PredictedCount
	a.TouchedCount = score.TouchedCount
	a.IntersectCount = score.IntersectCount
	a.Precision = &score.Precision
	a.Recall = &score.Recall
	if err := s.InsertPredictorAccuracy(ctx, a); err != nil {
		log.Printf("accuracy_compute: persist for run %s: %v", runID, err)
	}
}

// RunAccuracyCompute is the exported entry point for callers outside
// the daemon package (e.g., the backfill CLI). Same semantics as the
// internal computeAndPersistAccuracy.
func RunAccuracyCompute(ctx context.Context, s *store.Store, runID string, pred *predictor.Result, worktreePath string) {
	computeAndPersistAccuracy(ctx, s, runID, pred, worktreePath)
}

// touchedFiles runs `git diff --name-only main` in the given worktree
// and returns the result as a list of repo-relative paths. This
// captures BOTH committed changes (between main and HEAD) AND
// uncommitted changes (staged + unstaged in the working tree),
// which is what we want because Hive's build pipeline doesn't
// necessarily commit per iteration. Empty list when there are no
// diffs. Returns an error if git itself fails (not when the diff
// is empty).
func touchedFiles(ctx context.Context, worktreePath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "main")
	cmd.Dir = worktreePath
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// strings.Split returns []string{""} for an empty input; filter.
	var files []string
	for _, l := range lines {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}
