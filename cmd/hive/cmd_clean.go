package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var (
	cleanDryRun   bool
	cleanKeepLast int
	cleanNoBranch bool
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Reclaim per-run worktrees, scratch dirs, and hive/<run> branches for old terminal runs",
	Long: "Garbage-collect Hive's per-run on-disk artifacts. Keeps the most-recent " +
		"runs (see [cleanup] keep_last_runs) and never touches running runs. " +
		"The daemon also sweeps on boot when [cleanup] auto_sweep is true.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		params := map[string]any{"dry_run": cleanDryRun}
		if cmd.Flags().Changed("keep-last") {
			params["keep_last"] = cleanKeepLast
		}
		if cleanNoBranch {
			params["branches"] = false
		}
		out, err := rpcCallRaw(rpc.MethodCleanupRun, params)
		if err != nil {
			return err
		}
		var r struct {
			Runs   int      `json:"runs"`
			Bytes  int64    `json:"bytes"`
			DryRun bool     `json:"dry_run"`
			Kept   int      `json:"kept"`
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(out, &r)
		verb := "Reclaimed"
		if r.DryRun {
			verb = "Would reclaim"
		}
		fmt.Printf("%s %d run(s), %s (kept %d)\n", verb, r.Runs, humanBytes(r.Bytes), r.Kept)
		for _, e := range r.Errors {
			fmt.Printf("  ! %s\n", e)
		}
		return nil
	},
}

// humanBytes renders a byte count as a human-readable size (B/KiB/MiB/...).
func humanBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}

func init() {
	cleanCmd.Flags().BoolVar(&cleanDryRun, "dry-run", false, "report what would be reclaimed without removing")
	cleanCmd.Flags().IntVar(&cleanKeepLast, "keep-last", 0, "override [cleanup] keep_last_runs for this run")
	cleanCmd.Flags().BoolVar(&cleanNoBranch, "no-branches", false, "do not delete hive/<run> branches")
}
