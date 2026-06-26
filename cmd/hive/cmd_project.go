package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/pkg/rpc"
)

var (
	projectAddName    string
	projectAddRepo    string
	projectEditName   string
	projectEditRepo   string
	projectEditStatus string
)

var projectCmd = &cobra.Command{Use: "project", Short: "Manage projects"}

var projectAddCmd = &cobra.Command{
	Use:   "add <slug>",
	Short: "Register a new project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		params := map[string]any{"slug": args[0], "name": projectAddName}
		if projectAddRepo != "" {
			params["repo_path"] = projectAddRepo
		}
		if _, err := rpcCall(rpc.MethodAddProject, params); err != nil {
			return err
		}
		fmt.Printf("Added project %s (%s)\n", args[0], projectAddName)
		return nil
	},
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered projects",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := rpcCallRaw(rpc.MethodListProjects, map[string]any{})
		if err != nil {
			return err
		}
		var projs []rpc.ProjectView
		if err := json.Unmarshal(raw, &projs); err != nil {
			return fmt.Errorf("decode projects: %w", err)
		}
		if len(projs) == 0 {
			fmt.Println("No projects registered.")
			return nil
		}
		fmt.Printf("%-20s  %-24s  %-10s  %s\n", "SLUG", "NAME", "STATUS", "REPO")
		for _, p := range projs {
			fmt.Printf("%-20s  %-24s  %-10s  %s\n", p.Slug, p.Name, p.Status, p.RepoPath)
		}
		return nil
	},
}

var projectEditCmd = &cobra.Command{
	Use:   "edit <slug>",
	Short: "Edit a project's name, repo, or status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Send only the flags the user actually set, so an unset flag
		// (e.g. --status) never blanks the existing column.
		params := map[string]any{"slug": args[0]}
		if cmd.Flags().Changed("name") {
			params["name"] = projectEditName
		}
		if cmd.Flags().Changed("repo") {
			params["repo_path"] = projectEditRepo
		}
		if cmd.Flags().Changed("status") {
			params["status"] = projectEditStatus
		}
		if len(params) == 1 {
			return fmt.Errorf("nothing to edit: set at least one of --name, --repo, --status")
		}
		if _, err := rpcCall(rpc.MethodEditProject, params); err != nil {
			return err
		}
		fmt.Printf("Updated project %s\n", args[0])
		return nil
	},
}

var projectRmCmd = &cobra.Command{
	Use:   "rm <slug>",
	Short: "Delete a project (refused if it has running runs)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodDeleteProject, map[string]any{"slug": args[0]}); err != nil {
			return err
		}
		fmt.Printf("Deleted %s\n", args[0])
		return nil
	},
}

var projectArchiveCmd = &cobra.Command{
	Use:   "archive <slug>",
	Short: "Archive a project (status -> archived)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodArchiveProject, map[string]any{"slug": args[0]}); err != nil {
			return err
		}
		fmt.Printf("Archived %s\n", args[0])
		return nil
	},
}

// newProjectGraduateCmd is the `hive project graduate <slug>` child. It starts
// the async project.graduate RPC and streams its lifecycle events (progress,
// verdict, done, failed) until a terminal event arrives. Mirrors the async
// subscribe→start→stream pattern of `hive roadmap decompose`.
func newProjectGraduateCmd() *cobra.Command {
	var force, draft, dryRun bool
	cmd := &cobra.Command{
		Use:   "graduate <slug>",
		Short: "Validate completion + open the feature→target PR",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runProjectGraduate(args[0], force, draft, dryRun)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "bypass the audit + build gate (not the completion/health gate)")
	cmd.Flags().BoolVar(&draft, "draft", false, "open the PR as a draft")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "run all checks + audit, print verdict + PR body, do not open the PR")
	return cmd
}

// graduateFinding mirrors graduate.Finding for client-side decoding (the daemon
// publishes the *graduate.GraduationVerdict under "verdict", serialized via its
// JSON tags).
type graduateFinding struct {
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Title          string `json:"title"`
	Evidence       string `json:"evidence"`
	Recommendation string `json:"recommendation"`
}

