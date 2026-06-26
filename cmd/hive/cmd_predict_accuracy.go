package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/daemon"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/store"
)

var (
	accuracyRunID         string
	accuracyFormat        string
	accuracyBackfillSince string
)

var predictAccuracyCmd = &cobra.Command{
	Use:   "accuracy",
	Short: "Inspect per-run predictor accuracy (precision/recall)",
	Long: `Inspect the computed precision/recall for one run, or run the
backfill subcommand to compute accuracy for all completed runs that
don't yet have a row.`,
}

var predictAccuracyShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show accuracy for one run by ID",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, err := defaultHiveDBPath()
		if err != nil {
			return err
		}
		s, err := store.Open(cmd.Context(), dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		return runAccuracyForRun(cmd.Context(), cmd.OutOrStdout(), s, accuracyRunID, accuracyFormat)
	},
}

var predictAccuracyBackfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Compute accuracy for completed runs that lack rows",
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, err := defaultHiveDBPath()
		if err != nil {
			return err
		}
		s, err := store.Open(cmd.Context(), dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		_, err = runAccuracyBackfill(cmd.Context(), cmd.OutOrStdout(), s, accuracyBackfillSince)
		return err
	},
}

func init() {
	predictAccuracyShowCmd.Flags().StringVar(&accuracyRunID, "run", "", "run ID (required)")
	predictAccuracyShowCmd.Flags().StringVar(&accuracyFormat, "format", "human", "output format: human|json")
	_ = predictAccuracyShowCmd.MarkFlagRequired("run")
	predictAccuracyBackfillCmd.Flags().StringVar(&accuracyBackfillSince, "since", "", "limit to runs created in last N (e.g. 24h, 7d)")
	predictAccuracyCmd.AddCommand(predictAccuracyShowCmd, predictAccuracyBackfillCmd)
}

// runAccuracyForRun prints one accuracy row in the requested format.
func runAccuracyForRun(ctx context.Context, out io.Writer, s *store.Store, runID, format string) error {
	row, err := s.GetPredictorAccuracy(ctx, runID)
	if err != nil {
		return fmt.Errorf("get accuracy for %s: %w", runID, err)
	}
	if format == "json" {
		return writeAccuracyJSON(out, row)
	}
	writeAccuracyHuman(out, row)
	return nil
}

// runAccuracyBackfill iterates runs without accuracy rows and calls
// computeAndPersistAccuracy for each. Returns the number processed.
// The since arg is the same Nd|Nh|Nm format as `hive predict stats`.
func runAccuracyBackfill(ctx context.Context, out io.Writer, s *store.Store, since string) (int, error) {
	var sinceT time.Time
	if since != "" {
		d, err := parseSinceDuration(since)
		if err != nil {
			return 0, fmt.Errorf("--since: %w", err)
		}
		sinceT = time.Now().Add(-d)
	}
	ids, err := s.ListRunsWithoutAccuracy(ctx, sinceT)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "backfilling %d runs...\n", len(ids))
	for _, id := range ids {
		predJSON, predErr := s.GetPredictionJSON(ctx, id)
		var pred *predictor.Result
		if predErr == nil && len(predJSON) > 0 {
			pred = &predictor.Result{}
			if err := json.Unmarshal(predJSON, pred); err != nil {
				pred = nil // treat as no prediction
			}
		}
		// Worktree path mirrors worktree.Manager's convention:
		// <hive_dir>/worktrees/<runID>. defaultHiveDBPath gives us
		// the db.sqlite path; the parent is hive_dir.
		dbPath, _ := defaultHiveDBPath()
		hiveDir := filepath.Dir(dbPath)
		worktreePath := filepath.Join(hiveDir, "worktrees", id)
		daemon.RunAccuracyCompute(ctx, s, id, pred, worktreePath)
	}
	fmt.Fprintln(out, "done")
	return len(ids), nil
}

func writeAccuracyHuman(out io.Writer, a *store.PredictorAccuracy) {
	fmt.Fprintf(out, "run-id: %s\n", a.RunID)
	if a.SkippedReason != "" {
		fmt.Fprintf(out, "skipped: %s\n", a.SkippedReason)
		fmt.Fprintf(out, "predicted: %d\n", a.PredictedCount)
		fmt.Fprintf(out, "touched: %d\n", a.TouchedCount)
		return
	}
	if a.Precision != nil {
		fmt.Fprintf(out, "precision: %.3f\n", *a.Precision)
	}
	if a.Recall != nil {
		fmt.Fprintf(out, "recall: %.3f\n", *a.Recall)
	}
	fmt.Fprintf(out, "predicted: %d\n", a.PredictedCount)
	fmt.Fprintf(out, "touched: %d\n", a.TouchedCount)
	fmt.Fprintf(out, "intersect: %d\n", a.IntersectCount)
}

func writeAccuracyJSON(out io.Writer, a *store.PredictorAccuracy) error {
	m := map[string]any{
		"run_id":          a.RunID,
		"predicted_count": a.PredictedCount,
		"touched_count":   a.TouchedCount,
		"intersect_count": a.IntersectCount,
		"computed_at":     a.ComputedAt,
	}
	if a.Precision != nil {
		m["precision"] = *a.Precision
	}
	if a.Recall != nil {
		m["recall"] = *a.Recall
	}
	if a.SkippedReason != "" {
		m["skipped_reason"] = a.SkippedReason
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}
