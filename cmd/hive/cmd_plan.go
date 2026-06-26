package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// planResumeID is the cobra-bound value of --resume.
var planResumeID string

// planFeatureBranch is the cobra-bound value of --feature-branch. When set (or
// when the project already has a feature branch configured), `hive plan`
// engages the integration loop: create/adopt the branch, commit docs per-save,
// and offer to push on exit.
var planFeatureBranch string

// planSetupResult is the parsed roadmap.plan_setup result the CLI needs.
type planSetupResult struct {
	Active        bool
	FeatureBranch string
	TargetBranch  string
	Clean         bool
	Report        string
}

// planCmd is the `hive plan <project>` command. It opens an interactive
// Roadmap Planner chat session for the named project: the planner agent
// scans existing specs, asks Socratic clarifying questions, and (under
// confirm) writes a roadmap markdown + drafts new spec docs.
//
// Architecture: the planner-kind chat session lives in the same
// chat_sessions table as a regular `hive chat`. The daemon's streamChat
// (Phase 8.A T6) checks the persisted session.kind and routes planner
// sessions to a dedicated agent with the planner registry + system
// prompt + ForceSonnet router. So all `hive plan` needs to do client-side
// is (a) resolve the project, (b) issue ONE chat.send with kind="plan" +
// project_slug=<slug> + a sentinel "begin" message to create the session,
// then (c) hand off to the existing runChatREPLWithSession.
//
// --resume <session-id> skips the seed step and resumes a prior planner
// session directly. The daemon validates kind on every turn so a non-
// planner session-id passed here will surface an error from the daemon.
var planCmd = newPlanCmd()

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan <project>",
		Short: "Open an interactive Roadmap Planner session for a project",
		Long: `Opens a Socratic Q&A chat with the Hive Roadmap Planner for the named
project. The planner reads your existing specs, asks one clarifying
question at a time, and (with your confirmation) writes a roadmap.md
plus drafts new spec docs as the design converges.

The session persists; list it with 'hive chat history' and resume with
'hive plan <project> --resume <session-id>'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(args[0], planResumeID, defaultPlanDeps())
		},
	}
	cmd.Flags().StringVar(&planResumeID, "resume", "", "resume an existing planner session by id")
	cmd.Flags().StringVar(&planFeatureBranch, "feature-branch", "", "feature branch for this initiative; engages the integration loop (create/adopt + commit docs + push on exit)")
	return cmd
}

// planDeps lets the test scaffolding swap out the three side-effecting
// pieces — project lookup, session-seed RPC, and REPL handoff — without
// touching a daemon socket. defaultPlanDeps wires the real implementations.
type planDeps struct {
	lookupProject func(slug string) (rpc.ProjectView, error)
	seedSession   func(slug string) (sessionID string, err error)
	runREPL       func(sessionID string) error
	// setupFeatureBranch calls roadmap.plan_setup; returns the parsed result.
	setupFeatureBranch func(slug, featureBranch string) (planSetupResult, error)
	// pushFeatureBranch calls roadmap.plan_push.
	pushFeatureBranch func(slug string) error
	// confirm prompts the operator for a y/N; returns true on yes.
	confirm func(prompt string) bool
}

func defaultPlanDeps() *planDeps {
	return &planDeps{
		lookupProject: lookupProjectBySlug,
		seedSession:   seedPlanSession,
		runREPL:       runChatREPLWithSession,
		setupFeatureBranch: func(slug, fb string) (planSetupResult, error) {
			res, err := rpcCall(rpc.MethodRoadmapPlanSetup, map[string]any{"project_slug": slug, "feature_branch": fb})
			if err != nil {
				return planSetupResult{}, err
			}
			active, _ := res["active"].(bool)
			feature, _ := res["feature_branch"].(string)
			target, _ := res["target_branch"].(string)
			clean, _ := res["clean"].(bool)
			report, _ := res["report"].(string)
			return planSetupResult{
				Active:        active,
				FeatureBranch: feature,
				TargetBranch:  target,
				Clean:         clean,
				Report:        report,
			}, nil
		},
		pushFeatureBranch: func(slug string) error {
			_, err := rpcCall(rpc.MethodRoadmapPlanPush, map[string]any{"project_slug": slug})
			return err
		},
		confirm: confirmYN,
	}
}

// confirmYN reads a line from stdin and returns true for "y"/"yes"
// (case-insensitive). Any other input (including empty/EOF) is treated as no.
func confirmYN(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// runPlan is the pure-ish orchestration logic the cobra cmd delegates to.
// It validates the project, optionally seeds a fresh planner session, and
// hands control to the chat REPL. All I/O is routed through deps so tests
// can pin behavior without dialing the daemon.
func runPlan(slug, resumeID string, deps *planDeps) error {
	if resumeID != "" {
		// Resume path: trust the user-supplied id. The daemon's streamChat
		// re-reads the session row's kind on every turn and rejects mismatches
		// (T6b validation), so we let the daemon be the gatekeeper rather than
		// duplicate the check here.
		fmt.Fprintf(os.Stderr, "[Plan] resuming session %s\n", resumeID)
		return deps.runREPL(resumeID)
	}

	proj, err := deps.lookupProject(slug)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", slug, err)
	}
	if proj.RepoPath == "" {
		return fmt.Errorf("project %q has no repo_path; set with `hive project edit %s --repo <path>`", slug, slug)
	}

	setup, serr := deps.setupFeatureBranch(slug, planFeatureBranch)
	if serr != nil {
		return fmt.Errorf("feature-branch setup: %w", serr)
	}
	if setup.Active {
		fmt.Fprintf(os.Stderr, "[Plan] feature branch %s (target %s)\n", setup.FeatureBranch, setup.TargetBranch)
		fmt.Fprint(os.Stderr, setup.Report)
		if !setup.Clean {
			if !deps.confirm("Feature branch is not clean (see above). Continue planning anyway? [y/N] ") {
				return fmt.Errorf("aborted: feature branch not clean")
			}
		}
	}

	sessionID, err := deps.seedSession(slug)
	if err != nil {
		return fmt.Errorf("start planner session: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[Plan] session %s started for project %s\n", sessionID, slug)

	replErr := deps.runREPL(sessionID)
	if setup.Active {
		if deps.confirm(fmt.Sprintf("Push %s to origin now? [y/N] ", setup.FeatureBranch)) {
			if perr := deps.pushFeatureBranch(slug); perr != nil {
				fmt.Fprintf(os.Stderr, "[Plan] push failed: %v\n", perr)
			} else {
				fmt.Fprintf(os.Stderr, "[Plan] pushed %s to origin\n", setup.FeatureBranch)
			}
		}
	}
	return replErr
}

// lookupProjectBySlug fetches the project from the daemon by listing all
// projects and filtering by slug. There's no dedicated project.get RPC
// (Phase 7 designed around the list-then-filter pattern; see
// lookupProjectSlug in cmd_decompose.go for the same approach). Returns a
// non-nil error if the daemon is unreachable or the slug isn't registered.
func lookupProjectBySlug(slug string) (rpc.ProjectView, error) {
	raw, err := rpcCallRaw(rpc.MethodListProjects, map[string]any{})
	if err != nil {
		return rpc.ProjectView{}, err
	}
	var projs []rpc.ProjectView
	if err := json.Unmarshal(raw, &projs); err != nil {
		return rpc.ProjectView{}, fmt.Errorf("decode projects: %w", err)
	}
	for _, p := range projs {
		if p.Slug == slug {
			return p, nil
		}
	}
	return rpc.ProjectView{}, fmt.Errorf("no project with slug %q", slug)
}

// seedPlanSession issues a chat.send streaming RPC with session_id="" +
// kind="plan" + project_slug=<slug> + a sentinel "begin" user message. The
// daemon's streamChat (chat_rpc.go:1090) treats session_id=="" as
// session-creation, honors kind + project_slug on that first call (and
// validates planner sessions require project_slug), and emits the new
// session id as the first frame.
//
// Why a sentinel message: the daemon rejects empty message ("message is
// required" at chat_rpc.go:1085). The PlannerSystemPrompt's step-1
// instruction tells the agent to greet + run hive_list_specs on any user
// message, so "begin" is the minimal valid trigger.
//
// We drain frames to end-of-turn so the daemon persists the assistant
// reply (the operator's first turn already happened before the REPL takes
// over). Returns the session id captured from the `session` frame.
func seedPlanSession(slug string) (string, error) {
	conn, err := dialDaemon()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	params := map[string]any{
		"message":      "begin",
		"kind":         "plan",
		"project_slug": slug,
	}
	req := rpc.Request[map[string]any]{
		ID:     fmt.Sprintf("plan-seed-%d", time.Now().UnixNano()),
		Method: rpc.MethodChatSend,
		Params: params,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal chat.send: %w", err)
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return "", fmt.Errorf("write chat.send: %w", err)
	}
	// 6.1b-i lesson #3: seed turn can take a while if the planner runs
	// hive_list_specs + a few clarifying tools; mirror forwardChatTool's
	// 600s window so a hung daemon doesn't block hive plan forever.
	_ = conn.SetReadDeadline(time.Now().Add(600 * time.Second))

	// Mirror streamChatTurn's frame loop, capturing the session id and
	// rendering the initial greeting/tool-result frames so the operator
	// sees the agent's bootstrap before the REPL prompt appears. Tool-
	// proposed frames are NOT handled here — the planner's first turn
	// only calls hive_list_specs (a read tool, no confirm). If the
	// planner does propose a mutating tool on turn 1, we'll surface an
	// error message and let the operator pick up via --resume.
	rdr := bufio.NewReader(conn)
	var sessionID string
	for {
		line, readErr := rdr.ReadBytes('\n')
		if len(line) > 0 {
			var env struct {
				Result *chat.Frame `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if jerr := json.Unmarshal(line, &env); jerr == nil {
				if env.Error != nil {
					return sessionID, errors.New(env.Error.Message)
				}
				if env.Result != nil {
					f := env.Result
					switch f.Kind {
					case "session":
						sessionID = f.Text
					case "text":
						fmt.Println(f.Text)
					case "tool_result":
						fmt.Fprintf(os.Stderr, "· [%s] %s\n", f.Tool, truncate(f.Result, 80))
					case "turn_done":
						fmt.Fprintf(os.Stderr, "(%s · $%.4f)\n", f.Model, f.CostUSD)
					case "error":
						return sessionID, errors.New(f.Text)
					}
				}
			}
		}
		if readErr != nil {
			// EOF: daemon closed the conn at end of turn.
			if sessionID == "" {
				return "", errors.New("daemon closed connection without emitting a session frame")
			}
			return sessionID, nil
		}
	}
}
