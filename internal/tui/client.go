package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/tui/tabs"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// Client manages the daemon connection: streaming events.subscribe +
// request/response RPCs for initial state. Designed for goroutine
// lifecycle: Run starts the consumer; Close stops it.
type Client struct {
	SockPath string
	program  *tea.Program

	stopCh chan struct{}
}

// NewClient constructs a Client. The Bubbletea program is bound later
// via Bind (because the program is created in cmd_tui.go after the
// client).
func NewClient(sockPath string) *Client {
	return &Client{
		SockPath: sockPath,
		stopCh:   make(chan struct{}),
	}
}

// Bind attaches the program so the consumer goroutine can send msgs.
func (c *Client) Bind(p *tea.Program) { c.program = p }

// FetchInitialState calls daemon.status + task.list + project.list and
// returns an initialStateMsg. Used by the root model's Init cmd.
func (c *Client) FetchInitialState() tea.Msg {
	var msg initialStateMsg
	if raw, err := c.call("daemon.status", nil); err == nil {
		var status map[string]any
		_ = json.Unmarshal(raw, &status)
		msg.Status = status
	}
	// Seed BOTH the queued (pending) and needs-attention lanes. Without the
	// needs_attention filter, a task that failed before the TUI subscribed
	// never enters the snapshot, so the "Needs attention" lane stays empty
	// until a live event flips it.
	if raw, err := c.call("task.list", map[string]any{"statuses": []string{"pending", "needs_attention"}}); err == nil {
		var tasks []rpc.TaskView
		_ = json.Unmarshal(raw, &tasks)
		msg.Tasks = tasks
	}
	if raw, err := c.call("project.list", nil); err == nil {
		var projs []rpc.ProjectView
		_ = json.Unmarshal(raw, &projs)
		msg.Projects = projs
	}
	// For every sequenced project, fetch its sequence.status so the
	// snapshot has a per-project cache the Projects tab + Sequence modal
	// can render without a separate round-trip. A failed fetch for one
	// project is skipped — it'll refresh on the next sequence.* event.
	seqMap := map[string]*rpc.SeqStatusView{}
	for _, p := range msg.Projects {
		if p.DispatchMode != "sequenced" {
			continue
		}
		view, err := c.SequenceStatus(p.Slug)
		if err != nil || view == nil {
			continue
		}
		seqMap[p.ID] = view
	}
	msg.SequenceStatus = seqMap
	return msg
}

