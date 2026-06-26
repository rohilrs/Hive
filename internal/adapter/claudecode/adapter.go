package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/scavenger"
	"github.com/rohilrs/Hive/internal/verdict"
)

// ErrStageTimeout is returned by RunStage when the stage subprocess is killed
// because its StageRequest.Timeout deadline fired — the agent ran longer than
// the stage budget (default 20m for build). Distinct from ErrToolCallStall (a
// single tool call exceeding the per-tool budget). executePipeline maps it to a
// clear needs_attention summary instead of the raw, cryptic "signal: killed".
var ErrStageTimeout = errors.New("stage timed out")

type stageTimeoutError struct {
	stage string
	d     time.Duration
}

func (e *stageTimeoutError) Error() string {
	stage := e.stage
	if stage == "" {
		stage = "stage"
	}
	return fmt.Sprintf("%s timed out after %s (subprocess killed — the agent ran past the stage budget; it may be stuck or looping)", stage, e.d)
}

func (e *stageTimeoutError) Unwrap() error { return ErrStageTimeout }

// Config controls the claudecode Adapter. The fields are split into
// adapter-process-wide settings (set once at daemon boot) and the rest
// of stage-specific knobs that come in via StageRequest at call time.
type Config struct {
	Binary     string        // path to `claude` CLI (or fake-claude under test)
	ExtraArgs  []string      // appended before per-stage flags (e.g., model, allowed tools)
	HiveBinary string        // absolute path to the hive binary (for mcp-stage-server spawn)
	RealHome   string        // real $HOME holding ~/.claude (skills source-of-truth)
	Classifier ClassifierSDK // optional fallback for verdict classification

	// Model selects the claude model. RunStage takes its model per-stage
	// from StageRequest.Model and ignores this; the ChatAgent provider has
	// no per-stage request, so it reads Config.Model. Empty = claude default.
	Model string

	// Scavenger is the lifecycle client. The adapter uses it only to
	// discover the per-project plugin directory; daemon lifecycle is
	// handled at a higher layer (internal/daemon). Nil = scavenger off.
	Scavenger        *scavenger.Client
	ScavengerEnabled bool
	ScavengerBinary  string

	// StallStore persists L1 / L2 stall detections. Nil = log-only.
	// The adapter still SIGTERMs the subprocess on L2 even with no store.
	StallStore StallStore

	// StallHeartbeatTimeout is the L1 threshold. Zero disables L1.
	StallHeartbeatTimeout time.Duration

	// StallToolCallTimeout is the L2 threshold. Zero disables L2.
	StallToolCallTimeout time.Duration

	// StallDiffStagnation: kill an IMPLEMENT-stage subprocess that produces no
	// new worktree content for this long. Zero disables.
	StallDiffStagnation time.Duration

	// StageVerdicts (optional) is the daemon-side registry that maps
	// (runID, stage) to the per-stage verdict.Listener. When non-nil,
	// the adapter registers its Listener at stage start so the HTTP
	// MCP route /mcp/stage/<run>/<stage> can dispatch
	// hive_submit_review_verdict directly into the same channel that
	// the UDS path uses.
	//
	// Optional: when nil, only the per-stage UDS works (current
	// behavior).
	StageVerdicts StageRegistrar

	// ApprovalsEnabled (Phase 4.5) routes every worker tool call through
	// Claude Code's --permission-prompt-tool to the hive_permission_check
	// MCP tool. When true the worker is launched WITHOUT
	// --dangerously-skip-permissions (the permission tool is the gate).
	// Spike: the tool always allows + logs the input it receives.
	ApprovalsEnabled bool

	// DaemonSocket is the daemon's RPC socket path. The permission tool
	// forwards approval.evaluate requests here (Phase 4.5). Empty when
	// approvals are off.
	DaemonSocket string

	// ApprovalTimeoutSeconds bounds how long the permission tool waits for
	// the daemon's decision (Phase 4.6 ask mode can block until an
	// operator responds). The tool's read deadline is this + a buffer.
	ApprovalTimeoutSeconds int

	// UseHTTPChat opts the chat mcp.json emission into HTTP transport
	// (Phase 6.3A toggle). When true, the chat MCP server is reached
	// via the daemon's long-running HTTP listener; stdio subprocess
	// is not spawned. Set per-smoke during Phase 6.3B; becomes
	// unconditional after the Phase 6.3C cutover (this flag goes
	// away then).
	UseHTTPChat bool

	// MCPURLPath is the absolute path of the daemon's mcp.url file
	// (e.g. ~/.hive/mcp.url). Read on each Send when UseHTTPChat is
	// true to populate the MCPBaseURL passed to WriteMCPConfig.
	MCPURLPath string
}

