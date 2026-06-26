package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// newMergeCmd builds `hive merge` with the `retry` subcommand — recovery for a
// task parked at the terminal merge_failed gate. It re-checks the PR (already
// merged -> satisfied; else re-arm the merge queue with a fresh attempt budget).
func newMergeCmd() *cobra.Command {
	merge := &cobra.Command{Use: "merge", Short: "Merge-queue recovery actions"}
	merge.AddCommand(&cobra.Command{
		Use:   "retry <task-id>",
		Short: "Re-attempt a task parked at merge_failed (reconcile if already merged, else re-arm)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := rpcCallRaw(rpc.MethodMergeRetry, map[string]any{"task_id": args[0]})
			if err != nil {
				return err
			}
			var res struct {
				TaskID string `json:"task_id"`
				Action string `json:"action"`
			}
			_ = json.Unmarshal(raw, &res)
			switch res.Action {
			case "satisfied":
				fmt.Printf("Task %s: PR already merged — marked satisfied\n", res.TaskID)
			case "rearmed":
				fmt.Printf("Task %s: re-armed (awaiting_merge) — the merge queue will re-attempt\n", res.TaskID)
			default:
				fmt.Printf("merge retry %s: %s\n", args[0], string(raw))
			}
			return nil
		},
	})
	return merge
}
