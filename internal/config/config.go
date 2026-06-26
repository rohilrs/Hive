package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Concurrency    Concurrency    `toml:"concurrency"`
	Costs          Costs          `toml:"costs"`
	Models         Models         `toml:"models"`
	Pipelines      Pipelines      `toml:"pipelines"`
	Predictor      Predictor      `toml:"predictor"`
	ConflictGuard  ConflictGuard  `toml:"conflict_guard"`
	StallDetection StallDetection `toml:"stall_detection"`
	Approvals      Approvals      `toml:"approvals"`
	Chat           Chat           `toml:"chat"`
	Scavenger      Scavenger      `toml:"scavenger"`
	ClaudeCLI      ClaudeCLI      `toml:"claude_cli"`
	Anthropic      Anthropic      `toml:"anthropic"`
	Sources        Sources        `toml:"sources"`
	LLM            LLM            `toml:"llm"`
	TUI            TUI            `toml:"tui"`
	Scheduler      Scheduler      `toml:"scheduler"`
	Integration    Integration    `toml:"integration"`
	Cleanup        Cleanup        `toml:"cleanup"`
}

// Dispatch mode constants. dispatch_mode generalizes the legacy
// auto_dispatch bool (Phase 3.7.2). When dispatch_mode is empty, it
// falls back to auto_dispatch for backward compatibility.
const (
	DispatchModeManual    = "manual"    // no auto-dispatch; run.now only
	DispatchModeAutoAll   = "auto_all"  // fire every pending task (legacy auto_dispatch=true)
	DispatchModeSequenced = "sequenced" // phase-ordered dispatch (engine lands Phase 2)
)

// Scheduler controls dispatch behavior. Phase 3.7.2 added AutoDispatch;
// the sequenced-dispatcher work generalizes it to DispatchMode and adds
// TargetBranch.
type Scheduler struct {
	// AutoDispatch is the legacy per-project toggle. Retained for
	// backward compatibility: when DispatchMode is empty, AutoDispatch
	// is consulted (true -> auto_all, false -> manual). Prefer
	// DispatchMode in new config.
	AutoDispatch bool `toml:"auto_dispatch"`

	// DispatchMode selects the dispatch policy: "manual", "auto_all",
	// or "sequenced". Empty falls back to AutoDispatch. An explicit
	// value always wins over AutoDispatch.
	DispatchMode string `toml:"dispatch_mode"`

	// TargetBranch is the integration branch this project's work targets:
	// it is both the base branch new worktrees fork from and the PR base
	// for the finish-branch pipeline. Applies to ALL dispatch modes.
	// Empty resolves to "main".
	TargetBranch string `toml:"target_branch"`

	// MergePollIntervalSeconds controls how often the merge-poller checks
	// awaiting_merge PRs (human_merge / auto_merge_on_green). 0 uses the
	// default; a negative value disables the poller entirely.
	MergePollIntervalSeconds int `toml:"merge_poll_interval_seconds"`
}

// ResolvedMode returns the effective dispatch mode, applying the
// AutoDispatch back-compat fallback. An explicit DispatchMode always
// takes precedence.
func (s Scheduler) ResolvedMode() string {
	if s.DispatchMode != "" {
		return s.DispatchMode
	}
	if s.AutoDispatch {
		return DispatchModeAutoAll
	}
	return DispatchModeManual
}

// ResolvedTargetBranch returns TargetBranch or "main" when empty.
func (s Scheduler) ResolvedTargetBranch() string {
	if s.TargetBranch == "" {
		return "main"
	}
	return s.TargetBranch
}

// ResolvedMergePollInterval returns the poll interval, defaulting to 30s.
// Returns 0 for a negative configured value. NOTE (Phase 2): this no longer
// disables detection — the unified reconcile loop floors a non-positive result
// to 30s because that loop also hosts the Linear write-back outbox and must
// never stop ticking. There is currently no way to disable merge detection.
func (s Scheduler) ResolvedMergePollInterval() time.Duration {
	if s.MergePollIntervalSeconds < 0 {
		return 0
	}
	if s.MergePollIntervalSeconds == 0 {
		return 30 * time.Second
	}
	return time.Duration(s.MergePollIntervalSeconds) * time.Second
}