// StageRegistrar is the minimal subset of internal/verdict.StageRegistry
// the adapter needs. Defined here so the adapter doesn't accumulate
// daemon-side coupling for this single seam.
type StageRegistrar interface {
	Register(runID, stage string, l *verdict.Listener)
	Remove(runID, stage string)
}

// approvalsPromptNudge is appended to the worker system prompt when
// approvals are enabled. It steers the worker to the native file tools
// (which the default policy allows) and away from shell-based file
// writes (tee/cat/python heredocs, git apply/commit) that fail-closed
// gating denies — avoiding wasted iterations + L3 loop detections.
const approvalsPromptNudge = "\n\nIMPORTANT — tool-use is gated by an approval policy. " +
	"Use the native Read, Edit, Write, and MultiEdit tools for ALL file reading and editing. " +
	"Do NOT create or modify files via the shell (no `tee`/`cat` heredocs, no `python` file writes, " +
	"no `git apply`, no `git commit`) — such commands are DENIED. Reserve Bash for read-only " +
	"inspection and build/test commands (e.g. `go build`, `go test`, `git status`/`git diff`). " +
	"If a tool call is denied, switch to the native tool rather than retrying the same shell approach."

type Adapter struct {
	cfg Config

	// onWorkerStarted, onWorkerExited are the daemon-installed
	// (runID, pid) lifecycle hooks. RunStage currys them into the
	// (pid)-only SubprocessConfig.OnStarted/OnExited surface — the
	// adapter knows the RunID at call time but SubprocessConfig
	// doesn't. Set via SetWorkerCallbacks; nil = no-op.
	onWorkerStarted func(runID string, pid int) error
	onWorkerExited  func(runID string, pid int)
}

// New constructs an Adapter. Caller retains ownership of cfg.Classifier.
func New(cfg Config) *Adapter { return &Adapter{cfg: cfg} }

func (a *Adapter) Name() string { return "claude-code" }

func (a *Adapter) Close() error { return nil }

// SetStageVerdicts injects the daemon's StageRegistry after construction.
// Called by the composition root once the daemon is built (adapter is
// constructed before the daemon, so the registry isn't available at
// New time). Call once at composition time before any RunStage; not safe
// to call concurrently with RunStage (mutates Config without synchronization).
func (a *Adapter) SetStageVerdicts(r StageRegistrar) { a.cfg.StageVerdicts = r }

// SetWorkerCallbacks installs lifecycle hooks invoked when this adapter
// spawns a stage subprocess. Phase 7 restart-recovery uses them to
// stamp/clear runs.worker_pid so a daemon crash leaves a survivor list
// that the next boot can SIGKILL.
//
// Late-binding mirrors SetStageVerdicts — the daemon is constructed
// after the adapter, so the closures (which capture the store) can't
// be passed at New time. Call once at composition; not safe to call
// concurrently with RunStage.
//
// Either or both callbacks may be nil. If onStarted is nil, no stamp
// happens. If onExited is nil, no clear happens. If both are nil this
// is a no-op.
func (a *Adapter) SetWorkerCallbacks(
	onStarted func(runID string, pid int) error,
	onExited func(runID string, pid int),
) {
	a.onWorkerStarted = onStarted
	a.onWorkerExited = onExited
}

