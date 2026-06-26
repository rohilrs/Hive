// Package daemon is Hive's long-running orchestrator process. Owns the
// scheduler tick loop, the RPC server (Unix socket), the SQLite store,
// and the pipeline engine. Depends on adapter.Adapter (spec §5.3) — the
// concrete provider is chosen by the caller.
package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/approval"
	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/conflict"
	"github.com/rohilrs/Hive/internal/daemon/eventbus"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/mcphttp"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/sources"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
	"github.com/rohilrs/Hive/internal/worktree"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// predictorIface is the predictor-facing subset that dispatch needs.
// *predictor.Predictor satisfies this interface; tests supply fakes.
type predictorIface interface {
	Predict(ctx context.Context, task, repoRoot, bundleDir string) (*predictor.Result, error)
}

// ScavengerLifecycle is the per-worktree subset of *scavenger.Client the
// daemon uses. nil = no scavenger management.
type ScavengerLifecycle interface {
	IndexWorktree(ctx context.Context, wtPath string) error
	InstallPlugin(ctx context.Context, wtPath string) error
	StartDaemon(ctx context.Context, wtPath string) error
	StopDaemon(wtPath string) error
}

// Config bundles the inputs the daemon needs at construction time. The
// adapter is required and chosen by the caller (composition root in
// cmd_daemon.go); the daemon itself stays provider-agnostic.
type Config struct {
	HiveDir   string
	Cfg       *config.Config
	Adapter   adapter.Adapter
	Scavenger ScavengerLifecycle // optional: nil = no lifecycle management
	// Predictor is the optional pre-dispatch predictor. nil means the
	// scheduler dispatches without prediction (pre-2b.3 behavior).
	// Accepts *predictor.Predictor or any test fake implementing predictorIface.
	Predictor predictorIface
	// Guard is the in-memory conflict guard. nil disables conflict
	// checking; the scheduler dispatches everything concurrently
	// (predictor still runs if enabled; just no enforcement).
	Guard *conflict.Guard

	// Store is the optional *store.Store. When nil, daemon.New opens
	// one at HiveDir/db.sqlite (pre-3.2 behavior). When supplied, the
	// daemon uses the provided store directly. The composition-root
	// pattern (cmd/hive) opens the store once and shares it across
	// adapter (for stall persistence) and daemon.
	Store *store.Store

	// LoopDetector is the optional pipeline.LoopDetector implementation
	// for L3 loop detection (Phase 3.3). nil disables L3 (build
	// pipeline still runs, but never escalates / marks needs_attention
	// for loops).
	LoopDetector pipeline.LoopDetector
}