// Integration configures the feature-branch integration loop: a per-initiative
// feature branch where task work auto-integrates (finish-branch → PR → CI →
// merge), separate from the Scheduler.TargetBranch promotion target. Empty
// FeatureBranch disables the whole feature (backward-compatible default).
type Integration struct {
	// FeatureBranch is the per-initiative integration branch. Task worktrees
	// fork from it and task PRs target it. Empty = feature disabled.
	FeatureBranch string `toml:"feature_branch"`
	// TaskAutoIntegrate: when true, a successful build run auto-chains
	// finish-branch → PR into FeatureBranch → CI → auto-merge, in ANY dispatch
	// mode. When false, integration only happens via `hive run finish`.
	TaskAutoIntegrate bool `toml:"task_auto_integrate"`
	// MergeMethod is the gh pr merge strategy for task→feature merges:
	// "squash" | "merge" | "rebase". Empty → "merge".
	MergeMethod string `toml:"merge_method"`
	// AutoFixCI: when true, a GitHub-CI failure on a task PR triggers one
	// auto-fix child run (same branch) before the task goes needs_attention.
	AutoFixCI bool `toml:"auto_fix_ci"`
	// CIFixPushCommand is the shell command run (in the task worktree) to push
	// an auto-fix commit to the PR branch before ci-monitor re-checks CI. Empty
	// → "git push origin HEAD". Only used when AutoFixCI is set.
	CIFixPushCommand string `toml:"ci_fix_push_command"`
}

// ResolvedMergeMethod returns MergeMethod or "merge" when empty.
func (i Integration) ResolvedMergeMethod() string {
	if i.MergeMethod == "" {
		return "merge"
	}
	return i.MergeMethod
}

// ResolvedCIFixPushCommand returns CIFixPushCommand or "git push origin HEAD".
func (i Integration) ResolvedCIFixPushCommand() string {
	if i.CIFixPushCommand == "" {
		return "git push origin HEAD"
	}
	return i.CIFixPushCommand
}

// Cleanup controls reclamation of per-run on-disk artifacts (run-artifact GC).
type Cleanup struct {
	AutoSweep            *bool `toml:"auto_sweep"`             // sweep on daemon boot (default true)
	KeepLastRuns         int   `toml:"keep_last_runs"`         // retain N most-recent runs (default 20)
	OrphanGraceMinutes   int   `toml:"orphan_grace_minutes"`   // min age for orphan-dir GC (default 60)
	CleanBranches        *bool `toml:"clean_branches"`         // also git branch -D hive/<run> (default true)
	SweepIntervalMinutes int   `toml:"sweep_interval_minutes"` // periodic sweep cadence (default 30; negative disables, boot-only)
}

func (c Cleanup) ResolvedAutoSweep() bool {
	return c.AutoSweep == nil || *c.AutoSweep
}
func (c Cleanup) ResolvedKeepLastRuns() int {
	if c.KeepLastRuns <= 0 {
		return 20
	}
	return c.KeepLastRuns
}
func (c Cleanup) ResolvedOrphanGrace() time.Duration {
	m := c.OrphanGraceMinutes
	if m <= 0 {
		m = 60
	}
	return time.Duration(m) * time.Minute
}
func (c Cleanup) ResolvedCleanBranches() bool {
	return c.CleanBranches == nil || *c.CleanBranches
}

// ResolvedSweepInterval is the periodic worktree/scratch sweep cadence. 0
// (unset) defaults to 30m; a negative value disables the periodic loop (the
// boot sweep still runs when auto_sweep is on). Returns 0 when disabled.
func (c Cleanup) ResolvedSweepInterval() time.Duration {
	if c.SweepIntervalMinutes < 0 {
		return 0
	}
	if c.SweepIntervalMinutes == 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.SweepIntervalMinutes) * time.Minute
}