// ClassifyVerdict satisfies adapter.Adapter for the standalone
// classification path (e.g., re-classifying recorded reviewer text).
func (a *Adapter) ClassifyVerdict(ctx context.Context, text string) (*adapter.Verdict, error) {
	if a.cfg.Classifier == nil {
		return &adapter.Verdict{Kind: adapter.VerdictChangesRequested}, nil
	}
	return ClassifyText(ctx, a.cfg.Classifier, text)
}

// RunStage executes one stage of a Hive run end-to-end:
//  1. Materialize a HOME-redirected scope under StageDir/home
//     (skills allow-listed, credentials/settings symlinked).
//  2. If the stage demands a verdict tool, bind the per-stage UDS
//     verdict listener BEFORE spawning the worker (spec §5.6 race fix)
//     and write the matching .mcp.json file that the worker will load
//     via `claude -p --mcp-config`.
//  3. Spawn the worker with an allow-listed environment (HOME pointed
//     at the redirected stage home; only PATH/USER/LOGNAME/LANG/LC_*/TZ
//     and HIVE_*/ANTHROPIC_* pass through).
//  4. After the worker exits, drain the verdict listener (non-blocking
//     since the worker either tool-called before exit or never will).
//     If the listener has no frame and a Classifier is configured, fall
//     back to text classification on the concatenated assistant text.
//
// nonEmptyPrompt guards the `claude --print` positional prompt. The CLI rejects
// an empty prompt ("Input must be provided either through stdin or as a prompt
// argument when using --print" — claude >= 2.1.161), and the stage's real
// instructions live in --append-system-prompt anyway. Fall back to a minimal
// directive so a caller with no user prompt still runs instead of crashing with
// a cryptic CLI error.
func nonEmptyPrompt(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Proceed with the task described in the system prompt."
	}
	return s
}