// graduateVerdict mirrors graduate.GraduationVerdict for client-side decoding.
type graduateVerdict struct {
	Status   string            `json:"status"`
	Summary  string            `json:"summary"`
	Findings []graduateFinding `json:"findings"`
}

// runProjectGraduate starts the async graduation and blocks until the daemon
// publishes a terminal event (graduate.done or graduate.failed). It subscribes
// to the event stream BEFORE starting the job so a fast graduate's terminal
// event cannot be missed between start and subscribe. Progress + verdict are
// printed as they stream; a failed event returns a non-nil error (non-zero
// exit). A blocking verdict produces BOTH a verdict and a failed event, so the
// user sees WHY graduation was blocked before the error.
//
// Backstop: Stage-3 build + audit can be quiet for minutes, so the no-event
// idle timeout is 10 minutes (vs decompose's 15) and is RESET on every event
// received, giving an actively-progressing run unbounded total runtime while
// still catching a wedged/dead daemon. Implemented as a goroutine + timer (not
// a read deadline) so ReadBytes is never interrupted mid-frame.
func runProjectGraduate(slug string, force, draft, dryRun bool) error {
	// 1. Subscribe FIRST so we cannot miss events from a fast graduate.
	conn, err := net.DialTimeout("unix", daemonSocketPath(), 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial daemon: %w (is `hive daemon` running?)", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(fmt.Sprintf(
		`{"id":"graduate-%d","method":"events.subscribe","params":{}}%s`,
		time.Now().UnixNano(), "\n"))); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	rdr := bufio.NewReader(conn)
	if _, err := rdr.ReadBytes('\n'); err != nil { // consume ack line
		return fmt.Errorf("read subscribe ack: %w", err)
	}

	// 2. Start the async job on a SEPARATE short-lived connection (the
	//    subscribe conn is now busy streaming events).
	params := map[string]any{
		"project_slug": slug,
		"force":        force,
		"draft":        draft,
		"dry_run":      dryRun,
	}
	startRaw, err := rpcCallRawWithTimeout(rpc.MethodProjectGraduate, params, 10*time.Second)
	if err != nil {
		return err
	}
	var ack struct {
		GraduateID string `json:"graduate_id"`
	}
	if err := json.Unmarshal(startRaw, &ack); err != nil || ack.GraduateID == "" {
		return fmt.Errorf("start graduate: bad ack %s", startRaw)
	}

	// 3. Backstop: a 10-minute idle timer that closes the conn if no event
	//    arrives. Reset on every event so a slow-but-live run is not killed.
	const backstop = 10 * time.Minute
	resetCh := make(chan struct{}, 8) // buffered so progress-heavy paths don't block
	resultCh := make(chan struct{})   // closed when the main goroutine is done

	go func() {
		timer := time.NewTimer(backstop)
		defer timer.Stop()
		for {
			select {
			case <-resultCh:
				return
			case <-resetCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(backstop)
			case <-timer.C:
				conn.Close() // unblock ReadBytes in the main goroutine
				return
			}
		}
	}()
	defer close(resultCh) // signal the backstop goroutine when we return

	// 4. Read events until the terminal event (done/failed) for our graduate_id.
	for {
		line, rerr := rdr.ReadBytes('\n')
		if rerr != nil {
			return fmt.Errorf("graduate interrupted (daemon disconnected): %w", rerr)
		}

		select {
		case resetCh <- struct{}{}:
		default:
		}

		var ev rpc.EventMessage
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if id, _ := ev.Data["graduate_id"].(string); id != ack.GraduateID {
			continue
		}
		switch ev.Type {
		case rpc.EventGraduateProgress:
			label, _ := ev.Data["phase_label"].(string)
			fmt.Fprintf(os.Stderr, "→ %s\n", label)
		case rpc.EventGraduateVerdict:
			b, _ := json.Marshal(ev.Data["verdict"])
			var v graduateVerdict
			if json.Unmarshal(b, &v) == nil {
				printGraduateVerdict(v)
			}
		case rpc.EventGraduateDone:
			if dr, _ := ev.Data["dry_run"].(bool); dr {
				fmt.Fprintln(os.Stdout, "dry-run complete (no PR opened)")
			} else {
				prURL, _ := ev.Data["pr_url"].(string)
				fmt.Fprintf(os.Stdout, "PR opened: %s\n", prURL)
			}
			return nil
		case rpc.EventGraduateFailed:
			msg, _ := ev.Data["error"].(string)
			return fmt.Errorf("graduate failed: %s", msg)
		}
	}
}