// TUI configures the event-subscription transport + future TUI client
// behavior. Phase 3.5a uses EventBufferSize + ResyncOnOverflow +
// HeartbeatSeconds; Phase 3.5b will add RefreshIntervalMs.
type TUI struct {
	// EventBufferSize is the per-subscriber bounded channel capacity.
	// Default 1000.
	EventBufferSize int `toml:"event_buffer_size"`

	// ResyncOnOverflow controls whether a dropped event triggers a
	// synthetic `resync` event (true) or is silent (false). Default
	// true.
	ResyncOnOverflow bool `toml:"event_resync_on_overflow"`

	// HeartbeatSeconds is the cadence of daemon.heartbeat events.
	// Zero disables heartbeats. Default 5.
	HeartbeatSeconds int `toml:"heartbeat_seconds"`
}

// ConflictGuard gates predicted-file-overlap detection in the scheduler.
// When false, the predictor still runs (for metrics) but the scheduler
// doesn't queue conflicting runs — they all dispatch concurrently.
// Default true.
type ConflictGuard struct {
	Enabled bool `toml:"enabled"`
}

type Concurrency struct {
	MaxWorkers        int `toml:"max_workers"`
	PerRepoCap        int `toml:"per_repo_cap"`
	PerRepoCapNoGuard int `toml:"per_repo_cap_no_guard"`
	FinishBranchCap   int `toml:"finish_branch_cap"`
	PlanCap           int `toml:"plan_cap"`
}

type Costs struct {
	CapPerRunUSD float64 `toml:"cap_per_run_usd"`
	CapPerDayUSD float64 `toml:"cap_per_day_usd"`
	AlertAtPct   int     `toml:"alert_at_pct"`
}

type Models struct {
	WorkerLadder   []string `toml:"worker_ladder"`
	ReviewerLadder []string `toml:"reviewer_ladder"`
	Classifier     string   `toml:"classifier"`
	ChatDefault    string   `toml:"chat_default"`
	ChatReasoning  string   `toml:"chat_reasoning"`
	// DecomposeModel is the model for roadmap.decompose (one-shot tool turn).
	// Empty defers to the claude CLI default (subscription) / decompose.DefaultModel (API).
	DecomposeModel string `toml:"decompose_model"`
	// GraduateValidatorModel is the model for the Stage-4 completion audit of
	// `hive project graduate` (a roaming oneshot tool turn). Empty defers to the
	// claude CLI default (subscription).
	GraduateValidatorModel string `toml:"graduate_validator_model"`
}

type Pipelines struct {
	Build        BuildPipeline        `toml:"build"`
	Debug        SimplePipeline       `toml:"debug"`
	Plan         SimplePipeline       `toml:"plan"`
	FinishBranch FinishBranchPipeline `toml:"finish_branch"`
	Resolve      ResolvePipeline      `toml:"resolve"`
	Graduate     GraduatePipeline     `toml:"graduate"`
}

// ResolvePipeline configures the conflict-resolver pipeline. It inherits the
// build pipeline's test/validate commands + worker ladder unless overridden;
// only the resolve-specific knobs live here.
type ResolvePipeline struct {
	// Auto, when true, auto-dispatches a resolve run when the sequence
	// engine's auto-merge hits a content conflict (instead of needs_attention).
	// Default false (opt-in per project).
	Auto bool `toml:"auto"`
	// MaxIterations bounds the resolve->validate feedback loop. Default 5.
	MaxIterations int `toml:"max_iterations"`
	// StageTimeoutMinutes bounds each resolve subprocess. Default 20.
	StageTimeoutMinutes int `toml:"stage_timeout_minutes"`
}