func (a *Adapter) RunStage(ctx context.Context, req adapter.StageRequest) (*adapter.StageOutput, error) {
	out := &adapter.StageOutput{StartedAt: time.Now()}
	defer func() { out.EndedAt = time.Now() }()

	if req.StageDir == "" {
		return out, fmt.Errorf("StageRequest.StageDir is required")
	}
	if err := os.MkdirAll(req.StageDir, 0700); err != nil {
		return out, fmt.Errorf("mkdir stage dir: %w", err)
	}

	scope, err := MaterializeScope(ScopeRequest{
		StageDir: req.StageDir, RealHome: a.cfg.RealHome, Skills: req.Skills,
		RestrictPermissions: a.cfg.ApprovalsEnabled,
	})
	if err != nil {
		return out, fmt.Errorf("scope: %w", err)
	}

	// Bind the verdict UDS listener if the stage has a verdict tool
	// (currently only the review stage). Implement stages skip this.
	var listener *verdict.Listener
	var verdictSock string
	if req.VerdictToolName != "" {
		verdictSock = filepath.Join(req.StageDir, "verdict.sock")
		l, err := verdict.Listen(verdictSock)
		if err != nil {
			return out, fmt.Errorf("verdict listen: %w", err)
		}
		defer l.Close()
		listener = l
		if a.cfg.StageVerdicts != nil {
			a.cfg.StageVerdicts.Register(req.RunID, req.StageName, listener)
			defer a.cfg.StageVerdicts.Remove(req.RunID, req.StageName)
		}
	}

	// Write mcp.json if EITHER the stage has a verdict tool OR
	// scavenger is enabled (so implement stages with scavenger still
	// get scavenger MCP tools available, not just the hooks).
	mcpConfigPath := ""
	if req.VerdictToolName != "" || a.cfg.ScavengerEnabled || a.cfg.ApprovalsEnabled || req.DocToolName != "" {
		path, err := WriteMCPConfig(MCPConfigRequest{
			DestDir:                req.StageDir,
			HiveBinary:             a.cfg.HiveBinary,
			NotifySock:             verdictSock,
			RunID:                  req.RunID,
			StageName:              req.StageName,
			ToolName:               req.VerdictToolName,
			ScavengerEnabled:       a.cfg.ScavengerEnabled,
			ScavengerBinary:        a.cfg.ScavengerBinary,
			ApprovalsEnabled:       a.cfg.ApprovalsEnabled,
			DaemonSocket:           a.cfg.DaemonSocket,
			ApprovalTimeoutSeconds: a.cfg.ApprovalTimeoutSeconds,
			DocToolName:            req.DocToolName,
		})
		if err != nil {
			return out, fmt.Errorf("mcp config: %w", err)
		}
		mcpConfigPath = path
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	// Canonical claude -p invocation per Phase 0 spike-2:
	//   claude -p "<prompt>" --output-format stream-json --verbose [...]
	// ExtraArgs come first so test fixtures (e.g. -fixture for fake-claude)
	// can be injected without conflicting with the real-claude flag set.
	// --dangerously-skip-permissions: Claude Code's default permission gate
	// blocks MCP tools (incl. hive_submit_review_verdict) in non-interactive
	// mode. Autonomous orchestration requires bypassing — the daemon IS the
	// authorizing principal, and each stage runs in its own HOME-redirect.
	args := append([]string{}, a.cfg.ExtraArgs...)
	args = append(args, "--output-format", "stream-json", "--verbose")
	// Phase 4.5: with approvals on, the permission-prompt-tool is the
	// gate — do NOT skip permissions (that would bypass the tool too).
	// Otherwise keep the historical bypass so MCP tools don't stall.
	if a.cfg.ApprovalsEnabled {
		args = append(args, "--permission-mode", "default",
			"--permission-prompt-tool", "mcp__hive_perm__hive_permission_check")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	systemPrompt := req.SystemPrompt
	if a.cfg.ApprovalsEnabled {
		// Phase 4.5: tool-use is gated. Nudge the worker toward the native
		// file tools so it doesn't burn iterations (and trip the L3 loop
		// detector) trying shell-based file writes that the gate denies.
		systemPrompt += approvalsPromptNudge
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	// Scavenger plugin (per-run model): the worktree already holds its own
	// .scavenger/claude-plugin/ (created by the daemon's per-run
	// InstallPlugin before the pipeline started). Source the plugin from
	// there, patch it (strip SessionEnd so per-stage sessions don't kill
	// the per-run daemon), and pass --plugin-dir. No symlink, no dependency
	// on the canonical repo being scavenger-init-ed.
	if a.cfg.ScavengerEnabled && req.Cwd != "" {
		srcPluginDir := scavengerPluginSource(req.Cwd)
		if _, err := os.Stat(filepath.Join(srcPluginDir, ".claude-plugin", "plugin.json")); err == nil {
			pluginDir := srcPluginDir
			if req.RunDir != "" {
				patched, perr := WritePluginDir(srcPluginDir, req.RunDir)
				if perr != nil {
					return out, fmt.Errorf("claudecode: prepare plugin dir for run %s: %w", req.RunID, perr)
				}
				pluginDir = patched
			}
			args = append(args, "--plugin-dir", pluginDir)
		}
	}
	if mcpConfigPath != "" {
		// --strict-mcp-config: only use the servers Hive wrote into
		// mcpConfigPath. Without it, claude ALSO loads the user's global MCP
		// servers (~/.claude.json — Gmail/Calendar/Drive, etc.), leaking
		// unrelated tools into the autonomous worker (a security + iteration-
		// noise hazard — reviewers waste turns on tools they shouldn't have).
		args = append(args, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
	}
	args = append(args, "-p", nonEmptyPrompt(req.UserPrompt))

	// Phase 3.2: construct stall monitor when either threshold is set.
	// The monitor watches the event stream via the subprocess OnEvent
	// hook and SIGTERMs on L2 detections via the Signaller (set after
	// the subprocess is built).
	var monitor *Monitor
	if a.cfg.StallHeartbeatTimeout > 0 || a.cfg.StallToolCallTimeout > 0 {
		monitor = NewMonitor(MonitorConfig{
			RunID:            req.RunID,
			StageID:          req.StageID,
			HeartbeatTimeout: a.cfg.StallHeartbeatTimeout,
			ToolCallTimeout:  a.cfg.StallToolCallTimeout,
			Store:            a.cfg.StallStore,
		})
	}

	subCfg := SubprocessConfig{
		Binary: a.cfg.Binary,
		Args:   args,
		Env:    buildWorkerEnv(scope.StageHome),
		Cwd:    req.Cwd,
	}
	if monitor != nil {
		subCfg.OnEvent = monitor.OnEvent
	}
	// Phase 7 restart-recovery: curry adapter-level (runID, pid)
	// callbacks into the subprocess-level (pid)-only signature. The
	// pipeline doesn't construct SubprocessConfig directly — it goes
	// through this adapter, which is where we know req.RunID.
	if a.onWorkerStarted != nil {
		subCfg.OnStarted = func(pid int) error {
			return a.onWorkerStarted(req.RunID, pid)
		}
	}
	if a.onWorkerExited != nil {
		subCfg.OnExited = func(pid int) {
			a.onWorkerExited(req.RunID, pid)
		}
	}
	sub := NewSubprocess(subCfg)
	if monitor != nil {
		monitor.SetSignaller(sub)
		monCtx, monCancel := context.WithCancel(ctx)
		defer monCancel()
		go monitor.Run(monCtx)
	}
	// Phase: diff-stagnation (L4). Only the implement stage, and only when
	// configured. Polls the worktree progress hash; if it shows no NEW content
	// for StallDiffStagnation, SIGTERMs the subprocess (caught below via the
	// diffStalled flag) — catches a stuck/looping agent that keeps making tool
	// calls but produces no code, before the 20m stage timeout.
	var diffStalled atomic.Bool
	if req.StageName == "implement" && a.cfg.StallDiffStagnation > 0 {
		poll := a.cfg.StallDiffStagnation / 4
		if poll < 30*time.Second {
			poll = 30 * time.Second
		}
		if poll > 60*time.Second {
			poll = 60 * time.Second
		}
		dsCtx, dsCancel := context.WithCancel(ctx)
		defer dsCancel()
		go watchDiffStagnation(dsCtx, req.Cwd, poll, a.cfg.StallDiffStagnation, sub, &diffStalled)
	}
	res, runErr := sub.Run(ctx)
	if res != nil {
		out.Stderr = res.Stderr
		out.ExitCode = res.ExitCode
		out.RawEvents = []byte(rawEventsDump(res.Events))
		if err := os.WriteFile(filepath.Join(req.StageDir, "events.jsonl"), out.RawEvents, 0600); err != nil {
			log.Printf("claudecode: write events.jsonl: %v", err)
		}
		if err := os.WriteFile(filepath.Join(req.StageDir, "stderr.log"), []byte(out.Stderr), 0600); err != nil {
			log.Printf("claudecode: write stderr.log: %v", err)
		}
		// Phase 3.1: extract token usage. claude emits usage on the result
		// event (and sometimes on intermediate events). Take the last non-zero
		// reading — that's typically the cumulative count from the result event.
		for _, ev := range res.Events {
			if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 || ev.Usage.CacheReadTokens > 0 {
				out.Tokens = adapter.TokenUsage{
					Input:    ev.Usage.InputTokens,
					Output:   ev.Usage.OutputTokens,
					CacheHit: ev.Usage.CacheReadTokens,
				}
			}
		}
		// Phase 3.1: reconstruct tool calls by pairing tool_use + tool_result
		// events on ToolID. Wall-clock timestamps here are coarse (same `now`
		// for all events from one subprocess result); good enough for 3.1's
		// per-stage aggregation. Phase 3.2's stall monitor will refactor
		// parseJSONL to capture per-event timestamps when needed.
		//
		// Two shapes are supported:
		//   Shape 1 — top-level events (fake-claude fixtures, legacy format):
		//     {"type":"tool_use","id":"...","name":"...","input":{...}}
		//     {"type":"tool_result","tool_use_id":"...","is_error":false}
		//   Shape 2 — nested in message.content (real claude stream-json):
		//     {"type":"assistant","message":{"content":[{"type":"tool_use",...}]}}
		//     {"type":"user","message":{"content":[{"type":"tool_result",...}]}}
		type pending struct {
			name      string
			argsJSON  json.RawMessage
			startedAt time.Time
		}
		inflight := map[string]pending{}
		now := time.Now()
		recordUse := func(id, name string, input json.RawMessage) {
			if id == "" {
				// Defensive: an empty ID would collide with other empty-ID
				// calls and confuse the matching. Drop with no record.
				return
			}
			inflight[id] = pending{name: name, argsJSON: input, startedAt: now}
		}
		recordResult := func(useID string, isError bool) {
			if p, ok := inflight[useID]; ok {
				out.ToolCalls = append(out.ToolCalls, adapter.ToolCallRecord{
					Name:      p.name,
					ArgsJSON:  p.argsJSON,
					StartedAt: p.startedAt,
					EndedAt:   now,
					Success:   !isError,
				})
				delete(inflight, useID)
			}
		}
		for _, ev := range res.Events {
			// Shape 1: top-level tool_use / tool_result (fake-claude fixtures).
			switch ev.Type {
			case EventToolUse:
				recordUse(ev.ToolID, ev.ToolName, ev.ToolInput)
			case EventToolResult:
				recordResult(ev.ToolUseID, ev.IsError)
			}
			// Shape 2: nested in assistant/user message.content (real claude).
			// We don't gate on ev.Type so a future event type that also carries
			// tool blocks still works; the inner type check is what matters.
			for _, block := range ev.Message.Content {
				switch block.Type {
				case "tool_use":
					recordUse(block.ID, block.Name, block.Input)
				case "tool_result":
					recordResult(block.ToolUseID, block.IsError)
				}
			}
		}
		// Any leftover inflight calls (tool_use without tool_result) are
		// orphaned — could be a crash or a still-running call. Record them
		// with EndedAt zero and Success false so the pipeline doesn't lose
		// them.
		for _, p := range inflight {
			out.ToolCalls = append(out.ToolCalls, adapter.ToolCallRecord{
				Name:      p.name,
				ArgsJSON:  p.argsJSON,
				StartedAt: p.startedAt,
				// EndedAt zero; Success false (zero values).
			})
		}
	}
	// Phase 3.2: if the monitor fired L2, the subprocess was SIGTERM'd
	// from inside Run; cmd.Wait returned an error. Translate to the
	// typed sentinel so executePipeline can route to needs_attention
	// with the offending tool name in the summary. Comes BEFORE the
	// generic runErr propagation so the tool-name context isn't lost.
	if monitor != nil && monitor.Killed() {
		tool, _ := monitor.CulpritTool()
		return out, errToolCallStallWith(tool)
	}
	if runErr != nil {
		// Diff-stagnation kill (our SIGTERM from watchDiffStagnation). It's our
		// own kill so it precedes the generic propagation; placed BEFORE the
		// timeout check because a stagnation SIGTERM may leave ctx fine (it does
		// not cancel the timeout context).
		if diffStalled.Load() {
			return out, &implementStagnationError{d: a.cfg.StallDiffStagnation}
		}
		// A subprocess kill from OUR stage-timeout deadline (req.Timeout) surfaces
		// from cmd.Wait as a raw "signal: killed". Translate it to the typed
		// timeout error so executePipeline reports "implement timed out after 20m"
		// rather than a cryptic SIGKILL. The monitor's L2 SIGTERM is handled just
		// above; an abandon cancels the PARENT ctx → Canceled (not DeadlineExceeded),
		// so this only fires for the timeout we set.
		if req.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, &stageTimeoutError{stage: req.StageName, d: req.Timeout}
		}
		return out, runErr
	}

	if listener != nil {
		if v, err := drainListener(listener); v != nil || err != nil {
			out.Verdict = v
			return out, err
		}
	}
	if a.cfg.Classifier != nil {
		text := lastAssistantText(res)
		if text == "" {
			out.Verdict = &adapter.Verdict{Kind: adapter.VerdictChangesRequested}
		} else {
			v, _ := ClassifyText(ctx, a.cfg.Classifier, text)
			out.Verdict = v
		}
	} else if req.VerdictToolName != "" {
		out.Verdict = &adapter.Verdict{Kind: adapter.VerdictChangesRequested}
	}
	return out, nil
}

// drainListener reads one event from the verdict listener with a 50ms
// bounded wait. The timeout absorbs the goroutine-scheduling window
// between the worker's writeAck return and the listener goroutine
// publishing to the channel — normally microseconds but non-zero.
//
// Returns (nil, nil) when no event arrives within the window, meaning
// the worker exited without calling the verdict tool. Caller should
// fall through to classifier / pessimistic default.
func drainListener(l *verdict.Listener) (*adapter.Verdict, error) {
	select {
	case f := <-l.Frames():
		return &adapter.Verdict{
			Kind:       adapter.VerdictKind(f.Verdict),
			Confidence: f.Confidence,
			FileRefs:   f.FileRefs,
			Summary:    f.Summary,
			FromTool:   true,
		}, nil
	case ack := <-l.Rejections():
		if ack.Error == verdict.AckErrFileRefsMissing {
			return nil, verdict.ErrFileRefsMissing
		}
		return nil, fmt.Errorf("verdict rejected: %s", ack.Error)
	case <-time.After(50 * time.Millisecond):
		// Worker exited without the listener-side goroutine publishing
		// a frame or rejection in the window. Treat as "no tool call"
		// and fall through to classifier / pessimistic default below.
		return nil, nil
	}
}

// buildWorkerEnv constructs the worker process environment per spec
// §5.4: HOME is forced to the stage-scoped home; only a tight set of
// system vars pass through; HIVE_* and ANTHROPIC_* are forwarded so
// the worker can pick up credentials and config without exposing the
// caller's full env.
func buildWorkerEnv(stageHome string) []string {
	env := []string{"HOME=" + stageHome}
	for _, k := range []string{"PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HIVE_") || strings.HasPrefix(kv, "ANTHROPIC_") {
			env = append(env, kv)
		}
	}
	return env
}

// lastAssistantText concatenates assistant text events into a single
// blob for fallback classification. Both `text` and `delta` fields are
// included because claude -p emits them on different event subtypes.
func lastAssistantText(res *SubprocessResult) string {
	if res == nil {
		return ""
	}
	var sb strings.Builder
	for _, ev := range res.Events {
		if ev.Type == EventText {
			if ev.Text != "" {
				sb.WriteString(ev.Text)
			}
			if ev.Delta != "" {
				sb.WriteString(ev.Delta)
			}
		}
	}
	return sb.String()
}

// rawEventsDump joins captured event raw lines back into a single
// newline-delimited buffer suitable for archival in StageOutput.RawEvents.
func rawEventsDump(events []Event) string {
	var sb strings.Builder
	for _, ev := range events {
		if len(ev.Raw) > 0 {
			sb.Write(ev.Raw)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// scavengerPluginSource returns the worktree-local scavenger plugin dir.
func scavengerPluginSource(worktree string) string {
	return filepath.Join(worktree, ".scavenger", "claude-plugin")
}
