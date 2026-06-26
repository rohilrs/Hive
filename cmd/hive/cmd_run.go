package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var runCmd = &cobra.Command{
	Use:   "run <task-id>",
	Short: "Run a task now (bypass priority)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := rpcCall(rpc.MethodRunNow, map[string]any{"task_id": args[0]})
		if err != nil {
			return err
		}
		fmt.Printf("Started run %s for task %s\n", result["run_id"], args[0])
		return nil
	},
}

var runFinishCmd = &cobra.Command{
	Use:   "finish <task-id>",
	Short: "Finish a completed build run: open a PR into the feature branch, watch CI, auto-merge",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		res, err := rpcCall(rpc.MethodTaskFinish, map[string]any{"task_id": args[0]})
		if err != nil {
			return err
		}
		fmt.Printf("Finishing task %v (run %v) → feature-branch PR\n", res["task_id"], res["run_id"])
		return nil
	},
}

func init() {
	runCmd.AddCommand(runFinishCmd)
}