// SequenceStatus fetches the sequence.status view for a project by slug.
// Returns a typed *rpc.SeqStatusView (mirrors the daemon's seqStatusView).
func (c *Client) SequenceStatus(slug string) (*rpc.SeqStatusView, error) {
	raw, err := c.call(rpc.MethodSequenceStatus, map[string]any{"project_slug": slug})
	if err != nil {
		return nil, err
	}
	var view rpc.SeqStatusView
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

// RoadmapContent fetches the project's roadmap markdown, read branch-aware by
// the daemon (working tree → feature/target branch). Used by the roadmap viewer
// as a fallback when its own working-tree read misses (shared repo on another
// branch).
func (c *Client) RoadmapContent(slug string) (string, error) {
	raw, err := c.call(rpc.MethodRoadmapContent, map[string]any{"project_slug": slug})
	if err != nil {
		return "", err
	}
	var v struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v.Content, nil
}

// SequenceEnable turns on sequenced dispatch for a project, optionally
// pinning a target branch + advancement policy (both empty = daemon
// defaults).
func (c *Client) SequenceEnable(slug, target, policy string) (map[string]any, error) {
	params := map[string]any{"project_slug": slug}
	if target != "" {
		params["target_branch"] = target
	}
	if policy != "" {
		params["policy"] = policy
	}
	return c.callObject(rpc.MethodSequenceEnable, params)
}

// SequenceDisable reverts a project to manual dispatch (daemon sweeps any
// awaiting_merge gate state to satisfied).
func (c *Client) SequenceDisable(slug string) (map[string]any, error) {
	return c.callObject(rpc.MethodSequenceDisable, map[string]any{"project_slug": slug})
}

// SequencePause pauses sequenced dispatch (no new phases advance).
func (c *Client) SequencePause(slug string) (map[string]any, error) {
	return c.callObject(rpc.MethodSequencePause, map[string]any{"project_slug": slug})
}

// SequenceResume resumes a paused sequenced dispatcher.
func (c *Client) SequenceResume(slug string) (map[string]any, error) {
	return c.callObject(rpc.MethodSequenceResume, map[string]any{"project_slug": slug})
}

// SequenceAdvance manually advances the active phase (human_merge policy).
func (c *Client) SequenceAdvance(slug string) (map[string]any, error) {
	return c.callObject(rpc.MethodSequenceAdvance, map[string]any{"project_slug": slug})
}

// SequenceSkip skips a single blocked/pending task in the sequence by task ID.
func (c *Client) SequenceSkip(taskID string) (map[string]any, error) {
	return c.callObject(rpc.MethodSequenceSkip, map[string]any{"task_id": taskID})
}

// SequenceComplete marks a roadmap phase operator-complete (every task in it
// must be satisfied/skipped) so the dispatcher advances. Mirrors
// `hive sequence complete <project> --phase N`.
func (c *Client) SequenceComplete(slug, phase string) (map[string]any, error) {
	return c.callObject(rpc.MethodSequenceComplete, map[string]any{"project_slug": slug, "phase": phase})
}

// SourcesSync pulls bound sources into tasks now. An empty source syncs every
// bound source; a source kind ("github"|"linear"|"inbox") restricts it. The
// sync is global (by source kind, not per-project) — mirrors `hive sync`. The
// network fetch can take a while, so it uses a 60s deadline.
func (c *Client) SourcesSync(source string) (map[string]any, error) {
	params := map[string]any{}
	if source != "" {
		params["source"] = source
	}
	return c.callObjectWithTimeout(rpc.MethodSourcesSync, params, 60*time.Second)
}

// RoadmapSyncLinear mirrors the project's roadmap into Linear (document +
// per-phase milestones). Mirrors `hive roadmap sync-linear <project>`. No-op
// unless the project's Linear source is write-back bound. The upsert walks
// every phase over the network, so it uses a 120s deadline (matching the CLI).
func (c *Client) RoadmapSyncLinear(slug string) (map[string]any, error) {
	return c.callObjectWithTimeout(rpc.MethodRoadmapSyncLinear, map[string]any{"project_slug": slug}, 120*time.Second)
}

// Cleanup fires cleanup.run (GC of per-run worktrees, scratch dirs, and
// hive/<run> branches for old terminal runs). Mirrors `hive clean`:
//
//   - dryRun true  → report what WOULD be reclaimed without removing anything
//     (the Clean modal's preview phase).
//   - keepLast nil → use the daemon's [cleanup] keep_last_runs default; a
//     non-nil pointer overrides it for this run.
//   - noBranches true → set "branches": false so hive/<run> branches are left
//     in place (the CLI's --no-branches). When false the param is omitted so
//     the daemon applies its default (delete branches).
//
// Uses a 30s deadline because the GC walks the filesystem (worktree + scratch
// dirs), which can take longer than the default 10s on a large state dir.
func (c *Client) Cleanup(dryRun bool, keepLast *int, noBranches bool) (map[string]any, error) {
	params := map[string]any{"dry_run": dryRun}
	if keepLast != nil {
		params["keep_last"] = *keepLast
	}
	if noBranches {
		params["branches"] = false
	}
	return c.callObjectWithTimeout(rpc.MethodCleanupRun, params, 30*time.Second)
}

// taskDecomposeResultWire is the wire shape of task.decompose's result.
// Mirrors decomposeResultWire in cmd/hive/cmd_decompose.go (the daemon's
// DecomposeResult). Kept TUI-local so the confirm modal can render the
// typed decompose.ProposedSubtask fields directly.
type taskDecomposeResultWire struct {
	Subtasks     []decompose.ProposedSubtask `json:"subtasks"`
	Model        string                      `json:"model"`
	CostUSD      float64                     `json:"cost_usd"`
	InputTokens  int                         `json:"input_tokens"`
	OutputTokens int                         `json:"output_tokens"`
}

// TaskDecompose fires task.decompose for the given task and returns the
// proposed sub-task breakdown. Mirrors `hive decompose <task-id>`'s first
// RPC. maxSubtasks <= 0 omits the param so the daemon applies its default
// (10, hard-capped at 20 server-side). Uses a 120s read deadline because
// it's a Sonnet tool-use turn (the CLI uses the same deadline).
func (c *Client) TaskDecompose(taskID string, maxSubtasks int) (*decompose.Result, error) {
	params := map[string]any{"task_id": taskID}
	if maxSubtasks > 0 {
		params["max_subtasks"] = maxSubtasks
	}
	raw, err := c.callWithTimeout(rpc.MethodDecompose, params, 120*time.Second)
	if err != nil {
		return nil, err
	}
	var w taskDecomposeResultWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("parse decompose result: %w", err)
	}
	return &decompose.Result{
		Subtasks:     w.Subtasks,
		Model:        w.Model,
		CostUSD:      w.CostUSD,
		InputTokens:  w.InputTokens,
		OutputTokens: w.OutputTokens,
	}, nil
}

