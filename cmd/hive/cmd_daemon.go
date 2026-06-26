package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/adapter/claudecode"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/conflict"
	"github.com/rohilrs/Hive/internal/daemon"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/llm/claudecli"
	"github.com/rohilrs/Hive/internal/scavenger"
	"github.com/rohilrs/Hive/internal/sources"
	"github.com/rohilrs/Hive/internal/store"
)

// Compile-time assertion: *claudecli.OneshotToolRunner must satisfy graduate.Runner.
// Both packages are already imported at the cmd layer, so there is no import cycle.
var _ graduate.Runner = (*claudecli.OneshotToolRunner)(nil)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run the Hive daemon (long-running orchestrator)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(config.LoadOptions{})
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		hiveBin, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve hive binary: %w", err)
		}

		realHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}

		// Phase 3.2: open the store up front so both the claudecode
		// adapter (via the stall adapter) and the daemon share it. The
		// pre-3.2 flow had daemon.New open the store, which left the
		// adapter without persistence access.
		hiveDir := filepath.Join(realHome, ".hive")
		if err := os.MkdirAll(hiveDir, 0700); err != nil {
			return fmt.Errorf("mkdir %s: %w", hiveDir, err)
		}

		// Hive-specific gh auth: if <hiveDir>/gh_token exists, load it into
		// GH_TOKEN for the daemon — and thus every gh subprocess (create-pr,
		// ci-monitor, pr merge), which inherit os.Environ(). gh resolves
		// GH_TOKEN > GITHUB_TOKEN > keyring, so this lets Hive use a dedicated
		// classic PAT (repo scope → can read GH-Actions check runs that
		// fine-grained PATs can't) WITHOUT touching the operator's interactive
		// gh auth. Absent/empty file = no-op (keeps existing behavior).
		if tok, terr := os.ReadFile(filepath.Join(hiveDir, "gh_token")); terr == nil {
			if t := strings.TrimSpace(string(tok)); t != "" {
				_ = os.Setenv("GH_TOKEN", t)
				log.Printf("gh: using Hive-specific GH_TOKEN from %s/gh_token", hiveDir)
			}
		}
		storeInst, err := store.Open(cmd.Context(), filepath.Join(hiveDir, "db.sqlite"))
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}

		// Scavenger client (Phase 2a). Always constructed; the adapter
		// and daemon both check Enabled before using it. The daemon now
		// manages per-run worktree-scoped indexes/plugins/daemons (see
		// executePipeline), not a single global daemon.
		scavClient := scavenger.NewClient(scavenger.Config{
			Binary:               cfg.Scavenger.Binary,
			IndexTimeout:         time.Duration(cfg.Scavenger.IndexTimeoutSeconds) * time.Second,
			MaxConcurrentDaemons: cfg.Scavenger.MaxConcurrentDaemons,
		})

		// Phase 3.2: convert StallDetection seconds/minutes into
		// time.Duration for the claudecode adapter. A zero threshold
		// disables the corresponding layer.
		heartbeat := time.Duration(cfg.StallDetection.EventHeartbeatSeconds) * time.Second
		toolTimeout := time.Duration(cfg.StallDetection.ToolCallMaxMinutes) * time.Minute

		adp := claudecode.New(claudecode.Config{
			Binary:                 cfg.ClaudeCLI.Binary,
			HiveBinary:             hiveBin,
			RealHome:               realHome,
			Classifier:             pickClassifierSDK(cfg),
			Scavenger:              scavClient,
			ScavengerEnabled:       cfg.Scavenger.Enabled,
			ScavengerBinary:        cfg.Scavenger.Binary,
			StallStore:             newDaemonStallAdapter(storeInst),
			StallHeartbeatTimeout:  heartbeat,
			StallToolCallTimeout:   toolTimeout,
			StallDiffStagnation:    time.Duration(cfg.StallDetection.ImplementStagnationMinutes) * time.Minute,
			ApprovalsEnabled:       cfg.Approvals.Enabled,
			DaemonSocket:           filepath.Join(hiveDir, "daemon.sock"),
			ApprovalTimeoutSeconds: cfg.Approvals.HookTimeoutSeconds,
		})

		if cfg.Approvals.Enabled {
			mode := cfg.Approvals.Mode
			if mode == "" {
				mode = "ask"
			}
			log.Printf("approvals: enabled (mode=%s)", mode)
		}

		// Per-run scavenger lifecycle. The daemon's executePipeline gates
		// on Scavenger.Enabled + IndexWorktreeOnRun before touching the
		// client, so we always hand it the concrete client and let those
		// per-run checks decide. The adapter also gets the client (for
		// PluginDir discovery) regardless.
		var scavLifecycle daemon.ScavengerLifecycle = scavClient

		guard := conflict.NewGuard()

		// T3 persistent-mcp-fetcher: lift the predictor into a local so we
		// can Close() its capsule.Fetcher at shutdown. With NewMCPFetcher
		// the fetcher owns per-repo `scavenger mcp-bridge` subprocesses;
		// the io.Closer assertion is the teardown hook (CLIFetcher returns
		// false on the assertion and is a no-op, so this is also safe if
		// we ever swap back).
		predictorInst := pickPredictor(cfg)
		defer func() {
			if closer, ok := predictorInst.Fetcher.(io.Closer); ok {
				_ = closer.Close()
			}
		}()

		d, err := daemon.New(daemon.Config{
			Cfg:          cfg,
			Adapter:      adp,
			Scavenger:    scavLifecycle,
			Predictor:    predictorInst,
			Guard:        guard,
			Store:        storeInst,
			LoopDetector: pickLoopDetector(cfg),
		})
		if err != nil {
			return err
		}

		// Wire the daemon's StageRegistry into the adapter so the HTTP MCP
		// /mcp/stage route can find the per-stage verdict.Listener directly
		// (bypassing the UDS hop). The adapter is constructed before the
		// daemon so this must happen post-New.
		adp.SetStageVerdicts(d.StageVerdicts())

		// Phase 8.B: wire the decompose runner used by task.decompose
		// AND roadmap.decompose. Provider preference now mirrors the
		// chat path:
		//   - "claude-code" (default) → OneshotToolRunner spawns a
		//     stdio-MCP `claude -p` per turn on the user's Claude
		//     subscription (no API billing).
		//   - "api" → anthropic.SDK reads ANTHROPIC_API_KEY at
		//     daemon-construction time; if unset, the runner is left
		//     nil and the handlers return a clear "not configured"
		//     error per-request rather than failing daemon startup.
		// This change also affects the existing hive decompose <task>
		// path — it now uses the subscription by default instead of
		// silently doing nothing when ANTHROPIC_API_KEY is unset.
		// Decompose model from config ([models] decompose_model); empty falls
		// back to decompose.DefaultModel so behavior is unchanged when unset.
		decomposeModel := cfg.Models.DecomposeModel
		if decomposeModel == "" {
			decomposeModel = decompose.DefaultModel
		}
		if cfg.Chat.Provider == "api" {
			if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
				// SDKConfig.Model is the FALLBACK the SDK uses only when a turn
				// sets no model. decompose currently stamps TurnInput.Model =
				// decompose.DefaultModel, which the SDK honors first — so on the
				// API path the effective model stays decompose.DefaultModel until
				// decompose is taught to take the model from config. The
				// subscription path below has no such caveat (cfg.Model wins).
				d.SetDecomposeRunner(anthropic.NewSDK(anthropic.SDKConfig{APIKey: key, Model: decomposeModel}))
			}
		} else {
			// Subscription path: cfg.Model wins over the per-turn TurnInput.Model,
			// so this FULLY pins the decompose model from config.
			d.SetDecomposeRunner(claudecli.NewOneshotToolRunner(claudecli.Config{Binary: cfg.ClaudeCLI.Binary, Model: decomposeModel}))
		}

		// Graduate audit runner: always uses OneshotToolRunner (the audit roams a
		// worktree via RunRoamingTool, which the anthropic.SDK path does not support).
		// This is intentionally NOT gated on the API-key branch above.
		graduateModel := cfg.Models.GraduateValidatorModel
		d.SetGraduateRunner(claudecli.NewOneshotToolRunner(claudecli.Config{Binary: cfg.ClaudeCLI.Binary, Model: graduateModel}))

		// Phase 7 restart-recovery: stamp runs.worker_pid when a stage
		// subprocess starts; clear it when the subprocess exits. The
		// boot-path recoverOrphanedWorkers sweep reads this to SIGKILL
		// any leftover workers from a previous crashed daemon.
		//
		// context.Background() (not cmd.Context()) is deliberate: we
		// want the clear to succeed even mid-shutdown when the daemon
		// ctx is already canceled. The stamp is one-shot per worker
		// start and doesn't need cancellation either.
		adp.SetWorkerCallbacks(
			func(runID string, pid int) error {
				return storeInst.SetRunWorkerPID(context.Background(), runID, pid)
			},
			func(runID string, pid int) {
				_ = storeInst.ClearRunWorkerPID(context.Background(), runID)
			},
		)

		// Phase 6.1: select + inject the chat agent based on the configured
		// provider. "claude-code" runs the agent on a Claude subscription via
		// `claude -p` (no API key, --resume continuity); "api" (default) runs
		// the SDK-backed agent reading ANTHROPIC_API_KEY. If the SDK key is
		// missing we log + leave the agent nil so streamChat reports it per
		// turn rather than failing daemon startup.
		switch cfg.Chat.Provider {
		case "claude-code":
			chatAgent := claudecode.NewChatAgent(
				claudecode.Config{
					Binary:       cfg.ClaudeCLI.Binary,
					HiveBinary:   hiveBin,
					DaemonSocket: filepath.Join(hiveDir, "daemon.sock"),
					RealHome:     realHome,
					// Model left empty → claude's default (or pin via cfg.Chat.DefaultModel if desired).
					UseHTTPChat: cfg.Chat.UseHTTPMCP,
					MCPURLPath:  filepath.Join(hiveDir, "mcp.url"),
				},
				`You are Hive's assistant. Answer questions and perform actions via the tools advertised under tools/list. Every tool name begins with the prefix mcp__hive_chat__ — you MUST use the full prefixed name in tool_use blocks (e.g. mcp__hive_chat__hive_list_tasks, NOT hive_list_tasks). Calling an unprefixed name returns "No such tool available".

Read tools (no confirmation): mcp__hive_chat__hive_list_tasks, mcp__hive_chat__hive_get_task, mcp__hive_chat__hive_status, mcp__hive_chat__hive_cost_summary, mcp__hive_chat__hive_active_workers, mcp__hive_chat__hive_get_run, mcp__hive_chat__hive_search, mcp__hive_chat__hive_list_projects (list projects with ids, slugs, names), mcp__hive_chat__hive_predict (recompute cost/files prediction for a task).

Mutating tools (the user is prompted to approve y/n before each call): mcp__hive_chat__hive_add_task (create a task in a project's inbox), mcp__hive_chat__hive_run_now (dispatch a pending task), mcp__hive_chat__hive_abandon (cancel a running run), mcp__hive_chat__hive_edit_task (update a task's title/body), mcp__hive_chat__hive_approve (resolve a pending approval as approved), mcp__hive_chat__hive_deny (resolve a pending approval as denied), mcp__hive_chat__hive_resume (re-launch a needs_attention/error/abandoned run against its existing worktree), mcp__hive_chat__hive_decompose (break a task into sub-tasks and insert them as children).

Briefly think about which tool fits the request before issuing the tool call, then call it. If a tool call returns "No such tool available", FIRST verify your tool name starts with mcp__hive_chat__ — if not, retry with the prefixed name. If the name is already prefixed and you still get the error, wait one short beat and try the same tool again — the hive_chat MCP server may register asynchronously on a fresh invocation. Be concise.

# Output format for the Hive TUI

Your responses render inside a narrow terminal panel (usually 60–80 cols
wide). Follow these formatting rules strictly:

1. NEVER use markdown tables. The terminal renderer cannot fit them.
   No exceptions, even for "structured" data.

2. For lists of items (even single-item lists), use the stacked block format:

       **<Name>**  ·  <slug-or-short-id>
         <one short detail line>

   Separate items with a single blank line. Name is bold. The
   handle (slug for projects, short ID for tasks) is plain text — do
   NOT wrap it in backticks. The detail line is a `+"`·`"+`-joined summary
   of the most useful facts.

3. Open list responses with a one-sentence count ("You have 3 active
   projects:" / "Two pending tasks:"). Use stacked blocks even when
   there is only one result — do not collapse to prose. Consistency
   matters more than compactness.

4. Elide long identifiers:
   - Task / Run IDs (task-… / run-…): first 4 chars after the prefix +
     ellipsis + last 4, e.g. task-1780…4185. Do NOT wrap in backticks.
   - Paths: substitute $HOME with ~. If still long, show
     /firstSegment/…/lastTwo/segments.
   - Project IDs (proj-…): never show; the slug is the handle.

5. Detail lines per item type:
   - Project: headline is **Name**  ·  slug  ·  ~/path (path elided with
     ~ for $HOME and /first/…/lastTwo/segments for paths over 32 chars).
     Detail line shows non-zero task counts only:
       1 pending  ·  28 needs_attention  ·  40 done
     Omit a status bucket if its count is 0. Omit the detail line
     entirely if there are no tasks. The path goes on the HEADLINE,
     NEVER on a second indented line.

     BAD (path on detail line):
       **Hive (smoke test)**  ·  hive
         /mnt/e/Documents/Hive/hive

     GOOD (path on headline, optional counts on detail):
       **Hive (smoke test)**  ·  hive  ·  ~/Documents/Hive/hive
         1 pending  ·  40 done

   - Task: ONE LINE only, no detail line. Format:
       **<title>**  ·  <pipeline>  ·  <priority>  ·  <status>
     Do NOT include the task ID anywhere — the title is enough for the
     user to recognize. If two tasks share the same title in the same
     response, append " (in <project-slug>)" to disambiguate.

     BAD (task ID on tail + status on second line):
       **test smoke task**  ·  task-1780…4185
         pending  ·  P3  ·  build

     GOOD (single line, no ID):
       **test smoke task**  ·  build  ·  P3  ·  pending

   - Run: ONE LINE only. Format:
       **<title-or-pipeline>**  ·  <status>  ·  <pipeline>  ·  <age>

6. Use bold only for item names (the recognition target). Do not bold
   statuses, counts, or headers.

7. Do not wrap output in code fences unless it is genuinely code the
   user should copy.

8. If the user explicitly asks for "full IDs", "raw data", or "all
   columns", show full values — but still use stacked blocks, never
   tables.

9. Tool result conventions:
   - When a tool_result content is exactly {"error":"user cancelled, do not retry"}: stop trying that tool, briefly acknowledge, and wait for the next user message. Do not look for workarounds.
   - When a tool_result content begins with [user edited args before running] : the user adjusted your proposed args before approval; the rest of the content is the actual tool result from the edited call.`,
				filepath.Join(hiveDir, "chat"),
				d.Store(), // *store.Store satisfies claudecode.ChatSessionStore
			)
			d.SetChatAgent(chatAgent)
		default: // "api" (and unknown → fall back to api with a logged warning)
			if cfg.Chat.Provider != "" && cfg.Chat.Provider != "api" {
				log.Printf("chat: unknown provider %q, falling back to api", cfg.Chat.Provider)
			}
			sdkAgent, err := d.BuildSDKChatAgent()
			if err != nil {
				log.Printf("chat: SDK agent unavailable (%v); chat disabled until the API key is set", err)
			} else {
				d.SetChatAgent(sdkAgent)
			}
		}

		// Phase 8.A T6/T6b: planner agent factory (built per planner-kind
		// session in streamChat so each session reads + writes against
		// its own project's docs/superpowers/ tree). Provider-aware:
		//   - "claude-code" → buildPlannerCCAgent (subscription-billed,
		//     advertises PlannerToolNames via the chat-tools MCP server
		//     spawned with --mode plan)
		//   - "api" (and unknown fall-through) → BuildPlannerSDKAgent
		// Without an API key on the "api" path, the factory still wires
		// through but returns an error per-call so streamChat surfaces a
		// clear "planner unavailable" frame rather than panicking.
		switch cfg.Chat.Provider {
		case "claude-code":
			d.SetPlannerAgentFor(func(slug, cwd string) (chat.Agent, error) {
				return buildPlannerCCAgent(cfg, hiveBin, realHome, hiveDir, slug, cwd, d.Store()), nil
			})
		default:
			d.SetPlannerAgentFor(func(slug, cwd string) (chat.Agent, error) {
				return d.BuildPlannerSDKAgent(slug, cwd)
			})
		}

		// Phase 5.0: register the local-inbox source. Items land in
		// <hiveDir>/inbox/<project-slug>/*.md; the reconciler (later phase)
		// drives the sync cadence (config.Sources.Inbox.SyncIntervalMinutes).
		d.RegisterSource(&sources.InboxSource{Root: filepath.Join(hiveDir, "inbox")})
		// Phase 5.1: GitHub issue sync via the gh CLI (auth handled by gh).
		d.RegisterSource(&sources.GitHubSource{})
		// Phase 5.2: Linear issue sync via GraphQL (key from the configured env var).
		d.RegisterSource(&sources.LinearSource{APIKeyEnv: cfg.Sources.Linear.APIKeyEnv})
		// Linear write-back Phase 1: Hive->Linear mirror + status reconciler.
		d.SetLinearWriter(&sources.LinearWriter{APIKeyEnv: cfg.Sources.Linear.APIKeyEnv})

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() { <-sigCh; cancel() }()

		fmt.Printf("hive daemon started; ctrl-c to stop  (adapter: %s, scavenger: %s)\n",
			adp.Name(), scavengerStatus(cfg))
		if err := d.Start(ctx); err != nil {
			return err
		}
		d.Stop()
		return nil
	},
}

