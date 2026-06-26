package config

// DefaultConfigTOML is the template written by `hive init` to
// ~/.hive/config.toml. Mirrors Config (config.go) section-for-section.
//
// IMPORTANT: when adding a new section to Config, update this template.
// TestDefaultConfigTOMLCoversAllSections enforces this via reflection,
// and TestDefaultConfigTOMLParses enforces clean toml.Decode into Config.
//
// Values here mirror config.Default(); tune per project in
// ~/.hive/projects/<slug>/config.toml.
const DefaultConfigTOML = `# ~/.hive/config.toml — Hive daemon configuration
# Created by ` + "`hive init`" + `. Edit freely; the daemon re-reads
# on each start. Run ` + "`hive doctor`" + ` to validate.
#
# Precedence (low → high): compiled defaults → this file →
# ~/.hive/projects/<slug>/config.toml → HIVE_* env vars → CLI flags.

[concurrency]
# Daemon-wide concurrency caps for run scheduling.
# max_workers is the global cap; per_repo_cap is the per-project cap
# when conflict_guard is enabled; per_repo_cap_no_guard is the cap
# when conflict_guard is disabled. finish_branch_cap and plan_cap
# additionally cap those pipeline kinds.
max_workers           = 3
per_repo_cap          = 2
per_repo_cap_no_guard = 1
finish_branch_cap     = 1
plan_cap              = 2

[costs]
# Per-run and per-day USD caps; warn when usage crosses alert_at_pct.
cap_per_run_usd = 10.0
cap_per_day_usd = 50.0
alert_at_pct    = 80

[models]
# Ladder = model fallback order on rate-limit / failure escalation.
worker_ladder            = ["claude-sonnet-4-6", "claude-sonnet-4-6", "claude-opus-4-7"]
reviewer_ladder          = ["claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-7"]
classifier               = "claude-haiku-4-5"
# Chat agent default model (Haiku) + reasoning escalation (Sonnet).
chat_default             = "claude-haiku-4-5"
chat_reasoning           = "claude-sonnet-4-6"
# Model for roadmap.decompose. Empty defers to the claude CLI default.
decompose_model          = "claude-sonnet-4-6"
# Model for hive project graduate Stage-4 audit. Empty defers to the claude CLI default.
graduate_validator_model = ""

[pipelines]
# Per-pipeline configuration. Each pipeline has its own subsection below.

[pipelines.build]
# Build pipeline = implement → review loop, then test + validate.
max_iterations          = 3
prefetch_budget_tokens  = 3000
stage_timeout_minutes   = 20
conflict_hop_depth      = 1
# Shell command run after review APPROVE; empty skips the stage.
test_command                  = "go test ./..."
validate_command              = "go build ./... && go vet ./..."
test_stage_timeout_minutes    = 10
validate_stage_timeout_minutes = 5

[pipelines.build.documenter]
# Optional documenter stage runs after validate passes; produces
# docs + changelog. Disable per project if you don't want it.
enabled               = true
model                 = "claude-sonnet-4-6"
stage_timeout_minutes = 10
code_comments         = false
update_readme         = false
# Skills the documenter loads into its context (missing skills are
# silently skipped; opt in per project).
skills_to_load        = []

[pipelines.debug]
max_iterations     = 3
conflict_hop_depth = 0

[pipelines.plan]
max_iterations     = 3
conflict_hop_depth = 2

[pipelines.finish_branch]
# Finish-branch runs shell gates then create-pr + ci-monitor.
# Empty command skips that stage. Tune commands per project.
typecheck_command          = "go build ./..."
lint_command               = "go vet ./..."
format_command             = ""
test_command               = "go test ./..."
# prepare_command installs deps in a fresh worktree before the gates above.
# Only used by 'hive project graduate' (the gates assume deps; the per-task
# build stage installs them, but graduate has no build stage). Empty = skip.
prepare_command            = ""
create_pr_command          = "git push -u origin HEAD && gh pr create --fill --base {{target_branch}}"
ci_monitor_command         = "gh pr checks --watch"
stage_timeout_minutes      = 10
ci_monitor_timeout_minutes = 30
conflict_hop_depth         = 0
shell_output_max_bytes     = 0
# Max retry attempts when a local gate fails (spawns a child Build run).
max_fix_attempts           = 2

[pipelines.resolve]
# Conflict-resolver pipeline. Reproduces a merge conflict in a worktree,
# resolves it with a bounded validate-gated loop, and pushes only when green.
# Inherits the build pipeline's test_command/validate_command/worker_ladder.
auto                  = false   # auto-dispatch on an auto-merge content conflict
max_iterations        = 5
stage_timeout_minutes = 20

[pipelines.graduate]
# Completion-audit ensemble size (K). Each audit is an independent read-only
# pass; findings are unioned + verified. 1 = legacy single-pass. <=0 uses the
# compiled default (5).
audit_runs = 5
# seam_audit = true   # deterministic unwired-seam detection (routes/RPC/events); default on
# phase_audit = true   # per-roadmap-phase deliverable verification; default on
# Teach the seam extractor a non-standard stack (all optional, additive to the built-ins):
# [pipelines.graduate.seam_patterns]
# router_receivers = ["r"]                 # extra router var names (e.g. chi router r.Get)
# call_verbs       = ["execute", "trigger"] # extra cross-boundary call verbs
# reg_verbs        = ["register"]           # extra registration verbs
# exclude_globs    = ["generated/*"]        # paths to skip

[predictor]
# Predictor + capsule pre-fetch (Phase 2b). When false, the daemon
# dispatches without prediction (pre-2b behavior).
enabled                  = true
bundle_token_cap         = 6000
max_candidates           = 10
per_call_timeout_seconds = 5
haiku_timeout_seconds    = 10
# Empty inherits from models.classifier.
haiku_model              = "claude-haiku-4-5"
# Metrics-driven auto-degrade (Phase 2b.5).
precision_kill_threshold = 0.5
rolling_window_days      = 30
force_enable             = false

[conflict_guard]
# When false, the predictor still runs (for metrics) but the
# scheduler doesn't queue conflicting runs — they all dispatch
# concurrently. Default true.
enabled = true

[stall_detection]
# Layered stall detection (Phase 3.2). Zero disables a layer.
event_heartbeat_seconds   = 60
tool_call_max_minutes     = 5
# implement-stage: kill if the worktree diff shows no new content for this many
# minutes (0 disables; 20m StageTimeout is the hard cap).
implement_stagnation_minutes = 8
loop_check_after_iter     = 1
loop_similarity_threshold = 0.85
notify_on_stall           = false

[approvals]
# When true, claude subprocesses gate tool-use through the daemon
# via --permission-prompt-tool. Required for walk-away runs.
enabled              = false
# "ask" blocks for a live TUI decision (timeout -> deny); "deny"
# fail-closes immediately.
mode                 = "ask"
hook_timeout_seconds = 300
# Tools auto-allowed for the worker (implementer) subprocess.
default_allow_worker = ["Read", "Edit", "Write", "MultiEdit", "Grep", "Glob"]
# Safe-by-default Bash allow-list (globs; * spans any chars). Read
# + inspect + project toolchains + read-only git. Destructive or
# network commands (rm, mv, curl, wget, chmod, kill, sudo, git
# push/commit) intentionally absent — opt in per project.
default_allow_worker_bash = [
    "ls*", "cat *", "head *", "tail *", "wc *", "find *", "grep *",
    "which *", "pwd*", "echo *", "sort *", "uniq *", "cut *", "tr *",
    "dirname *", "basename *", "realpath *", "stat *", "file *", "tree *",
    "diff *", "test *", "date*", "env*", "sed *", "awk *", "jq *",
    "go *", "gofmt *", "node *", "npm *", "pnpm *", "npx *",
    "python *", "python3 *", "pip *", "pytest *", "tsc *", "cargo *", "make *",
    "git status*", "git diff*", "git log*", "git show*", "git branch*",
    "git rev-parse*", "git ls-files*", "git stash list*",
]
default_allow_reviewer = ["Read", "Grep", "Glob"]

[chat]
# Provider for the chat agent:
#   - "claude-code" runs on your Claude Pro/Team subscription via
#     ` + "`claude -p`" + ` (no API spend).
#   - "api" uses ANTHROPIC_API_KEY directly (per-turn billing).
provider               = "api"
default_model          = "claude-haiku-4-5"
reasoning_model        = "claude-sonnet-4-6"
api_key_env            = "ANTHROPIC_API_KEY"
max_iters              = 8
# Tools that bypass the confirm gate (read-only by default).
auto_confirm           = [
    "hive_list_tasks", "hive_get_task", "hive_get_run",
    "hive_active_workers", "hive_cost_summary", "hive_status",
    "hive_search", "hive_show_diff", "hive_predict", "hive_attach_run",
]
# How long the daemon waits for a user confirm response before
# auto-rejecting. Default 300.
confirm_timeout_seconds = 300
# When true, chat MCP server is reached via the daemon's long-running
# HTTP listener; stdio subprocess is not spawned (Phase 6.3 toggle).
use_http_mcp           = false
# Age threshold (hours, measured from started_at) at which the
# startup reaper marks open chat sessions as ended. 0 disables.
open_session_stale_hours = 1

[scavenger]
# When true, daemon hooks scavenger's --plugin-dir into each stage
# subprocess. Requires the ` + "`scavenger`" + ` binary on PATH.
enabled                = true
binary                 = "scavenger"
# Path (relative to worktree, or absolute) of scavenger's Claude
# Code plugin. Passed to claude as --plugin-dir when enabled.
plugin_dir             = ".scavenger/claude-plugin"
# Env var name scavenger's MCP bridge reads to find its daemon socket.
mcp_socket_env         = "SCAVENGER_SOCK"
# Per-run model: index the worktree + start a worktree-scoped daemon before
# each worker launches (gives workers the full dependency graph).
index_worktree_on_run  = true
# Cap on simultaneous per-run scavenger daemons. 0 = unlimited.
max_concurrent_daemons = 8
# Timeout (seconds) for a single per-run ` + "`scavenger index`" + `.
index_timeout_seconds  = 120

[claude_cli]
# Path to (or name of) the claude CLI binary.
binary         = "claude"
# Empty inherits the user's installed version. Pin for repeatability.
pinned_version = ""

[anthropic]
# Retry policy for transient Anthropic API errors (chat agent + SDK).
retry_max_attempts                       = 5
retry_backoff_initial_ms                 = 500
retry_backoff_max_ms                     = 30000
# Pause a pipeline after this many consecutive errors (0 disables).
pause_pipeline_after_consecutive_errors  = 3

[sources]
# Per-source sync intervals + auth. Each source has its own subsection.

[sources.inbox]
# Local inbox source: ~/.hive/inbox/<project-slug>/*.md files.
sync_interval_minutes = 5

[sources.github]
# GitHub issues source (uses ` + "`gh`" + ` CLI). Bind via ` + "`hive sources bind`" + `.
sync_interval_minutes = 30

[sources.linear]
# Linear source (GraphQL). API key read from the env var below.
sync_interval_minutes = 15
api_key_env           = "LINEAR_API_KEY"

[llm]
# Provider for control-plane LLM ops (classifier-fallback, predictor):
#   - "cli" spawns ` + "`claude -p`" + ` (uses Claude Max subscription).
#   - "api" uses the Anthropic SDK with ANTHROPIC_API_KEY.
provider = "cli"

[tui]
# Per-subscriber bounded channel capacity.
event_buffer_size        = 1000
# When true, a dropped event triggers a synthetic ` + "`resync`" + ` event.
event_resync_on_overflow = true
# Cadence of daemon.heartbeat events. 0 disables.
heartbeat_seconds        = 5

[scheduler]
# When true, the scheduler tick auto-dispatches pending tasks.
# Phase 3.7.2 made this opt-in so smoke tests aren't surprised.
auto_dispatch = false
# How often the merge-poller checks awaiting_merge PRs (human_merge /
# auto_merge_on_green). 0 uses the 30s default; negative disables the poller.
merge_poll_interval_seconds = 0

[integration]
# Feature-branch integration loop: task work auto-integrates into a
# per-initiative branch (finish-branch → PR → CI → auto-merge).
# Empty feature_branch disables the feature entirely (default).
feature_branch      = ""
# When true, a successful build run auto-chains finish-branch → PR →
# CI → auto-merge in ANY dispatch mode. When false, integration only
# happens via ` + "`hive run finish`" + `.
task_auto_integrate = false
# gh pr merge strategy for task→feature merges: "squash" | "merge" | "rebase".
# Empty defaults to "merge".
merge_method        = ""
# When true, a GitHub-CI failure on a task PR triggers one auto-fix
# child run before the task goes needs_attention.
auto_fix_ci         = false
# Shell command to push an auto-fix commit to the PR branch before CI re-checks
# (only used when auto_fix_ci is true). Empty defaults to "git push origin HEAD".
ci_fix_push_command = ""

[cleanup]
# Reclaim per-run worktrees + scratch dirs + hive/<run> branches for terminal
# runs beyond the retention window. auto_sweep runs this on boot AND on a
# periodic loop (sweep_interval_minutes; negative = boot-only).
auto_sweep            = true
keep_last_runs        = 20
orphan_grace_minutes  = 60
clean_branches        = true
sweep_interval_minutes = 30
`
