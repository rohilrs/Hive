package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

// predictExecutor is the minimal interface runPredict needs from a
// predictor. Defined here so tests can swap in a fake without pulling
// in the full Predictor struct + dependencies.
type predictExecutor interface {
	Predict(ctx context.Context, task, repoRoot, stageDir string) (*predictor.Result, error)
}

var (
	predictTask    string
	predictProject string
	predictFormat  string
)

var predictCmd = &cobra.Command{
	Use:   "predict",
	Short: "Dry-run predictor: print files/capsules Haiku would surface for a task",
	Long: `Runs the Hive predictor against a task description and prints what
it would suggest pre-fetching, without dispatching anything. Useful
for iterating on the predictor prompt or eyeballing the signal.

By default uses the 'claude' CLI binary (Claude Max subscription auth).
Set [llm] provider = "api" in ~/.hive/config.toml to use the Anthropic
SDK instead (requires ANTHROPIC_API_KEY in the environment).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(config.LoadOptions{ProjectSlug: predictProject})
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		p := pickPredictor(cfg)
		// Repo root: cwd. Stage dir: a tempdir under /tmp so prefetch.md
		// is inspectable but doesn't pollute the repo.
		repoRoot, err := os.Getwd()
		if err != nil {
			return err
		}
		stageDir, err := os.MkdirTemp("", "hive-predict-*")
		if err != nil {
			return err
		}
		return runPredictInDir(cmd.Context(), cmd.OutOrStdout(), p, predictTask, repoRoot, stageDir, predictFormat)
	},
}

func init() {
	predictCmd.Flags().StringVar(&predictTask, "task", "", "task description (required)")
	predictCmd.Flags().StringVar(&predictProject, "project", "", "project slug for per-project config overrides (optional)")
	predictCmd.Flags().StringVar(&predictFormat, "format", "human", "output format: human|json")
	_ = predictCmd.MarkFlagRequired("task")
	predictCmd.AddCommand(predictStatsCmd)
	predictCmd.AddCommand(predictAccuracyCmd)
}

// runPredictInDir is the cobra-facing entry point that passes an explicit
// stageDir to the predictor (so prefetch.md lands in a real tempdir).
func runPredictInDir(ctx context.Context, out io.Writer, p predictExecutor, task, repoRoot, stageDir, format string) error {
	res, err := p.Predict(ctx, task, repoRoot, stageDir)
	if err != nil {
		return err
	}
	return formatResult(out, res, format)
}

// runPredict is the test-facing entry point. It passes "/tmp" as the stageDir
// so tests don't need to manage tempdir lifecycle.
func runPredict(ctx context.Context, out io.Writer, p predictExecutor, task, repoRoot, format string) error {
	res, err := p.Predict(ctx, task, repoRoot, "/tmp")
	if err != nil {
		return err
	}
	return formatResult(out, res, format)
}

func formatResult(out io.Writer, res *predictor.Result, format string) error {
	if res == nil {
		fmt.Fprintln(out, "no prediction available (predictor degraded)")
		return nil
	}
	switch format {
	case "json":
		return writeJSON(out, res)
	default:
		return writeHuman(out, res)
	}
}

func writeJSON(out io.Writer, res *predictor.Result) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"files":            res.Files,
		"inline_capsules":  capsuleMetas(res.InlineCapsules),
		"overflow":         res.Overflow,
		"full_bundle_path": res.FullBundlePath,
		"metrics":          res.Metrics,
	})
}

func capsuleMetas(caps []capsule.Capsule) []map[string]any {
	out := make([]map[string]any, len(caps))
	for i, c := range caps {
		out[i] = map[string]any{
			"target":         c.Target,
			"token_estimate": c.TokenEstimate,
		}
	}
	return out
}

func writeHuman(out io.Writer, res *predictor.Result) error {
	fmt.Fprintf(out, "candidates: %d   inline: %d   overflow: %d   haiku_latency: %s\n\n",
		res.Metrics.CandidateCount, res.Metrics.InlineCount, res.Metrics.OverflowCount, res.Metrics.HaikuLatency)
	fmt.Fprintln(out, "Files (for conflict guard):")
	for _, f := range res.Files {
		fmt.Fprintf(out, "  - %s\n", f)
	}
	fmt.Fprintln(out, "\nInline capsules:")
	for _, c := range res.InlineCapsules {
		fmt.Fprintf(out, "  [%d tokens] %s\n", c.TokenEstimate, c.Target)
	}
	if len(res.Overflow) > 0 {
		fmt.Fprintln(out, "\nOverflow (pointer list only):")
		for _, c := range res.Overflow {
			fmt.Fprintf(out, "  - %s", c.File)
			if c.Symbol != "" {
				fmt.Fprintf(out, ":%s", c.Symbol)
			}
			fmt.Fprintf(out, " (score=%.2f) %s\n", c.Score, c.Reason)
		}
	}
	fmt.Fprintf(out, "\nFull bundle: %s\n", res.FullBundlePath)
	return nil
}