// SeamPatterns layers per-project additions over the built-in seam pattern
// library used by the graduate code-seam extractor. Pure data (stdlib-only) so
// internal/config stays light; the graduate package imports it.
type SeamPatterns struct {
	CallVerbs       []string `toml:"call_verbs"`
	RegVerbs        []string `toml:"reg_verbs"`
	RouterReceivers []string `toml:"router_receivers"`
	ExcludeGlobs    []string `toml:"exclude_globs"`
}

// GraduatePipeline configures `hive project graduate`'s completion-audit stage.
type GraduatePipeline struct {
	// AuditRuns is K for the ensemble: the audit runs this many times in
	// parallel and the findings are unioned + verified. Default 5. 1 reproduces
	// the legacy single-pass behavior (still verified). <=0 → default.
	AuditRuns int `toml:"audit_runs"`
	// SeamAudit enables the deterministic code-seam extractor (unwired
	// route/RPC/event detection) as an additional audit finding source.
	// nil/unset → true. Set false to disable for a noisy stack.
	SeamAudit *bool `toml:"seam_audit"`
	// SeamPatterns layers per-project verb/receiver/glob additions over the
	// built-in seam pattern library.
	SeamPatterns SeamPatterns `toml:"seam_patterns"`
	// PhaseAudit enables the phase-partitioned deliverable auditor (one focused
	// agent per roadmap phase verifies that phase's deliverables). nil/unset → true.
	PhaseAudit *bool `toml:"phase_audit"`
}

const defaultGraduateAuditRuns = 5

// ResolvedAuditRuns returns AuditRuns, defaulting to 5 when unset/non-positive.
func (g GraduatePipeline) ResolvedAuditRuns() int {
	if g.AuditRuns <= 0 {
		return defaultGraduateAuditRuns
	}
	return g.AuditRuns
}

// ResolvedSeamAudit returns SeamAudit, defaulting to true when unset.
func (g GraduatePipeline) ResolvedSeamAudit() bool {
	if g.SeamAudit == nil {
		return true
	}
	return *g.SeamAudit
}

// ResolvedPhaseAudit returns PhaseAudit, defaulting to true when unset.
func (g GraduatePipeline) ResolvedPhaseAudit() bool {
	if g.PhaseAudit == nil {
		return true
	}
	return *g.PhaseAudit
}

// FinishBranchPipeline configures the finish-branch pipeline: shell gates
// (typecheck/lint/test) then create-pr + ci-monitor. Empty command skips
// that stage. Commands default to the Go toolchain + gh; tune per project.
type FinishBranchPipeline struct {
	TypecheckCommand string `toml:"typecheck_command"`
	LintCommand      string `toml:"lint_command"`
	FormatCommand    string `toml:"format_command"`
	TestCommand      string `toml:"test_command"`
	// PrepareCommand installs/prepares dependencies in a fresh worktree before
	// the shippability gates run. The per-task build stage normally installs deps
	// (e.g. `pnpm install`) in the worktree, but `hive project graduate` runs the
	// gates on a fresh detached checkout with no build stage — so it runs this
	// once, before the gates. Empty = skip. ONLY consumed by graduate Stage 3.
	PrepareCommand          string `toml:"prepare_command"`
	CreatePRCommand         string `toml:"create_pr_command"`
	CIMonitorCommand        string `toml:"ci_monitor_command"`
	StageTimeoutMinutes     int    `toml:"stage_timeout_minutes"`
	CIMonitorTimeoutMinutes int    `toml:"ci_monitor_timeout_minutes"`
	ConflictHopDepth        int    `toml:"conflict_hop_depth"`
	ShellOutputMaxBytes     int    `toml:"shell_output_max_bytes"`
	MaxFixAttempts          int    `toml:"max_fix_attempts"`
}