// printGraduateVerdict renders a graduation verdict: status + summary headline,
// then a short findings list (severity + title per finding). A blocking verdict
// is printed even though a failed event follows, so the operator sees the cause.
func printGraduateVerdict(v graduateVerdict) {
	fmt.Fprintf(os.Stdout, "Verdict: %s\n", v.Status)
	if v.Summary != "" {
		fmt.Fprintf(os.Stdout, "  %s\n", v.Summary)
	}
	if len(v.Findings) > 0 {
		fmt.Fprintf(os.Stdout, "Findings (%d):\n", len(v.Findings))
		for _, f := range v.Findings {
			fmt.Fprintf(os.Stdout, "  [%s] %s\n", f.Severity, f.Title)
		}
	}
}

func newProjectGraduateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "graduate-status <slug>",
		Short: "Show the last graduate run's result for a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			raw, err := rpcCallRaw(rpc.MethodProjectGraduateStatus, map[string]any{"project_slug": args[0]})
			if err != nil {
				return err
			}
			var resp struct {
				Exists bool                    `json:"exists"`
				Result graduate.GraduateResult `json:"result"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return err
			}
			if !resp.Exists {
				fmt.Printf("No graduate has run for %q yet.\n", args[0])
				return nil
			}
			fmt.Print(graduate.RenderResultMarkdown(resp.Result))
			return nil
		},
	}
}

func newProjectRemediateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remediate <slug>",
		Short: "Create inbox tasks from the last graduate audit's confirmed findings",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			raw, err := rpcCallRaw(rpc.MethodProjectRemediate, map[string]any{"project_slug": args[0]})
			if err != nil {
				return err
			}
			var res struct {
				Created []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"created"`
				Skipped int `json:"skipped"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			if len(res.Created) == 0 {
				fmt.Printf("Nothing to remediate for %q", args[0])
				if res.Skipped > 0 {
					fmt.Printf(" (%d already have an open task)", res.Skipped)
				}
				fmt.Println(".")
				return nil
			}
			fmt.Printf("Created %d task(s) from graduate audit of %q:\n", len(res.Created), args[0])
			for _, t := range res.Created {
				fmt.Printf("  %s  %s\n", t.ID, t.Title)
			}
			if res.Skipped > 0 {
				fmt.Printf("Skipped %d (already has an open task).\n", res.Skipped)
			}
			return nil
		},
	}
}

func init() {
	projectAddCmd.Flags().StringVar(&projectAddName, "name", "", "human-readable project name (required)")
	projectAddCmd.Flags().StringVar(&projectAddRepo, "repo", "", "absolute path to the project repo")
	_ = projectAddCmd.MarkFlagRequired("name")

	projectEditCmd.Flags().StringVar(&projectEditName, "name", "", "new project name")
	projectEditCmd.Flags().StringVar(&projectEditRepo, "repo", "", "new repo path")
	projectEditCmd.Flags().StringVar(&projectEditStatus, "status", "", "new status (active|archived)")

	projectCmd.AddCommand(projectAddCmd, projectListCmd, projectEditCmd, projectRmCmd, projectArchiveCmd)
	projectCmd.AddCommand(newProjectConfigCmd())
	projectCmd.AddCommand(newProjectGraduateCmd())
	projectCmd.AddCommand(newProjectGraduateStatusCmd())
	projectCmd.AddCommand(newProjectRemediateCmd())
}