// Daemon is the long-running orchestrator. Constructed via New, started
// with Start, and shut down via Stop. Start blocks until ctx is canceled.
type Daemon struct {
	cfg           Config
	store         *store.Store
	scavLifecycle ScavengerLifecycle
	approver      approval.Engine
	wtMgr         *worktree.Manager
	adp           adapter.Adapter
	predictor     predictorIface
	guard         *conflict.Guard
	pipelines     map[string]pipeline.Pipeline
	sources       map[string]sources.Source

	// linearWriter performs Hive->Linear write-back mutations (Phase 1).
	// Set via SetLinearWriter at the composition root; nil disables write-back.
	linearWriter linearIssueWriter

	// chatAgent is the provider-specific chat backend (Phase 6.1). It is
	// injected via SetChatAgent at the composition root; New() does NOT set
	// it yet (composition-root wiring lands in a later task). streamChat
	// errors out if it is nil.
	// TODO(6.1): wire SetChatAgent from cmd/hive/cmd_daemon based on
	// cfg.Chat.Provider ("api" -> SDK agent, "claude-code" -> CC agent).
	chatAgent chat.Agent

	// plannerAgentFor builds a per-session planner agent (Phase 8.A T6).
	// streamChat calls this when the session's kind is "plan" to get a
	// fresh agent with the planner registry + planner system prompt +
	// ForceSonnet router, scoped to the bound project's repo_path.
	// Composition root sets it via SetPlannerAgentFor. Returning a nil
	// agent + error makes streamChat surface a clear "planner unavailable"
	// frame rather than panicking.
	plannerAgentFor func(slug, cwd string) (chat.Agent, error)

	// decomposeRunner is the anthropic Runner used by handleDecompose
	// (Phase 7 hive decompose). Production wires *anthropic.SDK in
	// cmd_daemon.go when ANTHROPIC_API_KEY is set; tests inject stubs
	// by setting this field directly. nil → handleDecompose returns a
	// clear error so a missing key doesn't fail daemon startup.
	decomposeRunner decompose.Runner

	// graduateRunner is the anthropic Runner used by runGraduate's Stage-4
	// completion audit (hive project graduate). Production wires the oneshot
	// roaming-tool runner in cmd_daemon.go; tests inject stubs via
	// SetGraduateRunner. nil → graduate.Audit returns a clear error.
	graduateRunner graduate.Runner

	// decomposeJobs tracks in-flight async roadmap.decompose runs (dedup +
	// lifecycle). Results stream to clients via the event bus, not this map.
	decomposeJobs *decomposeJobs

	// mergeGuard serializes PR merges per feature branch (the merge queue).
	mergeGuard *mergeGuard

	// mergeAttempts is an in-memory per-task merge-attempt counter (the circuit
	// breaker): after mergeAttemptCap failed dispatches checkOneMerge parks the
	// task terminally at merge_failed instead of re-queuing forever.
	mergeAttempts *mergeAttemptTracker

	// graduateInFlight ensures at most one `project.graduate` runs per project at
	// a time. Two concurrent graduations of the same project collide on the
	// feature-branch worktree (`git worktree add -B <feature>` fails when the
	// branch is already checked out by the first run). Keyed on project slug.
	graduateInFlight *mergeGuard

	// kickMerge is a buffered (cap 1) low-latency wake-up for the merge queue.
	// kickMergeQueue() does a non-blocking send; reconcileLoop selects on it and
	// runs a detectMerges pass immediately instead of waiting the 30s poll. The
	// 30s ticker remains the backstop, so a dropped kick (buffer full) is benign.
	kickMerge chan struct{}

	// groundFetchAt throttles the planner grounder's best-effort `git fetch` of
	// an origin-tracking branch ("repo\x00branch" -> last fetch time). The grounder
	// is rebuilt per planner tool-call, so an unthrottled fetch would hit the
	// network every turn; this caps it to once per groundFetchThrottle per branch.
	groundFetchAt sync.Map

	// Phase 5.3: per-source last-sync state (timestamps + last result),
	// populated by Sync (manual or polled by syncLoop). Field named
	// syncStatus (not "sync") to avoid shadowing the stdlib sync package.
	syncStatus syncState

	// prGateway abstracts `gh` PR ops for the merge-poller (Phase 3).
	// New() defaults it to ghPRGateway{}; tests override the field.
	prGateway prGateway

	sock      string
	pidPath   string
	pidLock   *os.File // flock-held file; nil before Start, set after acquireSingletonLock
	listener  net.Listener
	scheduler *Scheduler
	rpc       *RPCServer
	bus       *eventbus.Bus // Phase 3.5a: in-memory pub/sub

	// Phase 3.7: per-run cancel functions for run.abandon. Pipeline
	// goroutines register their cancel on entry + deregister on exit;
	// abandon handler looks up + calls.
	runCancelsMu sync.Mutex
	runCancels   map[string]context.CancelFunc

	// Phase 4.6: pending approvals awaiting an operator decision (ask
	// mode). approval.evaluate registers an entry + blocks; the TUI's
	// approval.resolve sends the decision. The metadata lets daemon.status
	// list pending approvals so a (re)subscribing TUI can hydrate them
	// (the approval.requested event may have fired before it connected).
	pendingMu        sync.Mutex
	pendingApprovals map[string]*pendingApproval

	// Phase 6.1b: pending chat confirms awaiting a chat.confirm RPC.
	// daemonConfirmGate registers an entry + blocks; handleChatConfirm
	// resolves it. Mirrors pendingApprovals but carries confirmDecision.
	pendingConfirms   map[string]chan confirmDecision
	pendingConfirmsMu sync.Mutex

	// chatConfirmGateForTool overrides the gate constructed by
	// handleChatTool when non-nil. Production leaves this nil; tests
	// inject a fake to assert deny/approve content shapes.
	chatConfirmGateForTool chat.ConfirmGate

	// chatRegistryForTest overrides the registry returned by
	// handleChatTool when non-nil. Production leaves this nil; tests
	// inject a pre-populated registry with custom handlers.
	chatRegistryForTest *chat.Registry

	// chatStreams maps session_id → active chat.send conn so the confirm
	// gate can emit tool_proposed frames on the right stream.
	chatStreams   map[string]any
	chatStreamsMu sync.Mutex

	// Phase 6.3: per-stage verdict listener registry. The HTTP MCP
	// /mcp/stage/<run>/<stage> route dispatches hive_submit_review_verdict
	// in-process; adapters register/remove via StageVerdicts().
	stageVerdicts *verdict.StageRegistry

	// mcpServer is the long-running MCP-over-HTTP server (Phase 6.3).
	// Bound in Start(), stopped in Stop().
	mcpServer   *http.Server
	mcpURL      string // base URL written to ~/.hive/mcp.url
	mcpHTTPAddr string // bound listener addr (host:port) for doctor.health; "" if not bound

	// Phase 7 (doctor): startedAt is stamped in New so doctor.health can
	// compute uptime without re-reading the pidfile.
	startedAt time.Time

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once

	// ready is closed by Start exactly once, after all daemon fields are
	// wired and before Start blocks on ctx.Done(). It makes the documented
	// "callers must serialize Stop after Start" contract observable so a
	// caller (esp. a test running Start in a goroutine) can wait for the
	// daemon to be fully wired before reading its fields or calling Stop.
	ready chan struct{}
}