// TaskDecomposeApply fires task.decompose_apply to insert the confirmed
// sub-tasks as children of taskID. Mirrors the CLI's apply call: the param
// envelope is {"parent_task_id": id, "subtasks": [...]} (matched to
// DecomposeApplyParams in internal/daemon/rpc_handlers.go).
func (c *Client) TaskDecomposeApply(taskID string, subtasks []decompose.ProposedSubtask) (map[string]any, error) {
	return c.callObject(rpc.MethodDecomposeApply, map[string]any{
		"parent_task_id": taskID,
		"subtasks":       subtasks,
	})
}

// call performs a single one-shot RPC and returns the raw result.
// Uses a 10s read deadline; use callWithTimeout for longer-running RPCs
// (e.g. roadmap.decompose, which fires a Sonnet tool turn that can take
// 30-60s).
func (c *Client) call(method string, params any) (json.RawMessage, error) {
	return c.callWithTimeout(method, params, 10*time.Second)
}

// callWithTimeout is call() with a configurable read deadline. Dial
// timeout stays at 5s — only the response wait varies.
func (c *Client) callWithTimeout(method string, params any, readTimeout time.Duration) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", c.SockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	payload := map[string]any{
		"id":     fmt.Sprintf("tui-%d", time.Now().UnixNano()),
		"method": method,
		"params": params,
	}
	if params == nil {
		payload["params"] = map[string]any{}
	}
	raw, _ := json.Marshal(payload)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	rdr := bufio.NewReader(conn)
	line, err := rdr.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc: %s", resp.Error.Message)
	}
	return resp.Result, nil
}