type BuildPipeline struct {
	MaxIterations        int             `toml:"max_iterations"`
	PrefetchBudgetTokens int             `toml:"prefetch_budget_tokens"`
	StageTimeoutMinutes  int             `toml:"stage_timeout_minutes"`
	ConflictHopDepth     int             `toml:"conflict_hop_depth"`
	Documenter           BuildDocumenter `toml:"documenter"`

	// TestCommand is the shell command run AFTER review APPROVE.
	// Empty string skips the stage. Default "go test ./...".
	TestCommand string `toml:"test_command"`

	// ValidateCommand is the shell command run after TestCommand
	// passes. Empty string skips the stage. Default
	// "go build ./... && go vet ./...".
	ValidateCommand string `toml:"validate_command"`

	// TestStageTimeoutMinutes bounds the TestCommand execution.
	// Default 10.
	TestStageTimeoutMinutes int `toml:"test_stage_timeout_minutes"`

	// ValidateStageTimeoutMinutes bounds the ValidateCommand execution.
	// Default 5.
	ValidateStageTimeoutMinutes int `toml:"validate_stage_timeout_minutes"`
}

type BuildDocumenter struct {
	Enabled             bool     `toml:"enabled"`
	Model               string   `toml:"model"`
	StageTimeoutMinutes int      `toml:"stage_timeout_minutes"`
	CodeComments        bool     `toml:"code_comments"`
	UpdateReadme        bool     `toml:"update_readme"`
	SkillsToLoad        []string `toml:"skills_to_load"`
}

type SimplePipeline struct {
	MaxIterations    int `toml:"max_iterations"`
	ConflictHopDepth int `toml:"conflict_hop_depth"`
}

type Predictor struct {
	// Enabled toggles the whole predictor + capsule pre-fetch path.
	// Per-project overrides allowed. When false, the daemon dispatches
	// without prediction (pre-2b behavior).
	Enabled bool `toml:"enabled"`

	// BundleTokenCap caps the total token estimate of capsules inlined
	// into the implement system prompt. Overflow capsules are surfaced
	// by name only; the full bundle is written to prefetch.md.
	BundleTokenCap int `toml:"bundle_token_cap"`

	// MaxCandidates is the upper bound on Haiku's returned candidate
	// list. Beyond this, Haiku is instructed to truncate to the most
	// relevant entries.
	MaxCandidates int `toml:"max_candidates"`

	// PerCallTimeoutSeconds bounds a single `scavenger capsule` CLI
	// invocation. Total predictor wall-clock is approximately
	// HaikuTimeoutSeconds + min(MaxCandidates, top-k) * PerCallTimeoutSeconds
	// in the worst sequential case (parallel fetches in practice).
	PerCallTimeoutSeconds int `toml:"per_call_timeout_seconds"`

	// HaikuTimeoutSeconds bounds the single Haiku PredictFiles call.
	HaikuTimeoutSeconds int `toml:"haiku_timeout_seconds"`

	// HaikuModel overrides the model name; empty inherits from
	// Models.Classifier (the existing Haiku-tier config).
	HaikuModel string `toml:"haiku_model"`

	// Existing fields preserved for 2b.5's metrics-driven auto-degrade.
	PrecisionKillThreshold float64 `toml:"precision_kill_threshold"`
	RollingWindowDays      int     `toml:"rolling_window_days"`
	ForceEnable            bool    `toml:"force_enable"`
}

// LLM picks which provider satisfies the control-plane LLM operations
// (classifier-fallback for verdicts, predictor for file ranking).
//   - "cli"  → spawn `claude -p` subprocess; uses Claude Max
//     subscription auth via the `claude` binary.
//   - "api"  → use the Anthropic SDK with ANTHROPIC_API_KEY.
type LLM struct {
	Provider string `toml:"provider"`
}

type StallDetection struct {
	EventHeartbeatSeconds      int     `toml:"event_heartbeat_seconds"`
	ToolCallMaxMinutes         int     `toml:"tool_call_max_minutes"`
	ImplementStagnationMinutes int     `toml:"implement_stagnation_minutes"`
	LoopCheckAfterIter         int     `toml:"loop_check_after_iter"`
	LoopSimilarityThreshold    float64 `toml:"loop_similarity_threshold"`
	NotifyOnStall              bool    `toml:"notify_on_stall"`
}