// SetChatAgent injects the chat backend used by streamChat. The composition
// root selects the provider (api/claude-code) and calls this after New().
func (d *Daemon) SetChatAgent(a chat.Agent) { d.chatAgent = a }

// SetPlannerAgentFor injects the planner agent factory (Phase 8.A T6).
// streamChat calls f(slug, cwd) when the session.Kind is "plan", binding
// the planner registry's read/write tools to the project's repo_path so
// each session reads + writes into its own docs/superpowers/ tree.
// Composition root supplies this; tests may inject a stub directly.
func (d *Daemon) SetPlannerAgentFor(f func(slug, cwd string) (chat.Agent, error)) {
	d.plannerAgentFor = f
}

// SetDecomposeRunner installs the anthropic Runner used by handleDecompose.
// Wired post-New (late-binding, mirroring SetStageVerdicts) so the daemon
// boots cleanly even when ANTHROPIC_API_KEY is unset; handleDecompose then
// returns a clear error per-request.
func (d *Daemon) SetDecomposeRunner(r decompose.Runner) { d.decomposeRunner = r }

// SetGraduateRunner installs the roaming-tool Runner used by runGraduate's
// Stage-4 completion audit. Wired post-New like SetDecomposeRunner.
func (d *Daemon) SetGraduateRunner(r graduate.Runner) { d.graduateRunner = r }

// SetLinearWriter wires the Hive->Linear write-back writer (composition root).
// nil disables write-back. Phase 1.
func (d *Daemon) SetLinearWriter(w linearIssueWriter) { d.linearWriter = w }