// callObject is a thin wrapper around call that unmarshals the result
// into a map[string]any. The bulk of TUI RPC wrappers want this shape;
// keeps them one-liners.
func (c *Client) callObject(method string, params any) (map[string]any, error) {
	raw, err := c.call(method, params)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// callObjectWithTimeout is callObject with a configurable read deadline.
// Used by roadmap.decompose (120s — Sonnet tool turn).
func (c *Client) callObjectWithTimeout(method string, params any, readTimeout time.Duration) (map[string]any, error) {
	raw, err := c.callWithTimeout(method, params, readTimeout)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// FetchCostSummary calls cost.summary and returns a costSummaryMsg.
// Used by the Costs tab on activation + periodic refresh.
func (c *Client) FetchCostSummary() tea.Msg {
	raw, err := c.call("cost.summary", nil)
	if err != nil {
		return costSummaryMsg{}
	}
	var view rpc.CostSummaryView
	_ = json.Unmarshal(raw, &view)
	return costSummaryMsg{Summary: view}
}

// AddProject calls project.add with optional dispatch_mode (manual|auto_all) +
// the [integration] block. dispatch_mode "" leaves the daemon default;
// "sequenced" is rejected daemon-side (no roadmap exists at create). Integration
// params are always sent so they round-trip on create.
func (c *Client) AddProject(slug, name, repoPath, dispatchMode, target, featureBranch, mergeMethod string, taskAutoIntegrate, autoFixCI bool) (map[string]any, error) {
	params := map[string]any{
		"slug":                slug,
		"name":                name,
		"repo_path":           repoPath,
		"feature_branch":      featureBranch,
		"merge_method":        mergeMethod,
		"task_auto_integrate": taskAutoIntegrate,
		"auto_fix_ci":         autoFixCI,
	}
	if dispatchMode != "" {
		params["dispatch_mode"] = dispatchMode
	}
	if target != "" {
		params["target_branch"] = target
	}
	return c.callObject("project.add", params)
}

// EditProject calls project.edit. name and repo_path are passed as-is; the
// daemon treats empty strings as "leave unchanged" per its existing semantics.
// Slug is immutable (linkage to tasks/runs/sources). dispatch_mode is sent when
// non-empty; target_branch + policy ride along only when the mode is sequenced.
// Integration params are always sent so they round-trip on save.
func (c *Client) EditProject(slug, name, repoPath, status, dispatchMode, target, policy, featureBranch, mergeMethod string, taskAutoIntegrate, autoFixCI bool) (map[string]any, error) {
	params := map[string]any{
		"slug":                slug,
		"name":                name,
		"repo_path":           repoPath,
		"feature_branch":      featureBranch,
		"merge_method":        mergeMethod,
		"task_auto_integrate": taskAutoIntegrate,
		"auto_fix_ci":         autoFixCI,
	}
	// status is sent only when set, preserving the daemon's nil-means-unchanged
	// semantics (EditProjectParams.Status is a *string). The edit modal always
	// supplies one; other callers omit it to leave the status untouched.
	if status != "" {
		params["status"] = status
	}
	if target != "" {
		params["target_branch"] = target // [scheduler] base; not sequenced-only
	}
	if dispatchMode != "" {
		params["dispatch_mode"] = dispatchMode
		if dispatchMode == "sequenced" {
			params["policy"] = policy
		}
	}
	return c.callObject(rpc.MethodEditProject, params)
}

// DeleteProject calls project.delete. The daemon cascades to tasks/runs
// per its existing semantics — callers SHOULD have surfaced a typed
// confirm UI before reaching this method.
func (c *Client) DeleteProject(slug string) (map[string]any, error) {
	return c.callObject(rpc.MethodDeleteProject, map[string]any{"slug": slug})
}

// SourcesList returns the current bind state for the project's sources.
// Wire shape (per daemon handleSourcesList): the response is the project's
// Sources map directly, keyed by source name (e.g. "github" / "linear" /
// "inbox"), with each value being the source-specific binding map. A key
// absent from the map means that source is unbound. Example:
//
//	{"github": {"repo": "owner/name"}, "linear": {"teams": ["HBA"]}}
//
// The 8.C.2 Sources modal expands this into three rows (github / linear /
// inbox), filling Bound=false for any source key not in the response.
//
// IMPORTANT: the daemon parameter is "slug" (not "project_slug"); earlier
// stubs of this wrapper sent "project_slug" and were silently rejected
// (slug required) — fixed in T4 alongside SourcesBind/Unbind below.
func (c *Client) SourcesList(projectSlug string) (map[string]any, error) {
	return c.callObject(rpc.MethodSourcesList, map[string]any{"slug": projectSlug})
}

// SourcesBind binds a source kind ("github"|"linear"|"inbox") to the
// project with the kind-specific binding map. Per-source required keys
// (mirroring cmd/hive/cmd_sources.go + internal/sources/):
//
//   - github: {"repo": "owner/name"} (plus optional labels []string)
//   - linear: {"teams": []string{"TEAM_KEY", ...}}  (NOT a single team_key)
//   - inbox:  {} — daemon auto-creates ~/.hive/inbox/<slug>/
//
// Daemon param names: "slug" / "source" / "binding" (NOT project_slug /
// kind / config — wire-shape discovered during T4 implementation while
// reading internal/daemon/rpc_handlers.go::SourcesBindParams).
func (c *Client) SourcesBind(projectSlug, kind string, binding map[string]any) (map[string]any, error) {
	return c.callObject(rpc.MethodSourcesBind, map[string]any{
		"slug":    projectSlug,
		"source":  kind,
		"binding": binding,
	})
}

// SourcesUnbind removes a source binding from the project. The daemon
// also closes any open source-derived tasks per its reconcile semantics.
// Daemon param names: "slug" / "source" (NOT project_slug / kind — see
// SourcesBind doc above).
func (c *Client) SourcesUnbind(projectSlug, kind string) (map[string]any, error) {
	return c.callObject(rpc.MethodSourcesUnbind, map[string]any{
		"slug":   projectSlug,
		"source": kind,
	})
}

// RemediateHealth fires health.remediate to rebase the project's feature
// branch onto its target (action "rebase") or merge the target in (action
// "merge"). Returns the daemon's fresh health payload (behind/ahead/clean/
// report/...). Local only — no push.
func (c *Client) RemediateHealth(projectSlug, action string) (map[string]any, error) {
	return c.callObject(rpc.MethodHealthRemediate, map[string]any{
		"project_slug": projectSlug,
		"action":       action,
	})
}

// RoadmapDecomposeStart fires the async roadmap.decompose for one phase and
// returns the decompose_id the daemon assigned. The proposal (or error)
// arrives later as a decompose.proposed / decompose.failed event on the
// already-open event stream — so there is no read deadline on the model turn.
// A short 10s ack deadline covers only validation + job registration.
//
// maxSubtasks <= 0 lets the daemon apply its default.
func (c *Client) RoadmapDecomposeStart(projectSlug, phase string, maxSubtasks int) (string, error) {
	params := map[string]any{"project_slug": projectSlug, "phase": phase}
	if maxSubtasks > 0 {
		params["max_subtasks"] = maxSubtasks
	}
	obj, err := c.callObjectWithTimeout(rpc.MethodRoadmapDecompose, params, 10*time.Second)
	if err != nil {
		return "", err
	}
	id, _ := obj["decompose_id"].(string)
	if id == "" {
		return "", fmt.Errorf("daemon did not return a decompose_id")
	}
	return id, nil
}

// ProjectGraduateStart fires the async project.graduate RPC and returns the
// graduate_id the daemon assigned. Lifecycle (progress / verdict / done /
// failed) arrives later as graduate.* events on the already-open event stream,
// so there is no read deadline on the model turn — a short 10s ack deadline
// covers only validation + job registration. Mirrors RoadmapDecomposeStart.
//
// dryRun runs checks + audit and prints the verdict + PR body without opening a
// PR; force bypasses the audit/build gate but still opens the PR; draft opens
// the PR as a draft.
func (c *Client) ProjectGraduateStart(slug string, force, draft, dryRun bool) (string, error) {
	params := map[string]any{
		"project_slug": slug,
		"force":        force,
		"draft":        draft,
		"dry_run":      dryRun,
	}
	obj, err := c.callObjectWithTimeout(rpc.MethodProjectGraduate, params, 10*time.Second)
	if err != nil {
		return "", err
	}
	id, _ := obj["graduate_id"].(string)
	if id == "" {
		return "", fmt.Errorf("daemon did not return a graduate_id")
	}
	return id, nil
}

// GraduateStatus fetches the last persisted graduate result for a project.
// Returns (nil, nil) when no run has been recorded (exists:false).
func (c *Client) GraduateStatus(slug string) (*graduate.GraduateResult, error) {
	obj, err := c.callObject(rpc.MethodProjectGraduateStatus, map[string]any{"project_slug": slug})
	if err != nil {
		return nil, err
	}
	exists, _ := obj["exists"].(bool)
	if !exists {
		return nil, nil
	}
	rb, err := json.Marshal(obj["result"])
	if err != nil {
		return nil, err
	}
	var rec graduate.GraduateResult
	if err := json.Unmarshal(rb, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// RoadmapDecomposeApply applies an approved roadmap-decompose proposal set
// (insert/merge/pull) in one daemon call. Uses callObject (10s default)
// because apply is fast — it writes tasks/merges synchronously rather than
// driving a Sonnet tool turn.
func (c *Client) RoadmapDecomposeApply(params map[string]any) (map[string]any, error) {
	return c.callObject(rpc.MethodRoadmapDecomposeApply, params)
}

// AddTask calls task.add.
func (c *Client) AddTask(projectSlug, title, body, pipeline string) (map[string]any, error) {
	params := map[string]any{"project_slug": projectSlug, "title": title}
	if body != "" {
		params["body"] = body
	}
	if pipeline != "" {
		params["pipeline"] = pipeline
	}
	raw, err := c.call("task.add", params)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// AddTaskWithMetadata is task.add with explicit priority + metadata. The
// roadmap decompose flow (8.C.2 T5) inserts each proposed subtask with a
// metadata map linking back to the source phase (roadmap_phase, roadmap_path,
// spec_path), so the linkage survives in store.Task.Metadata for later
// queries (e.g. the CLI's `hive roadmap decompose` guard against double-
// decomposing a phase).
//
// Separate from AddTask so the existing two-callsite (new_task modal +
// chat agent's hive_add_task tool) keeps its narrow 4-arg signature.
// Returns the daemon error verbatim — the batch loop in handleModalSubmit
// collects per-task errors so a single failed insert doesn't strand the
// rest of the batch.
func (c *Client) AddTaskWithMetadata(projectSlug, title, body, pipeline, priority string, metadata map[string]string) error {
	params := map[string]any{"project_slug": projectSlug, "title": title}
	if body != "" {
		params["body"] = body
	}
	if pipeline != "" {
		params["pipeline"] = pipeline
	}
	if priority != "" {
		params["priority"] = priority
	}
	if len(metadata) > 0 {
		params["metadata"] = metadata
	}
	_, err := c.call("task.add", params)
	return err
}

// FetchRunStages calls run.stages and returns a runStagesMsg. Used by
// the root model when drill-in is entered for a run that may not have
// stages in the snapshot yet (run was dispatched before TUI started,
// or only the first stage event has arrived).
func (c *Client) FetchRunStages(runID string) tea.Msg {
	raw, err := c.call("run.stages", map[string]any{"run_id": runID})
	if err != nil {
		return runStagesMsg{RunID: runID}
	}
	var stages []rpc.StageRow
	_ = json.Unmarshal(raw, &stages)
	return runStagesMsg{RunID: runID, Stages: stages}
}

// GetTask calls task.get and returns the full task (incl body).
func (c *Client) GetTask(taskID string) (map[string]any, error) {
	raw, err := c.call("task.get", map[string]any{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// EditTask calls task.edit (update title + body).
func (c *Client) EditTask(taskID, title, body string) (map[string]any, error) {
	raw, err := c.call("task.edit", map[string]any{"task_id": taskID, "title": title, "body": body})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// DeleteTask calls task.delete.
func (c *Client) DeleteTask(taskID string) (map[string]any, error) {
	raw, err := c.call("task.delete", map[string]any{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// ProjectRemediate creates inbox tasks from the project's last graduate audit's
// confirmed findings. Returns {created:[...], skipped:N}.
func (c *Client) ProjectRemediate(slug string) (map[string]any, error) {
	raw, err := c.call("project.remediate", map[string]any{"project_slug": slug})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// RunNow calls run.now to dispatch a pending task immediately.
func (c *Client) RunNow(taskID string) (map[string]any, error) {
	raw, err := c.call("run.now", map[string]any{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// ResolveTask calls resolve.now to trigger the conflict resolver for a stuck
// (needs_attention) task. Fire-and-forget from the TUI perspective — the
// resolver runs asynchronously on the daemon; results surface via the normal
// run.* event stream.
func (c *Client) ResolveTask(taskID string) (map[string]any, error) {
	return c.callObject(rpc.MethodResolveNow, map[string]any{"task_id": taskID})
}

// MergeRetry calls merge.retry to recover a task parked at merge_failed
// (reconcile if the PR already merged, else re-arm the merge queue).
func (c *Client) MergeRetry(taskID string) (map[string]any, error) {
	return c.callObject(rpc.MethodMergeRetry, map[string]any{"task_id": taskID})
}

// AbandonRun calls run.abandon.
func (c *Client) AbandonRun(runID string) (map[string]any, error) {
	raw, err := c.call("run.abandon", map[string]any{"run_id": runID})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// ResolveApproval calls approval.resolve to decide a pending approval
// (Phase 4.6). remember adds an allow/deny rule for the tool.
func (c *Client) ResolveApproval(approvalID, decision string, remember bool, toolName, argMatcher string) (map[string]any, error) {
	params := map[string]any{"approval_id": approvalID, "decision": decision}
	if remember {
		params["remember"] = true
		params["tool_name"] = toolName
		if argMatcher != "" {
			params["arg_matcher"] = argMatcher
		}
	}
	raw, err := c.call("approval.resolve", params)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// Run subscribes + drains events. On disconnect, signals daemonDownMsg
// and retries on a backoff schedule until Close is called. Should run
// in its own goroutine.
func (c *Client) Run() {
	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	attempt := 0
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		err := c.runOnce()
		if err != nil && c.program != nil {
			c.program.Send(daemonDownMsg{Err: err})
		}
		idx := attempt
		if idx >= len(backoff) {
			idx = len(backoff) - 1
		}
		attempt++
		select {
		case <-time.After(backoff[idx]):
		case <-c.stopCh:
			return
		}
	}
}

// runOnce holds one connection. Returns when the connection dies.
func (c *Client) runOnce() error {
	conn, err := net.DialTimeout("unix", c.SockPath, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	req := fmt.Sprintf(`{"id":"tui-stream","method":"events.subscribe","params":{}}%s`, "\n")
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}
	rdr := bufio.NewReader(conn)
	if _, err := rdr.ReadBytes('\n'); err != nil { // ack
		return err
	}
	if c.program != nil {
		c.program.Send(daemonReconnectedMsg{})
	}
	for {
		line, err := rdr.ReadBytes('\n')
		if err != nil {
			return err
		}
		var ev rpc.EventMessage
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if c.program != nil {
			c.program.Send(eventMsg{Event: ev})
		}
	}
}

// Close stops the consumer.
func (c *Client) Close() {
	select {
	case <-c.stopCh:
		// already closed
	default:
		close(c.stopCh)
	}
}

// ChatConfirmReq carries everything chat.confirm needs. Reason is used
// only when Approve=false (overrides the default "declined by user").
// EditedInput is used only when Approve=true; nil means "tool runs with
// original args".
type ChatConfirmReq struct {
	SessionID   string
	ToolCallID  string
	Approve     bool
	Reason      string
	EditedInput json.RawMessage
}

// ChatConfirm resolves a pending tool-use approval gate over the
// chat.confirm RPC. The connection is opened fresh per call to avoid
// contending with an in-flight chat.send stream (which holds its conn
// one-way).
func (c *Client) ChatConfirm(req ChatConfirmReq) error {
	params := map[string]any{
		"session_id":   req.SessionID,
		"tool_call_id": req.ToolCallID,
		"approve":      req.Approve,
	}
	if req.Reason != "" {
		params["reason"] = req.Reason
	}
	if len(req.EditedInput) > 0 {
		params["edited_input"] = req.EditedInput
	}
	_, err := c.call(rpc.MethodChatConfirm, params)
	return err
}

// ChatSetName updates a session's display name via the chat.set_name RPC.
func (c *Client) ChatSetName(sessionID, name string) error {
	_, err := c.call(rpc.MethodChatSetName, map[string]any{
		"session_id": sessionID,
		"name":       name,
	})
	return err
}

// ChatDelete removes a chat session and all its messages atomically via
// chat.delete. Daemon-side also evicts the in-memory scope cache and
// reaps the on-disk per-session scratch dir (claude-code provider only;
// the SDK agent has nothing to evict).
func (c *Client) ChatDelete(sessionID string) error {
	_, err := c.call(rpc.MethodChatDelete, map[string]any{
		"session_id": sessionID,
	})
	return err
}

// ChatHistoryGet fetches all messages for a session in created_at ASC order.
func (c *Client) ChatHistoryGet(sessionID string) ([]ChatMessageRow, error) {
	raw, err := c.call(rpc.MethodChatHistoryGet, map[string]any{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Messages []ChatMessageRow `json:"messages"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// ChatMessageRow mirrors store.ChatMessage's wire shape.
type ChatMessageRow struct {
	ID          string  `json:"id"`
	SessionID   string  `json:"session_id"`
	Role        string  `json:"role"`
	Content     string  `json:"content"`
	ToolCalls   string  `json:"tool_calls,omitempty"`
	ToolResults string  `json:"tool_results,omitempty"`
	CostUSD     float64 `json:"cost_usd"`
	CreatedAt   int64   `json:"created_at"`
}

// ChatHistoryList returns up to `limit` most recent chat sessions.
// limit <= 0 lets the daemon apply its default (50).
func (c *Client) ChatHistoryList(limit int) ([]ChatSessionRow, error) {
	raw, err := c.call(rpc.MethodChatHistoryList, map[string]any{"limit": limit})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Sessions []ChatSessionRow `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// ChatSessionRow mirrors store.ChatSession's wire shape (snake_case tags).
type ChatSessionRow struct {
	ID           string  `json:"id"`
	Surface      string  `json:"surface"`
	StartedAt    int64   `json:"started_at"`
	EndedAt      int64   `json:"ended_at"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Name         string  `json:"name,omitempty"`
	Provider     string  `json:"provider,omitempty"`
}

// StreamChat opens a one-shot streaming chat.send RPC against the daemon
// for one user turn. It launches a goroutine that reads line-delimited
// chat.Frame envelopes and dispatches each as a tabs.ChatFrameMsg to the bound
// Bubbletea program. On clean turn end (turn_done frame + daemon closes
// the conn) or any transport/RPC error, it emits tabs.ChatStreamEndedMsg with
// Err set accordingly. Callers should NOT call StreamChat again until the
// prior tabs.ChatStreamEndedMsg has been observed.
//
// The conn is held one-way by the daemon during the stream — chat.confirm
// (sent in response to a tool_proposed frame) MUST go on a separate conn,
// which the existing Client.call(...) already does by design.
func (c *Client) StreamChat(message, sessionID string) {
	if c.program == nil {
		return
	}
	go c.streamChatRaw(map[string]any{
		"message":    message,
		"session_id": sessionID,
	})
}

// StreamPlannerChat opens a chat.send stream with kind="plan" + project_slug.
// Mirrors StreamChat but seeds the planner-mode session creation; the
// daemon picks up kind="plan" and routes through the planner pipeline's
// system prompt. Used by the P keybind on the Projects tab — the TUI
// mirror of `hive plan <slug>` on the CLI.
//
// Sends "begin" as the seed user message — the planner system prompt's
// step 1 (call hive_list_specs, greet operator) fires on any user
// message. The daemon assigns + returns a fresh session ID; the chat
// tab captures it from the first "session" frame.
func (c *Client) StreamPlannerChat(projectSlug string) {
	if c.program == nil {
		return
	}
	go c.streamChatRaw(map[string]any{
		"message":      "begin",
		"session_id":   "",
		"kind":         "plan",
		"project_slug": projectSlug,
	})
}

// streamChatRaw is the shared dial+write+frame-parse loop behind
// StreamChat / StreamPlannerChat (and future planner-mode variants).
// Caller passes the chat.send params map directly; this function owns
// the conn, the goroutine lifetime, and the Started/Ended/Frame
// dispatch contract. Must be called from a goroutine — blocks until
// the daemon closes the conn or an error fires.
func (c *Client) streamChatRaw(params map[string]any) {
	if c.program == nil {
		return
	}
	conn, err := net.DialTimeout("unix", c.SockPath, 5*time.Second)
	if err != nil {
		c.program.Send(tabs.ChatStreamEndedMsg{Err: err})
		return
	}
	defer conn.Close()

	req := map[string]any{
		"id":     fmt.Sprintf("chat-%d", time.Now().UnixNano()),
		"method": rpc.MethodChatSend,
		"params": params,
	}
	body, merr := json.Marshal(req)
	if merr != nil {
		c.program.Send(tabs.ChatStreamEndedMsg{Err: merr})
		return
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		c.program.Send(tabs.ChatStreamEndedMsg{Err: err})
		return
	}
	c.program.Send(tabs.ChatStreamStartedMsg{})

	// Read line-delimited Response[chat.Frame] envelopes until EOF.
	rdr := bufio.NewReader(conn)
	for {
		line, rerr := rdr.ReadBytes('\n')
		if len(line) > 0 {
			var resp struct {
				Result *chat.Frame `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if jerr := json.Unmarshal(line, &resp); jerr == nil {
				// resp.Error covers hypothetical future RPC-level errors. The daemon's
				// chatWriteFrame currently routes all errors as in-result {kind:"error"}
				// chat.Frame values, so this branch is dead against today's daemon —
				// leave it as defense for any future protocol change.
				if resp.Error != nil {
					c.program.Send(tabs.ChatStreamEndedMsg{Err: errors.New(resp.Error.Message)})
					return
				}
				if resp.Result != nil {
					c.program.Send(tabs.ChatFrameMsg{Frame: *resp.Result})
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				c.program.Send(tabs.ChatStreamEndedMsg{Err: nil})
			} else {
				c.program.Send(tabs.ChatStreamEndedMsg{Err: rerr})
			}
			return
		}
	}
}
