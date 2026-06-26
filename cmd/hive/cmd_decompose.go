package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// decomposeCmd is registered in main.go alongside the other subcommands.
var decomposeCmd = newDecomposeCmd()

func newDecomposeCmd() *cobra.Command {
	var yes bool
	var maxN int
	cmd := &cobra.Command{
		Use:   "decompose <task-id>",
		Short: "Break a task into sub-tasks via Sonnet and insert them as children.",
		Long: `hive decompose <task-id> runs Sonnet via a submit_subtasks tool to
propose a sequence of independently-shippable sub-tasks, renders a
numbered proposal with cost + token counts, prompts y/N (default no),
then on confirm transactionally inserts them as children of the
original task.

--yes / -y skips the prompt for scripted use. --max N overrides the
daemon's default of 10 (hard capped at 20 server-side).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]

			// Best-effort lookup of the task for the preview header. Don't
			// fail the whole command if this misses — task.decompose will
			// surface the real error if the task truly doesn't exist.
			taskTitle, projectID := lookupTaskHeader(taskID)
			projectSlug := lookupProjectSlug(projectID)

			// Call task.decompose for the proposal.
			params := map[string]any{"task_id": taskID}
			if maxN > 0 {
				params["max_subtasks"] = maxN
			}
			// 120s read deadline covers a Sonnet tool_use turn (CC subscription
			// adds ~3s claude startup; API SDK is faster but can still tip over
			// 10s on slow connections or large prompts).
			rawDec, err := rpcCallRawWithTimeout(rpc.MethodDecompose, params, 120*time.Second)
			if err != nil {
				return fmt.Errorf("decompose: %w", err)
			}
			var res decomposeResultWire
			if err := json.Unmarshal(rawDec, &res); err != nil {
				return fmt.Errorf("parse decompose result: %w", err)
			}

			renderDecomposeProposal(os.Stdout, taskID, taskTitle, projectSlug, res.toResult())

			if !yes {
				if !promptYesNo(os.Stdin) {
					fmt.Println("Cancelled — no sub-tasks inserted.")
					return nil
				}
			} else {
				// Newline so the [y/N] prompt prefix in the render doesn't
				// share a line with the first apply-output line.
				fmt.Println("y")
			}

			rawApply, err := rpcCallRaw(rpc.MethodDecomposeApply, map[string]any{
				"parent_task_id": taskID,
				"subtasks":       res.Subtasks,
			})
			if err != nil {
				return fmt.Errorf("decompose apply: %w", err)
			}
			var applied struct {
				InsertedTaskIDs []string `json:"inserted_task_ids"`
			}
			if err := json.Unmarshal(rawApply, &applied); err != nil {
				return fmt.Errorf("parse apply result: %w", err)
			}
			fmt.Printf("Inserted %d sub-tasks:\n", len(applied.InsertedTaskIDs))
			for i, id := range applied.InsertedTaskIDs {
				if i < len(res.Subtasks) {
					fmt.Printf("  %s  %s\n", id, res.Subtasks[i].Title)
				} else {
					fmt.Printf("  %s\n", id)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().IntVar(&maxN, "max", 0, "max number of sub-tasks (default 10, hard cap 20)")
	return cmd
}

// decomposeResultWire is the wire shape — matches DecomposeResult on
// the daemon side. Kept in the cmd package to avoid the daemon import
// (cmd_hive must not import internal/daemon).
type decomposeResultWire struct {
	Subtasks     []decompose.ProposedSubtask `json:"subtasks"`
	Model        string                      `json:"model"`
	CostUSD      float64                     `json:"cost_usd"`
	InputTokens  int                         `json:"input_tokens"`
	OutputTokens int                         `json:"output_tokens"`
}

func (w *decomposeResultWire) toResult() *decompose.Result {
	return &decompose.Result{
		Subtasks:     w.Subtasks,
		Model:        w.Model,
		CostUSD:      w.CostUSD,
		InputTokens:  w.InputTokens,
		OutputTokens: w.OutputTokens,
	}
}

// lookupTaskHeader best-effort fetches title + project_id for the preview
// header. On any error (no daemon, missing task) returns empty strings;
// the caller falls back to displaying just the task id, and the real
// task.decompose call will surface the actual error.
func lookupTaskHeader(taskID string) (title, projectID string) {
	raw, err := rpcCallRaw(rpc.MethodGetTask, map[string]any{"task_id": taskID})
	if err != nil {
		return "", ""
	}
	var t struct {
		Title     string `json:"title"`
		ProjectID string `json:"project_id"`
	}
	_ = json.Unmarshal(raw, &t)
	return t.Title, t.ProjectID
}

// lookupProjectSlug resolves the project's slug from its id using
// project.list (there's no project.get RPC). Empty string on any
// failure — caller falls back to "(unknown)".
func lookupProjectSlug(projectID string) string {
	if projectID == "" {
		return ""
	}
	raw, err := rpcCallRaw(rpc.MethodListProjects, map[string]any{})
	if err != nil {
		return ""
	}
	var projs []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &projs); err != nil {
		return ""
	}
	for _, p := range projs {
		if p.ID == projectID {
			return p.Slug
		}
	}
	return ""
}

// renderDecomposeProposal prints a human-readable proposal to w. Pure —
// no I/O outside w, so tests can pin behavior on a bytes.Buffer.
func renderDecomposeProposal(w io.Writer, taskID, taskTitle, projectSlug string, res *decompose.Result) {
	header := taskID
	if taskTitle != "" {
		header = fmt.Sprintf("%s: %q", taskID, taskTitle)
	}
	fmt.Fprintf(w, "Decomposing %s\n\n", header)
	fmt.Fprintf(w, "Proposed breakdown (%d sub-tasks, $%.3f on %s, %.1fk in / %d out tokens):\n\n",
		len(res.Subtasks), res.CostUSD, res.Model, float64(res.InputTokens)/1000.0, res.OutputTokens)
	for i, st := range res.Subtasks {
		fmt.Fprintf(w, "  %d. [%s / %s] %s\n", i+1, st.Priority, st.Pipeline, st.Title)
		bodyPreview := previewBody(st.Body, 240)
		for _, line := range strings.Split(bodyPreview, "\n") {
			fmt.Fprintf(w, "     %s\n", line)
		}
		fmt.Fprintln(w)
	}
	slug := projectSlug
	if slug == "" {
		slug = "(unknown)"
	}
	fmt.Fprintf(w, "Insert %d sub-tasks into project %s? [y/N] ", len(res.Subtasks), slug)
}

// previewBody truncates body to at most maxChars runes, appending an
// ellipsis if cut. Rune-aware so a multi-byte codepoint at the boundary
// isn't sliced mid-byte (same gotcha caught in 6.2 truncateMiddle review).
func previewBody(body string, maxChars int) string {
	body = strings.TrimSpace(body)
	rs := []rune(body)
	if len(rs) > maxChars {
		return string(rs[:maxChars]) + "…"
	}
	return body
}

// promptYesNo reads a single line from in and returns true iff the
// (case-insensitive, trimmed) input is "y" or "yes". Empty/anything-else
// is treated as no — the prompt label is "[y/N]" so default-no matches
// the user's mental model.
func promptYesNo(in io.Reader) bool {
	reader := bufio.NewReader(in)
	line, _ := reader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