// New constructs a Daemon. The adapter is required. If HiveDir is empty
// it defaults to $HOME/.hive. The on-disk layout (worktrees/, logs/,
// db.sqlite, daemon.sock) is created here.
func New(cfg Config) (*Daemon, error) {
	if cfg.Adapter == nil {
		return nil, fmt.Errorf("daemon.Config.Adapter is required")
	}
	if cfg.Cfg == nil {
		return nil, fmt.Errorf("daemon.Config.Cfg is required")
	}
	if cfg.HiveDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		cfg.HiveDir = filepath.Join(home, ".hive")
	}

	for _, sub := range []string{"worktrees", "logs", ""} {
		if err := os.MkdirAll(filepath.Join(cfg.HiveDir, sub), 0700); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}

	var s *store.Store
	if cfg.Store != nil {
		s = cfg.Store
	} else {
		var err error
		s, err = store.Open(context.Background(), filepath.Join(cfg.HiveDir, "db.sqlite"))
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
	}

	bus := eventbus.New(eventbus.Config{
		BufferSize:       cfg.Cfg.TUI.EventBufferSize,
		ResyncOnOverflow: cfg.Cfg.TUI.ResyncOnOverflow,
	})

	d := &Daemon{
		cfg:           cfg,
		store:         s,
		startedAt:     time.Now(),
		scavLifecycle: cfg.Scavenger,
		approver:      pickApprover(cfg, s),
		wtMgr: worktree.NewManager(worktree.Config{
			WorktreeRoot: filepath.Join(cfg.HiveDir, "worktrees"),
		}),
		adp:              cfg.Adapter,
		predictor:        cfg.Predictor,
		guard:            cfg.Guard,
		pipelines:        map[string]pipeline.Pipeline{},
		sources:          map[string]sources.Source{},
		syncStatus:       syncState{lastSync: map[string]time.Time{}, lastResult: map[string]*SourceResult{}},
		sock:             filepath.Join(cfg.HiveDir, "daemon.sock"),
		pidPath:          filepath.Join(cfg.HiveDir, "daemon.pid"),
		bus:              bus,
		runCancels:       map[string]context.CancelFunc{},
		pendingApprovals: map[string]*pendingApproval{},
		pendingConfirms:  map[string]chan confirmDecision{},
		chatStreams:      map[string]any{},
		stageVerdicts:    verdict.NewStageRegistry(),
		prGateway:        ghPRGateway{},
		decomposeJobs:    newDecomposeJobs(),
		mergeGuard:       newMergeGuard(),
		mergeAttempts:    newMergeAttemptTracker(),
		graduateInFlight: newMergeGuard(),
		kickMerge:        make(chan struct{}, 1),
		ready:            make(chan struct{}),
	}

	d.pipelines["build"] = &pipeline.BuildPipeline{
		Adapter:  d.adp,
		Feedback: feedbackAdapter{S: s},
		Stages:   newStageAdapter(s),
		Stalls:   newPipelineStallAdapter(s, bus),
		Loop:     cfg.LoopDetector,
		Events:   newBusPublisher(bus),
		Cfg: pipeline.BuildConfig{
			MaxIterations: cfg.Cfg.Pipelines.Build.MaxIterations,
			Ladder: pipeline.ModelLadder{
				Worker:   cfg.Cfg.Models.WorkerLadder,
				Reviewer: cfg.Cfg.Models.ReviewerLadder,
			},
			StageTimeout:            time.Duration(cfg.Cfg.Pipelines.Build.StageTimeoutMinutes) * time.Minute,
			SkillsImpl:              []string{"implement"},
			SkillsReview:            []string{"review-code"},
			LoopCheckAfterIter:      cfg.Cfg.StallDetection.LoopCheckAfterIter,
			LoopSimilarityThreshold: cfg.Cfg.StallDetection.LoopSimilarityThreshold,
			LoopDiffBase:            "main",
			TestCommand:             cfg.Cfg.Pipelines.Build.TestCommand,
			ValidateCommand:         cfg.Cfg.Pipelines.Build.ValidateCommand,
			TestStageTimeout:        time.Duration(cfg.Cfg.Pipelines.Build.TestStageTimeoutMinutes) * time.Minute,
			ValidateStageTimeout:    time.Duration(cfg.Cfg.Pipelines.Build.ValidateStageTimeoutMinutes) * time.Minute,
			DocumenterEnabled:       cfg.Cfg.Pipelines.Build.Documenter.Enabled,
			DocumenterModel:         cfg.Cfg.Pipelines.Build.Documenter.Model,
			DocumenterTimeout:       time.Duration(cfg.Cfg.Pipelines.Build.Documenter.StageTimeoutMinutes) * time.Minute,
			DocumenterSkills:        cfg.Cfg.Pipelines.Build.Documenter.SkillsToLoad,
			DocumenterUpdateReadme:  cfg.Cfg.Pipelines.Build.Documenter.UpdateReadme,
			DocumenterCodeComments:  cfg.Cfg.Pipelines.Build.Documenter.CodeComments,
		},
	}

	d.pipelines["plan"] = &pipeline.PlanPipeline{
		Adapter:  d.adp,
		Feedback: feedbackAdapter{S: s},
		Stages:   newStageAdapter(s),
		Events:   newBusPublisher(bus),
		Cfg: pipeline.PlanConfig{
			MaxIterations: cfg.Cfg.Pipelines.Plan.MaxIterations,
			Ladder: pipeline.ModelLadder{
				Worker:   cfg.Cfg.Models.WorkerLadder,
				Reviewer: cfg.Cfg.Models.ReviewerLadder,
			},
			StageTimeout:     time.Duration(cfg.Cfg.Pipelines.Build.StageTimeoutMinutes) * time.Minute,
			SkillsBrainstorm: []string{"brainstorming"},
			SkillsSpec:       []string{"writing-plans"},
			SkillsReview:     []string{"review-code"},
			SkillsPlan:       []string{"writing-plans"},
		},
	}

	d.pipelines["debug"] = &pipeline.DebugPipeline{
		Adapter:  d.adp,
		Feedback: feedbackAdapter{S: s},
		Stages:   newStageAdapter(s),
		Events:   newBusPublisher(bus),
		Cfg: pipeline.DebugConfig{
			MaxIterations: cfg.Cfg.Pipelines.Debug.MaxIterations,
			Ladder: pipeline.ModelLadder{
				Worker:   cfg.Cfg.Models.WorkerLadder,
				Reviewer: cfg.Cfg.Models.ReviewerLadder,
			},
			StageTimeout:       time.Duration(cfg.Cfg.Pipelines.Build.StageTimeoutMinutes) * time.Minute,
			VerifyCommand:      cfg.Cfg.Pipelines.Build.TestCommand,
			VerifyStageTimeout: time.Duration(cfg.Cfg.Pipelines.Build.TestStageTimeoutMinutes) * time.Minute,
			SkillsReproduce:    []string{"implement"},
			SkillsIsolate:      []string{"debug"},
			SkillsFix:          []string{"implement"},
		},
	}

	d.pipelines["finish-branch"] = &pipeline.FinishBranchPipeline{
		Stages: newStageAdapter(s),
		Events: newBusPublisher(bus),
		Fixer:  &childRunner{d: d},
		Cfg: pipeline.FinishBranchConfig{
			FormatCommand:       cfg.Cfg.Pipelines.FinishBranch.FormatCommand,
			TypecheckCommand:    cfg.Cfg.Pipelines.FinishBranch.TypecheckCommand,
			LintCommand:         cfg.Cfg.Pipelines.FinishBranch.LintCommand,
			TestCommand:         cfg.Cfg.Pipelines.FinishBranch.TestCommand,
			CreatePRCommand:     cfg.Cfg.Pipelines.FinishBranch.CreatePRCommand,
			CIMonitorCommand:    cfg.Cfg.Pipelines.FinishBranch.CIMonitorCommand,
			StageTimeout:        time.Duration(cfg.Cfg.Pipelines.FinishBranch.StageTimeoutMinutes) * time.Minute,
			CIMonitorTimeout:    time.Duration(cfg.Cfg.Pipelines.FinishBranch.CIMonitorTimeoutMinutes) * time.Minute,
			ShellOutputMaxBytes: cfg.Cfg.Pipelines.FinishBranch.ShellOutputMaxBytes,
			MaxFixAttempts:      cfg.Cfg.Pipelines.FinishBranch.MaxFixAttempts,
		},
	}

	d.pipelines["resolve"] = &pipeline.ResolvePipeline{
		Adapter:  d.adp,
		Feedback: feedbackAdapter{S: s},
		Stages:   newStageAdapter(s),
		Events:   newBusPublisher(bus),
		Cfg: pipeline.ResolveConfig{
			MaxIterations:   cfg.Cfg.Pipelines.Resolve.MaxIterations,
			StageTimeout:    time.Duration(cfg.Cfg.Pipelines.Resolve.StageTimeoutMinutes) * time.Minute,
			TestCommand:     cfg.Cfg.Pipelines.Build.TestCommand,
			ValidateCommand: cfg.Cfg.Pipelines.Build.ValidateCommand,
			ShellMaxBytes:   8192,
			PushFn: func(run *pipeline.Run) error {
				return resolvePushBranch(run.WorktreePath, run.BranchName)
			},
		},
	}

	return d, nil
}

