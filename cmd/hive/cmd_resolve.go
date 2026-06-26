package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// newResolveCmd builds the `hive resolve <task-id>` command — the MANUAL
// conflict-resolver trigger. Where the auto path (finishChainEnded →
// dispatchResolveFn) reuses the live finish-branch worktree, this command is
// for a STUCK task whose worktree is already gone: the daemon provisions a
// fresh worktree on the task's PR branch, runs the resolve pipeline, then tears
// it down. Mirrors the `hive run` shape (ExactArgs(1), rpcCallRaw).
func newResolveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <task-id>",
		Short: "Manually run the conflict-resolver on a stuck task (provisions a fresh worktree on its PR branch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := rpcCallRaw(rpc.MethodResolveNow, map[string]any{"task_id": args[0]})
			if err != nil {
				return err
			}
			fmt.Printf("Resolving task %s: %s\n", args[0], string(result))
			return nil
		},
	}
}

var resolveCmd = newResolveCmd()
