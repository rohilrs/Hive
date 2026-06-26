package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var (
	approvalsListRun                    string
	approvalsListDenied                 bool
	approvalsListLimit                  int
	approvalRuleGlob, approvalRuleScope string
)

var approvalsCmd = &cobra.Command{Use: "approvals", Short: "Inspect + manage tool-use approvals"}

var approvalsListCmd = &cobra.Command{
	Use:   "list [run-id]",
	Short: "List recent approval decisions (audit trail)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		runID := approvalsListRun
		if len(args) == 1 {
			runID = args[0] // positional run-id overrides --run
		}
		params := map[string]any{"limit": approvalsListLimit}
		if runID != "" {
			params["run_id"] = runID
		}
		raw, err := rpcCallRaw(rpc.MethodApprovalList, params)
		if err != nil {
			return err
		}
		var rows []struct {
			RunID      string `json:"run_id"`
			Stage      string `json:"stage"`
			ToolName   string `json:"tool_name"`
			Decision   string `json:"decision"`
			ResolvedBy string `json:"resolved_by"`
			Reason     string `json:"reason"`
			ToolInput  string `json:"tool_input"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("decode approvals: %w", err)
		}
		if len(rows) == 0 {
			fmt.Println("No approval decisions recorded.")
			return nil
		}

		fmt.Printf("%-7s  %-12s  %-14s  %s\n", "DECISION", "STAGE", "TOOL", "COMMAND / ARG")
		allow, deny := 0, 0
		deniedCounts := map[string]int{} // "tool\targ" -> count
		for _, r := range rows {
			if r.Decision == "approve" {
				allow++
			} else {
				deny++
			}
			if approvalsListDenied && r.Decision != "deny" {
				continue
			}
			arg := canonicalArgFromJSON(r.ToolName, r.ToolInput)
			fmt.Printf("%-7s  %-12s  %-14s  %s\n", r.Decision, r.Stage, r.ToolName, truncate1(arg, 70))
			if r.Decision == "deny" {
				deniedCounts[r.ToolName+"\t"+arg]++
			}
		}

		fmt.Printf("\n%d approved, %d denied.\n", allow, deny)
		if len(deniedCounts) > 0 {
			fmt.Println("\nDenied (allow with `hive approvals allow <tool> --glob '<pattern>'`):")
			for k, n := range deniedCounts {
				parts := strings.SplitN(k, "\t", 2)
				fmt.Printf("  %2dx  %-10s  %s\n", n, parts[0], truncate1(parts[1], 60))
			}
		}
		return nil
	},
}

// canonicalArgFromJSON extracts the human-relevant arg (Bash command,
// file path) from a tool_input JSON blob for display.
func canonicalArgFromJSON(toolName, inputJSON string) string {
	var m map[string]any
	if json.Unmarshal([]byte(inputJSON), &m) != nil {
		return ""
	}
	switch toolName {
	case "Bash":
		if c, ok := m["command"].(string); ok {
			return c
		}
	case "Edit", "Write", "Read", "MultiEdit":
		if f, ok := m["file_path"].(string); ok {
			return f
		}
	}
	return ""
}

func truncate1(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func approvalRuleCmd(decision string) *cobra.Command {
	return &cobra.Command{
		Use:   decision + " <tool>",
		Short: decision + " a tool (optionally arg-glob + scope)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{
				"tool_name": args[0],
				"decision":  decision,
			}
			if approvalRuleGlob != "" {
				params["arg_matcher"] = approvalRuleGlob
			}
			if approvalRuleScope != "" {
				params["scope"] = approvalRuleScope
			}
			result, err := rpcCall(rpc.MethodApprovalRuleAdd, params)
			if err != nil {
				return err
			}
			fmt.Printf("Added %s rule %v for %s\n", decision, result["rule_id"], args[0])
			return nil
		},
	}
}

var approvalsPendingCmd = &cobra.Command{
	Use:   "pending",
	Short: "List in-flight pending approvals (ask mode) awaiting a decision",
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := rpcCallRaw(rpc.MethodApprovalPending, map[string]any{})
		if err != nil {
			return err
		}
		var rows []struct {
			ApprovalID string         `json:"approval_id"`
			RunID      string         `json:"run_id"`
			Stage      string         `json:"stage"`
			ToolName   string         `json:"tool_name"`
			ToolInput  map[string]any `json:"tool_input"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("decode pending: %w", err)
		}
		if len(rows) == 0 {
			fmt.Println("No pending approvals.")
			return nil
		}
		for _, r := range rows {
			arg := ""
			if c, ok := r.ToolInput["command"].(string); ok {
				arg = c
			} else if f, ok := r.ToolInput["file_path"].(string); ok {
				arg = f
			}
			fmt.Printf("%s  %-10s  %-12s  %s\n", r.ApprovalID, r.ToolName, r.Stage, arg)
		}
		return nil
	},
}

var approvalsResolveApprove, approvalsResolveDeny bool

var approvalsResolveCmd = &cobra.Command{
	Use:   "resolve <approval-id>",
	Short: "Resolve a pending approval headlessly (--approve | --deny)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if approvalsResolveApprove == approvalsResolveDeny {
			return fmt.Errorf("exactly one of --approve or --deny is required")
		}
		decision := "deny"
		if approvalsResolveApprove {
			decision = "approve"
		}
		result, err := rpcCall(rpc.MethodApprovalResolve, map[string]any{
			"approval_id": args[0], "decision": decision,
		})
		if err != nil {
			return err
		}
		if r, _ := result["resolved"].(bool); !r {
			fmt.Printf("Approval %s not found (already resolved or timed out).\n", args[0])
			return nil
		}
		fmt.Printf("Resolved %s: %s\n", args[0], decision)
		return nil
	},
}

func init() {
	approvalsListCmd.Flags().StringVar(&approvalsListRun, "run", "", "filter by run id (or pass as positional arg)")
	approvalsListCmd.Flags().BoolVar(&approvalsListDenied, "denied", false, "show only denied decisions")
	approvalsListCmd.Flags().IntVar(&approvalsListLimit, "limit", 200, "max rows to fetch")

	allowCmd := approvalRuleCmd("allow")
	denyCmd := approvalRuleCmd("deny")
	for _, c := range []*cobra.Command{allowCmd, denyCmd} {
		c.Flags().StringVar(&approvalRuleGlob, "glob", "", "arg matcher glob (e.g. 'git *' for Bash)")
		c.Flags().StringVar(&approvalRuleScope, "scope", "global", "global | project:<slug> | stage:<name>")
	}
	approvalsResolveCmd.Flags().BoolVar(&approvalsResolveApprove, "approve", false, "approve the pending tool call")
	approvalsResolveCmd.Flags().BoolVar(&approvalsResolveDeny, "deny", false, "deny the pending tool call")
	approvalsCmd.AddCommand(approvalsListCmd, allowCmd, denyCmd, approvalsPendingCmd, approvalsResolveCmd)
}