// Adapter returns the active adapter. Useful for tests + introspection.
func (d *Daemon) Adapter() adapter.Adapter { return d.adp }

// SetAdapter swaps the adapter; primarily for tests. Re-binds the build
// pipeline so future RunStage calls use the new adapter.
func (d *Daemon) SetAdapter(a adapter.Adapter) {
	d.adp = a
	if bp, ok := d.pipelines["build"].(*pipeline.BuildPipeline); ok {
		bp.Adapter = a
	}
	if pp, ok := d.pipelines["plan"].(*pipeline.PlanPipeline); ok {
		pp.Adapter = a
	}
	if dp, ok := d.pipelines["debug"].(*pipeline.DebugPipeline); ok {
		dp.Adapter = a
	}
}

// reapStaleChatSessions closes chat sessions that were left open (ended_at = 0)
// by a crash, context cancellation, or daemon restart mid-turn. Any session
// whose started_at is older than now - OpenSessionStaleHours hours is marked
// ended with the current wall time. Best-effort: errors are logged but never
// abort startup. No-op when OpenSessionStaleHours <= 0.
func (d *Daemon) reapStaleChatSessions() {
	staleHours := d.cfg.Cfg.Chat.OpenSessionStaleHours
	if staleHours <= 0 {
		return
	}
	staleBefore := time.Now().Unix() - int64(staleHours)*3600
	n, err := d.store.ReapStaleChatSessions(d.ctx, staleBefore)
	if err != nil {
		log.Printf("chat: reap stale open sessions: %v", err)
		return
	}
	if n > 0 {
		log.Printf("chat: reaped %d stale-open session(s) (threshold: %dh)", n, staleHours)
	}
}

// reapChatScratch removes orphan per-session chat scratch dirs whose
// session_id no longer has a chat_sessions row. Called from Start()
// after the chat agent has been injected (or not, in which case it's a
// no-op). Best-effort: a missing scratchRoot is fine; per-dir RemoveAll
// errors are silently skipped (next startup retries).
//
// The agent contract is the ReapOrphans interface — only the claude-
// code adapter implements it today; the SDK agent has no scratch root.
func (d *Daemon) reapChatScratch() {
	if d.chatAgent == nil {
		return
	}
	reaper, ok := d.chatAgent.(interface {
		ReapOrphans(ctx context.Context, knownSessions map[string]bool) ([]string, error)
	})
	if !ok {
		return
	}
	// Pull all session IDs — sessions are bounded; 10k cap covers any
	// realistic user. A future migration to "stream IDs" can swap in a
	// cheaper dedicated query if this ever feels heavy.
	rows, err := d.store.ListChatSessions(d.ctx, 10000)
	if err != nil {
		log.Printf("chat: list sessions for reap: %v", err)
		return
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[r.ID] = true
	}
	removed, err := reaper.ReapOrphans(d.ctx, known)
	if err != nil {
		log.Printf("chat: reap orphan scratch dirs: %v", err)
		return
	}
	if len(removed) > 0 {
		log.Printf("chat: reaped %d orphan scratch dir(s)", len(removed))
	}
}