// buildPlannerCCAgent constructs a planner-mode CC ChatAgent (Phase 8.A T6b).
// It mirrors the regular chat-agent construction but:
//   - uses chat.PlannerSystemPrompt(slug, cwd) so the model knows it's
//     planning and which project it's planning for
//   - calls claudecode.NewPlannerChatAgent so the agent advertises
//     PlannerToolNames via --allowedTools AND spawns the chat-tools MCP
//     subprocess with --mode plan (so tools/list also matches)
//   - uses a planner-specific scratch dir (<hiveDir>/plan) so planner
//     sessions and regular chat sessions don't share --resume state
//
// Lives in cmd_daemon.go (not internal/daemon) to preserve the adapter
// boundary invariant: only the composition root may import claudecode.
func buildPlannerCCAgent(cfg *config.Config, hiveBin, realHome, hiveDir, slug, cwd string, sessions claudecode.ChatSessionStore) chat.Agent {
	return claudecode.NewPlannerChatAgent(
		claudecode.Config{
			Binary:       cfg.ClaudeCLI.Binary,
			HiveBinary:   hiveBin,
			DaemonSocket: filepath.Join(hiveDir, "daemon.sock"),
			RealHome:     realHome,
			UseHTTPChat:  cfg.Chat.UseHTTPMCP,
			MCPURLPath:   filepath.Join(hiveDir, "mcp.url"),
			// Planner always wants reasoning depth (its router runs ForceSonnet),
			// so pin it to the configured reasoning model. Empty → CLI default.
			Model: cfg.Chat.ReasoningModel,
		},
		chat.PlannerSystemPrompt(slug, cwd),
		filepath.Join(hiveDir, "plan"),
		sessions,
	)
}

// scavengerStatus returns a short human-readable label for the banner
// indicating whether scavenger is off, managed per-run, or index-only.
func scavengerStatus(cfg *config.Config) string {
	if !cfg.Scavenger.Enabled {
		return "disabled"
	}
	if cfg.Scavenger.IndexWorktreeOnRun {
		return "per-run"
	}
	return "index-only"
}
