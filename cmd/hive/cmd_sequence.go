package main

import (
	"encoding/json"
	"fmt"

	"github.com/rohilrs/Hive/pkg/rpc"
	"github.com/spf13/cobra"
)

var (
	seqEnableTarget  string
	seqEnablePolicy  string
	seqCompletePhase string
)

var sequenceCmd = &cobra.Command{Use: "sequence", Short: "Manage sequenced (roadmap-phase-ordered) dispatch"}

var sequenceEnableCmd = &cobra.Command{
	Use:   "enable <project>",
	Short: "Enable sequenced dispatch (requires a roadmap + current-phase spec)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		params := map[string]any{"project_slug": args[0]}
		if seqEnableTarget != "" {
			params["target_branch"] = seqEnableTarget
		}
		if seqEnablePolicy != "" {
			params["policy"] = seqEnablePolicy
		}
		if _, err := rpcCall(rpc.MethodSequenceEnable, params); err != nil {
			return err
		}
		fmt.Printf("Sequenced dispatch enabled for %s\n", args[0])
		return nil
	},
}

var sequenceDisableCmd = &cobra.Command{
	Use:   "disable <project>",
	Short: "Disable sequenced dispatch (reverts to manual)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodSequenceDisable, map[string]any{"project_slug": args[0]}); err != nil {
			return err
		}
		fmt.Printf("Sequenced dispatch disabled for %s\n", args[0])
		return nil
	},
}

var sequenceStatusCmd = &cobra.Command{
	Use:   "status <project>",
	Short: "Show the derived roadmap-phase plan + per-task gate state",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := rpcCallRaw(rpc.MethodSequenceStatus, map[string]any{"project_slug": args[0]})
		if err != nil {
			return err
		}
		var v struct {
			Slug        string `json:"slug"`
			Status      string `json:"status"`
			Policy      string `json:"policy"`
			ActivePhase string `json:"active_phase"`
			Complete    bool   `json:"complete"`
			Phases      []struct {
				Number   string `json:"number"`
				Title    string `json:"title"`
				Complete bool   `json:"complete"`
				Tasks    []struct {
					ID        string `json:"id"`
					Title     string `json:"title"`
					Status    string `json:"status"`
					GateState string `json:"gate_state"`
				} `json:"tasks"`
				Blocked []struct {
					ID        string `json:"id"`
					Title     string `json:"title"`
					Status    string `json:"status"`
					GateState string `json:"gate_state"`
				} `json:"blocked"`
			} `json:"phases"`
			Unsequenced []struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Status    string `json:"status"`
				GateState string `json:"gate_state"`
			} `json:"unsequenced"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		state := v.Status
		if state == "" {
			state = "not enabled"
		}
		policy := v.Policy
		if policy == "" {
			policy = "-"
		}
		fmt.Printf("Project %s — sequenced dispatch: %s (policy: %s)\n", v.Slug, state, policy)
		if v.Complete {
			fmt.Println("All phases complete.")
		} else if v.ActivePhase != "" {
			fmt.Printf("Active phase: %s\n", v.ActivePhase)
		}
		for _, ph := range v.Phases {
			marker := " "
			if ph.Complete {
				marker = "x"
			} else if ph.Number == v.ActivePhase {
				marker = ">"
			}
			fmt.Printf("[%s] Phase %s: %s (%d tasks)\n", marker, ph.Number, ph.Title, len(ph.Tasks))
			for _, t := range ph.Tasks {
				fmt.Printf("      - %s [%s/%s] %s\n", t.ID, t.Status, t.GateState, t.Title)
			}
			for _, b := range ph.Blocked {
				fmt.Printf("      ! BLOCKED: %s %s\n", b.ID, b.Title)
			}
		}
		if len(v.Unsequenced) > 0 {
			fmt.Printf("Unsequenced (no roadmap phase): %d task(s)\n", len(v.Unsequenced))
		}
		return nil
	},
}

var sequencePauseCmd = &cobra.Command{
	Use:   "pause <project>",
	Short: "Pause sequenced dispatch (in-flight runs continue; no new dispatch)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodSequencePause, map[string]any{"project_slug": args[0]}); err != nil {
			return err
		}
		fmt.Printf("Sequenced dispatch paused for %s\n", args[0])
		return nil
	},
}

var sequenceResumeCmd = &cobra.Command{
	Use:   "resume <project>",
	Short: "Resume sequenced dispatch",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodSequenceResume, map[string]any{"project_slug": args[0]}); err != nil {
			return err
		}
		fmt.Printf("Sequenced dispatch resumed for %s\n", args[0])
		return nil
	},
}

var sequenceSkipCmd = &cobra.Command{
	Use:   "skip <task-id>",
	Short: "Mark a task skipped to unblock its phase",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodSequenceSkip, map[string]any{"task_id": args[0]}); err != nil {
			return err
		}
		fmt.Printf("Task %s skipped\n", args[0])
		return nil
	},
}

var sequenceAdvanceCmd = &cobra.Command{
	Use:   "advance <project>",
	Short: "Force the active phase forward: mark its awaiting-merge tasks satisfied (manual policy / stuck merge)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := rpcCallRaw(rpc.MethodSequenceAdvance, map[string]any{"project_slug": args[0]})
		if err != nil {
			return err
		}
		var r struct {
			Advanced    int    `json:"advanced"`
			ActivePhase string `json:"active_phase"`
		}
		_ = json.Unmarshal(raw, &r)
		fmt.Printf("Advanced %d task(s) in phase %s of %s\n", r.Advanced, r.ActivePhase, args[0])
		return nil
	},
}

var sequenceCompleteCmd = &cobra.Command{
	Use:   "complete <project>",
	Short: "Mark a roadmap phase complete (shipped/empty phase) so the dispatcher advances; writes the roadmap Status line back",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if seqCompletePhase == "" {
			return fmt.Errorf("--phase is required")
		}
		raw, err := rpcCallRaw(rpc.MethodSequenceComplete, map[string]any{
			"project_slug": args[0], "phase": seqCompletePhase,
		})
		if err != nil {
			return err
		}
		var res struct {
			ActivePhase    string `json:"active_phase"`
			RoadmapUpdated bool   `json:"roadmap_updated"`
		}
		_ = json.Unmarshal(raw, &res)
		fmt.Printf("Phase %s marked complete. Active phase: %s\n", seqCompletePhase, res.ActivePhase)
		if !res.RoadmapUpdated {
			fmt.Println("(note: roadmap Status line was not updated — see daemon log)")
		}
		return nil
	},
}

func init() {
	sequenceEnableCmd.Flags().StringVar(&seqEnableTarget, "target-branch", "", "integration/target branch (PR base + worktree base)")
	sequenceEnableCmd.Flags().StringVar(&seqEnablePolicy, "policy", "", "advancement policy: pr_opened|human_merge|auto_merge_on_green|manual (default pr_opened)")
	sequenceCompleteCmd.Flags().StringVar(&seqCompletePhase, "phase", "", "roadmap phase number to mark complete (e.g. 1, 2a)")
	sequenceCmd.AddCommand(sequenceEnableCmd, sequenceDisableCmd, sequenceStatusCmd, sequencePauseCmd, sequenceResumeCmd, sequenceSkipCmd, sequenceAdvanceCmd, sequenceCompleteCmd)
}
