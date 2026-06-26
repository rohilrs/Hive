package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var (
	taskAddProject, taskAddTitle, taskAddBody, taskAddPriority, taskAddPipeline string
)

var taskCmd = &cobra.Command{Use: "task", Short: "Manage tasks"}

var taskAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a task to a project's inbox",
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := rpcCall(rpc.MethodAddTask, map[string]any{
			"project_slug": taskAddProject, "title": taskAddTitle,
			"body": taskAddBody, "priority": taskAddPriority,
			"pipeline": taskAddPipeline,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Added task %s\n", result["task_id"])
		fmt.Printf("Dispatch it with `hive run %s` (auto-dispatch is off by default; enable [scheduler] auto_dispatch to queue automatically).\n", result["task_id"])
		return nil
	},
}

var taskListCmd = &cobra.Command{
	Use:   "list",
	Short: "List actionable tasks (pending + needs-attention)",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Include needs_attention so a task that failed or got stuck (e.g. an
		// un-mergeable PR) is visible from the CLI — it otherwise hides behind
		// done runs in the TUI and never appears here at all.
		raw, err := rpcCallRaw(rpc.MethodListTasks, map[string]any{
			"statuses": []string{"pending", "needs_attention"},
		})
		if err != nil {
			return err
		}
		var tasks []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Pipeline string `json:"pipeline"`
			Priority string `json:"priority"`
		}
		if err := json.Unmarshal(raw, &tasks); err != nil {
			return fmt.Errorf("decode task list: %w", err)
		}
		if len(tasks) == 0 {
			fmt.Println("No pending or needs-attention tasks.")
			return nil
		}
		for _, t := range tasks {
			fmt.Printf("%s  [%s]  %s  %s  %s\n", t.ID, t.Status, t.Pipeline, t.Priority, t.Title)
		}
		return nil
	},
}

var taskDeleteCmd = &cobra.Command{
	Use:   "delete <task-id>...",
	Short: "Delete one or more tasks (archives the mirrored Linear issue for write-back projects)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var failed int
		for _, id := range args {
			result, err := rpcCall(rpc.MethodDeleteTask, map[string]any{"task_id": id})
			if err != nil {
				fmt.Fprintf(os.Stderr, "delete %s: %v\n", id, err)
				failed++
				continue
			}
			if deleted, _ := result["deleted"].(bool); deleted {
				fmt.Printf("Deleted task %s\n", id)
			} else {
				fmt.Printf("task %s: %v\n", id, result)
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d delete(s) failed", failed, len(args))
		}
		return nil
	},
}

func init() {
	taskAddCmd.Flags().StringVarP(&taskAddProject, "project", "p", "", "project slug (required)")
	taskAddCmd.Flags().StringVarP(&taskAddTitle, "title", "t", "", "task title (required)")
	taskAddCmd.Flags().StringVarP(&taskAddBody, "body", "b", "", "task body")
	taskAddCmd.Flags().StringVar(&taskAddPriority, "priority", "P3", "priority (P0|P1|P2|P3)")
	taskAddCmd.Flags().StringVar(&taskAddPipeline, "pipeline", "build", "pipeline (build)")
	_ = taskAddCmd.MarkFlagRequired("project")
	_ = taskAddCmd.MarkFlagRequired("title")
	taskCmd.AddCommand(taskAddCmd, taskListCmd, taskDeleteCmd)
}