// Start binds the daemon's listeners and runs until ctx is done. It is
// NOT safe to call Stop while Start is in progress: Stop reads several
// fields (d.mcpServer, d.listener, d.cancel) that Start writes
// throughout its execution. Callers must serialize the two — the
// production cmd/hive/cmd_daemon flow does this via signal-handler ->
// cancel(ctx) -> Start returns -> Stop().
//
// Callers that run Start in a goroutine (e.g. tests) must wait for the
// daemon to finish wiring before reading its fields or calling Stop. Use
// WaitReady for this: Start closes the d.ready channel once all fields are
// wired (just before it blocks on ctx.Done()), and WaitReady observes that
// close. Do `go Start(ctx)` -> WaitReady(...) -> assert / Stop().
//
// Start binds the Unix socket, launches the RPC accept loop and the
// scheduler tick loop, then blocks until ctx is canceled. Stop must
// still be called by the caller to release resources.
func (d *Daemon) Start(ctx context.Context) error {
	d.ctx, d.cancel = context.WithCancel(ctx)

	// Phase 2b.4 follow-up: acquire singleton lock BEFORE touching the
	// socket. If another daemon is already running, refuse to start
	// (and don't unlink its socket via the os.Remove below). The lock
	// is held by an open file descriptor for the daemon's lifetime;
	// OS releases it on close OR process exit (clean, crash, SIGKILL).
	lock, err := acquireSingletonLock(d.pidPath, d.sock)
	if err != nil {
		return err
	}
	d.pidLock = lock

	_ = os.Remove(d.sock)
	ln, err := net.Listen("unix", d.sock)
	if err != nil {
		_ = d.pidLock.Close()
		_ = os.Remove(d.pidPath)
		return fmt.Errorf("listen daemon.sock: %w", err)
	}
	if err := os.Chmod(d.sock, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod daemon.sock: %w", err)
	}
	d.listener = ln

	// Phase 6 chat polish: close chat sessions left open by crashes or
	// mid-turn errors in a prior daemon run. Best-effort — errors are
	// logged but never abort startup.
	d.reapStaleChatSessions()

	// Phase 6 chat polish: reap orphan per-session chat scratch dirs
	// from prior daemon runs (sessions deleted while daemon was off,
	// pre-EvictSession sessions, or daemon crashes). Best-effort —
	// errors are logged but never abort startup.
	d.reapChatScratch()

	// Phase 6.3: long-running MCP-over-HTTP server. Bound now so the
	// listener is up before any claude spawn — eliminates the per-turn
	// stdio MCP startup race.
	mcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln.Close()
		_ = d.pidLock.Close()
		_ = os.Remove(d.pidPath)
		return fmt.Errorf("listen mcp http: %w", err)
	}
	d.mcpHTTPAddr = mcpLn.Addr().String()
	d.mcpURL = "http://" + d.mcpHTTPAddr
	urlPath := filepath.Join(d.cfg.HiveDir, "mcp.url")
	if err := os.WriteFile(urlPath, []byte(d.mcpURL), 0600); err != nil {
		mcpLn.Close()
		ln.Close()
		_ = d.pidLock.Close()
		_ = os.Remove(d.pidPath)
		return fmt.Errorf("write mcp.url: %w", err)
	}

	mcpSrv := mcphttp.NewServer()
	// Phase 8.A T8: chat route advertises per-session tool sets.
	// sessionKindLookup reads ChatSession.Kind + ProjectSlug from the
	// store + resolves the project's repo_path; plannerRegistryFor wraps
	// chat.NewPlannerRegistry. For kind=="plan" sessions tools/list and
	// tools/call see the planner registry's tools instead of the default
	// chat tools, fixing the "No such tool available: hive_list_specs"
	// failure mode under [chat] use_http_mcp = true.
	sessionKindLookup := func(ctx context.Context, sessionID string) (string, string, string, error) {
		sess, err := d.store.GetChatSession(ctx, sessionID)
		if err != nil || sess == nil {
			return "", "", "", err
		}
		if sess.Kind != store.KindPlan {
			return sess.Kind, "", "", nil
		}
		if sess.ProjectSlug == "" {
			return sess.Kind, "", "", nil
		}
		proj, perr := d.store.GetProjectBySlug(ctx, sess.ProjectSlug)
		if perr != nil || proj == nil || proj.RepoPath == nil || *proj.RepoPath == "" {
			return sess.Kind, sess.ProjectSlug, "", perr
		}
		return sess.Kind, sess.ProjectSlug, *proj.RepoPath, nil
	}
	plannerRegistryFor := func(slug, cwd string) (*chat.Registry, error) {
		return chat.NewPlannerRegistry(cwd, d.chatRegistry(), d.scheduler.effectiveFeatureBranchForProject(slug), d.plannerGrounderFor(slug, cwd)), nil
	}
	mcpSrv.RegisterChat(buildChatRoute(d.chatRegistry(), sessionKindLookup, plannerRegistryFor))
	mcpSrv.RegisterStage(buildStageRoute(d.stageVerdicts, nil)) // doc submit wired in 6.3 follow-up
	if d.approver != nil {
		mcpSrv.RegisterPerm(buildPermRoute(d.approver))
	}
	d.mcpServer = &http.Server{Handler: mcpSrv}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		_ = d.mcpServer.Serve(mcpLn)
	}()

	d.rpc = NewRPCServer(d)
	d.scheduler = NewScheduler(d)

	d.wg.Add(3)
	go func() { defer d.wg.Done(); d.rpc.Accept(d.ctx, ln) }()
	go func() { defer d.wg.Done(); d.scheduler.Loop(d.ctx) }()
	go func() {
		defer d.wg.Done()
		hbPeriod := time.Duration(d.cfg.Cfg.TUI.HeartbeatSeconds) * time.Second
		startHeartbeat(d.ctx, d.bus, hbPeriod)
	}()

	// Phase 5.3: poll bound sources on their configured intervals.
	d.goTracked(func() { d.syncLoop(d.ctx) })
	d.goTracked(func() { d.reconcileLoop(d.ctx) })

	// Run-artifact GC: reclaim old terminal runs' worktrees + scratch on boot
	// AND on a periodic loop so they don't accumulate over the daemon's
	// lifetime (best-effort, logged). Both gated by [cleanup] auto_sweep.
	if d.cfg.Cfg.Cleanup.ResolvedAutoSweep() {
		d.goTracked(func() { d.sweepRunArtifacts(d.ctx) })
		d.goTracked(func() { d.sweepLoop(d.ctx) })
	}

	// All fields are wired. Signal readiness exactly once (Start runs once
	// per daemon, and any early-error path above returned before reaching
	// here, leaving ready open — correct, since the daemon never became
	// ready). Callers blocked in WaitReady can now safely read fields /
	// call Stop per the Start contract.
	close(d.ready)

	<-d.ctx.Done()
	return nil
}