type Approvals struct {
	// Enabled turns on tool-use approval gating via Claude Code's
	// --permission-prompt-tool. Default false. Phase 4.5 ships a spike
	// (always-allow + input logging) behind this flag; the real engine
	// lands once the mechanism is validated against real claude.
	Enabled bool `toml:"enabled"`
	// Mode controls unmatched tool calls when enabled: "ask" (default)
	// blocks for a live TUI decision (timeout -> deny); "deny" fail-closed
	// denies immediately (4.5 behavior).
	Mode                   string   `toml:"mode"`
	HookTimeoutSeconds     int      `toml:"hook_timeout_seconds"`
	DefaultAllowWorker     []string `toml:"default_allow_worker"`
	DefaultAllowWorkerBash []string `toml:"default_allow_worker_bash"`
	DefaultAllowReviewer   []string `toml:"default_allow_reviewer"`
}

type Chat struct {
	// Model + loop settings for the daemon-hosted chat agent (Phase 6.1).
	// Provider selects the chat backend: "api" (Anthropic SDK) or
	// "claude-code" (Claude Code subprocess). Unknown values fall back to
	// "api" at the composition root.
	Provider       string `toml:"provider"`
	DefaultModel   string `toml:"default_model"`
	ReasoningModel string `toml:"reasoning_model"`
	APIKeyEnv      string `toml:"api_key_env"`
	MaxIters       int    `toml:"max_iters"`

	// AutoConfirm lists tools that bypass the confirm gate.
	AutoConfirm []string `toml:"auto_confirm"`
	// ConfirmTimeoutSeconds is how long the daemon waits for a user confirm
	// response before auto-rejecting. Default 300.
	ConfirmTimeoutSeconds int `toml:"confirm_timeout_seconds"`

	// UseHTTPMCP opts the chat tool MCP server into HTTP transport
	// (Phase 6.3A toggle). When true, the chat MCP server is reached
	// via the daemon's long-running HTTP listener; stdio subprocess
	// is not spawned. Set per-smoke during Phase 6.3B; becomes
	// unconditional after the Phase 6.3C cutover (this flag goes
	// away then).
	UseHTTPMCP bool `toml:"use_http_mcp"`

	// OpenSessionStaleHours is the age threshold (in hours, measured from
	// started_at) at which the startup reaper marks open chat sessions as
	// ended. A session with ended_at = 0 that is older than this threshold
	// is assumed to be orphaned (crash, daemon restart, mid-turn error) and
	// is closed with the current wall time. Default 1. Set to 0 to disable
	// the reaper entirely.
	OpenSessionStaleHours int `toml:"open_session_stale_hours"`
}

// Scavenger configures Hive's integration with the Scavenger
// dependency-graph + session-memory engine. Phase 2a wires the worker
// subprocess to scavenger's MCP bridge + Claude Code plugin so claude
// gets auto-injected capsules on Read and explicit MCP tools.
type Scavenger struct {
	// Enabled toggles the whole integration. When false, worker
	// subprocesses are spawned without --plugin-dir or scavenger MCP
	// entries, and the daemon does not start/stop scavenger.
	Enabled bool `toml:"enabled"`

	// Binary is the path to (or PATH-resolvable name of) the scavenger
	// CLI. Default "scavenger".
	Binary string `toml:"binary"`

	// PluginDir is the path (relative to the worker's worktree, or
	// absolute) where Hive expects scavenger's Claude Code plugin to
	// live. Passed to claude as --plugin-dir when Enabled.
	PluginDir string `toml:"plugin_dir"`

	// MCPSocketEnv names the environment variable scavenger's MCP
	// bridge reads to find its daemon socket. Hive sets it on worker
	// subprocesses when Enabled.
	MCPSocketEnv string `toml:"mcp_socket_env"`

	// IndexWorktreeOnRun enables the per-run model: each run indexes its
	// worktree and starts a worktree-scoped daemon before the worker
	// launches. Supersedes the old single global daemon.
	IndexWorktreeOnRun bool `toml:"index_worktree_on_run"`

	// MaxConcurrentDaemons caps simultaneous per-run scavenger daemons
	// (resource guard). 0 = unlimited. When the cap is hit, the run skips
	// its daemon (index-only; degraded in-run freshness) and proceeds.
	MaxConcurrentDaemons int `toml:"max_concurrent_daemons"`

	// IndexTimeoutSeconds bounds a single per-run `scavenger index`. On
	// timeout the index is abandoned (non-fatal) and the run proceeds.
	IndexTimeoutSeconds int `toml:"index_timeout_seconds"`
}