// WaitReady blocks until Start has finished wiring the daemon (or the timeout
// elapses). Returns true when ready. Used to serialize Stop after Start per the
// Start contract — esp. in tests that run Start in a goroutine.
func (d *Daemon) WaitReady(timeout time.Duration) bool {
	select {
	case <-d.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Stop is idempotent; safe to call multiple times. Cancels the context,
// closes the listener (which unblocks Accept), waits for goroutines,
// then closes the adapter and store and removes the socket file.
//
// Per-run scavenger daemons are torn down per-run in executePipeline; the
// daemon no longer owns a single global scavenger lifecycle.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		// Phase 6.3: shut down the HTTP MCP server before tearing down
		// the rest of the daemon.
		if d.mcpServer != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := d.mcpServer.Shutdown(shutdownCtx); err != nil {
				log.Printf("daemon: mcp http shutdown timed out (%v); forcing close", err)
				_ = d.mcpServer.Close() // force-close idle and active conns
			}
			cancel()
			urlPath := filepath.Join(d.cfg.HiveDir, "mcp.url")
			_ = os.Remove(urlPath)
		}

		// Phase 3.5a: notify subscribers of clean shutdown before
		// tearing down the listener. Brief sleep gives the streaming
		// handler time to flush the event to the socket before close.
		if d.bus != nil {
			d.bus.Publish(rpc.EventMessage{Type: rpc.EventDaemonStopping})
			time.Sleep(50 * time.Millisecond)
		}
		if d.cancel != nil {
			d.cancel()
		}
		if d.listener != nil {
			_ = d.listener.Close()
		}
		d.wg.Wait()
		if d.adp != nil {
			_ = d.adp.Close()
		}
		_ = d.store.Close()
		_ = os.Remove(d.sock)
		if d.pidLock != nil {
			_ = d.pidLock.Close()
			_ = os.Remove(d.pidPath)
		}
	})
}

// Store exposes the underlying store for tests and the scheduler.
func (d *Daemon) Store() *store.Store { return d.store }

// Pipeline returns the registered pipeline by name (nil if unknown).
func (d *Daemon) Pipeline(name string) pipeline.Pipeline { return d.pipelines[name] }

// HasPipeline reports whether a pipeline with the given name is registered.
func (d *Daemon) HasPipeline(name string) bool {
	_, ok := d.pipelines[name]
	return ok
}

// PipelineNames returns the registered pipeline names (e.g. "build").
func (d *Daemon) PipelineNames() []string {
	names := make([]string, 0, len(d.pipelines))
	for name := range d.pipelines {
		names = append(names, name)
	}
	return names
}

// WorktreeManager exposes the worktree manager.
func (d *Daemon) WorktreeManager() *worktree.Manager { return d.wtMgr }

// HiveDir returns the root state directory.
func (d *Daemon) HiveDir() string { return d.cfg.HiveDir }

// StartedAt returns the time New was called. Used by doctor.health
// to compute uptime.
func (d *Daemon) StartedAt() time.Time { return d.startedAt }

// MCPHTTPAddr returns the bound address of the MCP HTTP server
// (e.g. "127.0.0.1:36421"), or "" if HTTP MCP is disabled or the
// server failed to start. Used by doctor's mcp.http_listener check.
func (d *Daemon) MCPHTTPAddr() string { return d.mcpHTTPAddr }

// StageVerdicts returns the per-stage verdict listener registry. The
// claudecode adapter calls Register/Remove to attach a stage's Listener
// for HTTP-transport routing.
func (d *Daemon) StageVerdicts() *verdict.StageRegistry { return d.stageVerdicts }

// MCPURL returns the base URL of the HTTP MCP server. Empty until
// Start has bound the listener and written mcp.url.
func (d *Daemon) MCPURL() string { return d.mcpURL }

// goTracked runs fn in a goroutine tracked by the daemon WaitGroup so
// Stop() blocks until it returns. Used for pipeline execution: on
// shutdown d.cancel() cancels every run ctx, the worker subprocess
// groups are killed, and Stop waits here for those goroutines to unwind
// before the process exits — so no orphaned workers survive a restart.
func (d *Daemon) goTracked(fn func()) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn()
	}()
}

// registerRunCancel records a per-run cancel function. Pipeline
// goroutines call this on entry; abandon handler looks up + invokes.
func (d *Daemon) registerRunCancel(runID string, cancel context.CancelFunc) {
	d.runCancelsMu.Lock()
	d.runCancels[runID] = cancel
	d.runCancelsMu.Unlock()
}

// unregisterRunCancel cleans up after the pipeline returns (normally
// or via cancellation).
func (d *Daemon) unregisterRunCancel(runID string) {
	d.runCancelsMu.Lock()
	delete(d.runCancels, runID)
	d.runCancelsMu.Unlock()
}

// cancelRun cancels an in-flight run; returns true if it was found.
// Caller marks the run row + emits the event.
func (d *Daemon) cancelRun(runID string) bool {
	d.runCancelsMu.Lock()
	cancel, ok := d.runCancels[runID]
	d.runCancelsMu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// approvalRuleAdapter backs approval.RuleStore with the SQLite store,
// converting store.ApprovalRule -> approval.Rule so internal/approval
// doesn't import internal/store.
type approvalRuleAdapter struct{ s *store.Store }

func (a approvalRuleAdapter) ListApprovalRules(ctx context.Context) ([]approval.Rule, error) {
	rows, err := a.s.ListApprovalRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]approval.Rule, len(rows))
	for i, r := range rows {
		out[i] = approval.Rule{
			ID: r.ID, Scope: r.Scope, ToolName: r.ToolName,
			ArgMatcher: r.ArgMatcher, Decision: r.Decision,
		}
	}
	return out, nil
}

// pickApprover returns the real fail-closed engine (seeded from the
// [approvals] config allow-lists) when approvals are enabled, else the
// allow-all Stub (preserving pre-4.5 behavior).
func pickApprover(cfg Config, s *store.Store) approval.Engine {
	if !cfg.Cfg.Approvals.Enabled {
		return approval.NewStub()
	}
	var defaults []approval.Rule
	for _, tool := range cfg.Cfg.Approvals.DefaultAllowWorker {
		defaults = append(defaults, approval.Rule{Scope: "global", ToolName: tool, Decision: "allow"})
	}
	for _, pat := range cfg.Cfg.Approvals.DefaultAllowWorkerBash {
		defaults = append(defaults, approval.Rule{Scope: "global", ToolName: "Bash", ArgMatcher: pat, Decision: "allow"})
	}
	for _, tool := range cfg.Cfg.Approvals.DefaultAllowReviewer {
		// Reviewer defaults apply to both code review and spec review.
		defaults = append(defaults, approval.Rule{Scope: "stage:review", ToolName: tool, Decision: "allow"})
		defaults = append(defaults, approval.Rule{Scope: "stage:spec-review", ToolName: tool, Decision: "allow"})
	}
	return approval.NewRealEngine(approvalRuleAdapter{s: s}, defaults)
}

// pendingApproval holds a blocked approval's resolution channel + the
// request metadata (so daemon.status can list it for TUI hydration).
type pendingApproval struct {
	ch          chan approval.Decision
	RunID       string
	Stage       string
	ToolName    string
	Input       map[string]any
	RequestedAt int64
}

// registerPending records a pending approval (Phase 4.6 ask mode) and
// returns the channel the caller blocks on.
func (d *Daemon) registerPending(id, runID, stage, toolName string, input map[string]any) chan approval.Decision {
	ch := make(chan approval.Decision, 1)
	d.pendingMu.Lock()
	d.pendingApprovals[id] = &pendingApproval{
		ch: ch, RunID: runID, Stage: stage, ToolName: toolName,
		Input: input, RequestedAt: time.Now().Unix(),
	}
	d.pendingMu.Unlock()
	return ch
}

// resolvePending delivers a decision to a blocked approval. Returns
// false if the id is unknown (already resolved / timed out).
func (d *Daemon) resolvePending(id string, dec approval.Decision) bool {
	d.pendingMu.Lock()
	p, ok := d.pendingApprovals[id]
	if ok {
		delete(d.pendingApprovals, id)
	}
	d.pendingMu.Unlock()
	if ok {
		p.ch <- dec
	}
	return ok
}

// clearPending drops a pending entry without delivering (timeout/cancel).
func (d *Daemon) clearPending(id string) {
	d.pendingMu.Lock()
	delete(d.pendingApprovals, id)
	d.pendingMu.Unlock()
}

// listPending returns the currently-pending approvals for daemon.status,
// so a (re)subscribing TUI can hydrate its inbox.
func (d *Daemon) listPending() []map[string]any {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	out := make([]map[string]any, 0, len(d.pendingApprovals))
	for id, p := range d.pendingApprovals {
		out = append(out, map[string]any{
			"approval_id": id, "run_id": p.RunID, "stage": p.Stage,
			"tool_name": p.ToolName, "tool_input": p.Input,
		})
	}
	return out
}