type ClaudeCLI struct {
	Binary        string `toml:"binary"`
	PinnedVersion string `toml:"pinned_version"`
}

type Anthropic struct {
	RetryMaxAttempts                    int `toml:"retry_max_attempts"`
	RetryBackoffInitialMS               int `toml:"retry_backoff_initial_ms"`
	RetryBackoffMaxMS                   int `toml:"retry_backoff_max_ms"`
	PausePipelineAfterConsecutiveErrors int `toml:"pause_pipeline_after_consecutive_errors"`
}

type Sources struct {
	Inbox  SourceConfig `toml:"inbox"`
	GitHub SourceConfig `toml:"github"`
	Linear LinearConfig `toml:"linear"`
}

type SourceConfig struct {
	SyncIntervalMinutes int `toml:"sync_interval_minutes"`
}

type LinearConfig struct {
	SyncIntervalMinutes int    `toml:"sync_interval_minutes"`
	APIKeyEnv           string `toml:"api_key_env"`
}

// RepoKey derives the stable, human-greppable key for a repo's config layer:
// "<basename>-<8-hex-of-sha256(filepath.Clean(abspath))>" (e.g. "sidecar_ai-7f3ab210").
// The hash is over the cleaned path so trailing slashes / "." normalize to the
// same key. Empty path → "" (caller then merges no repo layer).
// The path is assumed to be a valid filesystem path; only leading/trailing
// whitespace is stripped (a "/" root yields the unusual key "/-<hash>", which
// callers never legitimately produce).
func RepoKey(repoPath string) string {
	p := strings.TrimSpace(repoPath)
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	sum := sha256.Sum256([]byte(clean))
	return filepath.Base(clean) + "-" + hex.EncodeToString(sum[:])[:8]
}

type LoadOptions struct {
	ConfigDir   string
	RepoKey     string // optional; merges repos/<RepoKey>/config.toml between global and project
	ProjectSlug string
	SkipEnv     bool
}

func Load(opts LoadOptions) (*Config, error) {
	cfg := Default()

	dir := opts.ConfigDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		dir = filepath.Join(home, ".hive")
	}

	globalPath := filepath.Join(dir, "config.toml")
	if err := mergeTOMLIfExists(cfg, globalPath); err != nil {
		return nil, fmt.Errorf("load global: %w", err)
	}

	if opts.RepoKey != "" {
		repoPath := filepath.Join(dir, "repos", opts.RepoKey, "config.toml")
		if err := mergeTOMLIfExists(cfg, repoPath); err != nil {
			return nil, fmt.Errorf("load repo %s: %w", opts.RepoKey, err)
		}
	}

	if opts.ProjectSlug != "" {
		projPath := filepath.Join(dir, "projects", opts.ProjectSlug, "config.toml")
		if err := mergeTOMLIfExists(cfg, projPath); err != nil {
			return nil, fmt.Errorf("load project %s: %w", opts.ProjectSlug, err)
		}
	}

	if !opts.SkipEnv {
		if err := applyEnvOverrides(cfg); err != nil {
			return nil, fmt.Errorf("env overrides: %w", err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config invalid: %w", err)
	}
	return cfg, nil
}

func mergeTOMLIfExists(cfg *Config, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := toml.DecodeFile(path, cfg)
	return err
}
