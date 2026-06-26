package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// handleSequencedBuildEnd hooks executePipeline's tail. It takes ownership of
// the task-status update (returns true) ONLY for a successful `build` run on a
// sequenced-project task that is part of the sequence — advancing the gate to
// `built` and chaining a finish-branch run on the same worktree+branch. In
// every other case it returns false and the caller applies the normal
// done/needs_attention task-status update (a failed build leaves gate=none,
// which correctly blocks the phase).
func (s *Scheduler) handleSequencedBuildEnd(ctx context.Context, run *store.Run, task *store.Task, proj *store.Project, result *pipeline.Result, worktreePath, branchName string, transferred *bool) bool {
	if run.Pipeline != "build" || result.Status != "done" {
		return false
	}
	if s.effectiveDispatchModeForProject(proj.Slug) != "sequenced" {
		return false
	}
	if _, ok := task.Metadata["roadmap_phase"].(string); !ok {
		return false // unsequenced task in a sequenced project — normal handling
	}
	if err := s.d.store.UpdateTaskGateState(ctx, task.ID, sequence.GateBuilt); err != nil {
		log.Printf("sequence: set gate built for %s: %v; falling back to normal handling", task.ID, err)
		return false
	}
	_ = s.d.store.UpdateTaskStatus(ctx, task.ID, "running")
	s.emitGateChanged(proj, task.ID, sequence.GateBuilt)
	*transferred = true
	s.chainFinishBranch(run, task, proj, worktreePath, branchName)
	return true
}

// chainFinishBranch launches a finish-branch run that REUSES the build run's
// worktree + branch (so `git push HEAD` carries the build commits). Mirrors
// childRunner.RunChildFix: own run row (parent_run_id = build run), direct
// pipeline.Run (no scavenger prep — finish-branch is pure shell gates), fresh
// runtime dir, fire-and-forget via goTracked. Bypasses the scheduler capacity
// gate (same as child fix runs). On finish-branch `done` the gate becomes
// `satisfied` (pr_opened); otherwise the gate stays `built` and the task goes
// needs_attention (blocks the phase).
func (s *Scheduler) chainFinishBranch(buildRun *store.Run, task *store.Task, proj *store.Project, worktreePath, branchName string) {
	p := s.d.pipelines["finish-branch"]
	if p == nil {
		log.Printf("sequence: finish-branch pipeline not registered; cannot chain for %s", task.ID)
		_ = s.d.store.UpdateTaskStatus(s.d.ctx, task.ID, "needs_attention")
		return
	}
	s.d.goTracked(func() {
		defer s.teardownScavengerWorkspace(worktreePath)
		ctx, cancel := context.WithCancel(s.d.ctx)
		finishID := newID("run")
		s.d.registerRunCancel(finishID, cancel)
		defer s.d.unregisterRunCancel(finishID)
		defer cancel()

		finishRow := &store.Run{
			ID: finishID, TaskID: task.ID, ProjectID: proj.ID,
			Pipeline: "finish-branch", Status: "running", ParentRunID: buildRun.ID,
		}
		if err := s.d.store.InsertRun(ctx, finishRow); err != nil {
			log.Printf("sequence: insert finish-branch run for %s: %v", task.ID, err)
			_ = s.d.store.UpdateTaskStatus(ctx, task.ID, "needs_attention")
			return
		}
		if err := s.d.store.MarkRunStarted(ctx, finishID); err != nil {
			log.Printf("sequence: mark finish-branch started for %s: %v", task.ID, err)
			s.finishChainEnded(ctx, finishID, task, proj, "needs_attention", "mark started: "+err.Error(), "", 0, "", "")
			return
		}
		_ = s.d.store.SetRunBranch(ctx, finishID, branchName)
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunStarted, Data: map[string]any{
			"run_id": finishID, "task_id": task.ID, "task_title": task.Title,
			"project_id": proj.ID, "pipeline": "finish-branch", "parent_run_id": buildRun.ID,
		}})
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskIntegrating, Data: map[string]any{
			"task_id": task.ID, "project_id": proj.ID,
		}})

		runtimeDir := filepath.Join(s.d.HiveDir(), finishID)
		if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
			s.finishChainEnded(ctx, finishID, task, proj, "needs_attention", "mkdir finish runtime: "+err.Error(), "", 0, "", "")
			return
		}

		pr := &pipeline.Run{
			ID: finishID, Task: task, Project: proj,
			WorktreePath: worktreePath, RuntimeDir: runtimeDir, BranchName: branchName,
			Pipeline: "finish-branch", TargetBranch: s.effectiveWorktreeBaseForProject(proj.Slug),
			Commands: s.d.runCommandsForProject(proj.Slug),
		}
		result, err := p.Run(ctx, pr)
		status, summary, prURL, prNum := "needs_attention", "", "", 0
		switch {
		case err != nil:
			summary = "finish-branch error: " + err.Error()
		case result != nil:
			status, summary, prURL, prNum = result.Status, result.Summary, result.PRURL, result.PRNumber
		}
		s.finishChainEnded(ctx, finishID, task, proj, status, summary, prURL, prNum, worktreePath, branchName)
	})
}

// resolveMergeOutcome maps the post-resolve merge attempt to the run's terminal
// (status, summary). merged is the confirmed PR merge state (from
// reMergeAfterResolve succeeding AND prGateway.State confirming); base is the
// branch the PR targets. Centralizing the strings here keeps the stored summary
// honest and unit-testable: a resolve that ends with the PR still conflicting
// must NOT carry a summary that says it merged.
func resolveMergeOutcome(merged bool, base string) (status, summary string) {
	if merged {
		return "done", "merged into " + base
	}
	return "needs_attention", "resolved + pushed, but PR still CONFLICTING against " + base + " — parked"
}

// dispatchResolveRun runs the resolve pipeline on the task's EXISTING PR branch
// + worktree (the live finish-branch worktree, which carries the build commits).
// It mirrors chainFinishBranch's run-row bookkeeping (own row, parent = the
// finish-branch run is not threaded here; resolve is its own terminal run),
// but runs SYNCHRONOUSLY on the caller's goroutine so the finish-branch
// worktree-teardown defer fires only after resolve completes.
//
// The resolve pipeline reproduces the merge against the project's target branch
// in the worktree, drives a bounded resolve→test→validate loop, and pushes the
// resolved merge on green. On a `done` result we re-attempt the auto-merge (the
// conflict is now resolved) and satisfy the gate on success; otherwise the task
// is surfaced for attention. Reuses runCommandsForProject so per-project
// test/validate overrides apply; does NOT provision a new worktree.
func (s *Scheduler) dispatchResolveRun(ctx context.Context, task *store.Task, proj *store.Project, worktreePath, branchName string) error {
	p := s.d.pipelines["resolve"]
	if p == nil {
		return errors.New("resolve pipeline not registered")
	}

	resolveID := newID("run")
	rctx, cancel := context.WithCancel(ctx)
	s.d.registerRunCancel(resolveID, cancel)
	defer s.d.unregisterRunCancel(resolveID)
	defer cancel()

	row := &store.Run{
		ID: resolveID, TaskID: task.ID, ProjectID: proj.ID,
		Pipeline: "resolve", Status: "running",
	}
	if err := s.d.store.InsertRun(rctx, row); err != nil {
		return err
	}
	if err := s.d.store.MarkRunStarted(rctx, resolveID); err != nil {
		_ = s.d.store.MarkRunEnded(rctx, resolveID, "needs_attention", "mark started: "+err.Error())
		return err
	}
	_ = s.d.store.SetRunBranch(rctx, resolveID, branchName)
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunStarted, Data: map[string]any{
		"run_id": resolveID, "task_id": task.ID, "task_title": task.Title,
		"project_id": proj.ID, "pipeline": "resolve",
	}})

	runtimeDir := filepath.Join(s.d.HiveDir(), resolveID)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		_ = s.d.store.MarkRunEnded(rctx, resolveID, "needs_attention", "mkdir resolve runtime: "+err.Error())
		return err
	}

	pr := &pipeline.Run{
		ID: resolveID, Task: task, Project: proj,
		WorktreePath: worktreePath, RuntimeDir: runtimeDir, BranchName: branchName,
		Pipeline: "resolve", TargetBranch: s.effectiveWorktreeBaseForProject(proj.Slug),
		Commands: s.d.runCommandsForProject(proj.Slug),
	}
	result, rerr := p.Run(rctx, pr)
	status, summary := "needs_attention", ""
	switch {
	case rerr != nil:
		summary = "resolve error: " + rerr.Error()
	case result != nil:
		status, summary = result.Status, result.Summary
	}
	_ = s.d.store.MarkRunEnded(rctx, resolveID, status, summary)
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunEnded, Data: map[string]any{
		"run_id": resolveID, "task_id": task.ID, "status": status, "summary": summary,
	}})

	if status != "done" {
		// Resolve exhausted/failed — the branch is no closer to merging; surface
		// it. EXPLICIT needs_attention (the resolve run is terminal "done" only on
		// success; refreshTaskStatus could mis-derive).
		s.parkResolveNeedsAttention(rctx, proj, task.ID)
		return nil
	}

	// Resolve pushed the resolved merge to the PR branch. Re-attempt the
	// auto-merge now that the conflict is gone.
	prURL, _, perr := s.d.store.PRForTask(rctx, task.ID)
	if perr != nil || prURL == "" {
		// No PR URL recoverable — can't drive the merge ourselves. Surface for
		// manual attention (the merge detector still confirms if it lands by
		// another path).
		s.parkResolveNeedsAttention(rctx, proj, task.ID)
		return nil
	}
	base := s.effectiveWorktreeBaseForProject(proj.Slug)
	if merr := s.reMergeAfterResolve(rctx, prURL, s.mergeMethodForProject(proj.Slug)); merr != nil {
		log.Printf("sequence: re-merge after resolve %s (%s): %v; parking needs_attention", task.ID, prURL, merr)
		// The pipeline's neutral "awaiting merge confirmation" summary was stored
		// BEFORE this server-side merge check. The PR is still CONFLICTING, so
		// re-mark the run with the honest terminal outcome before parking — the
		// stored summary must not imply the merge succeeded.
		fStatus, fSummary := resolveMergeOutcome(false, base)
		_ = s.d.store.MarkRunEnded(rctx, resolveID, fStatus, fSummary)
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunEnded, Data: map[string]any{
			"run_id": resolveID, "task_id": task.ID, "status": fStatus, "summary": fSummary,
		}})
		s.parkResolveNeedsAttention(rctx, proj, task.ID)
		return nil
	}
	// reMergeAfterResolve returned nil = a CONFIRMED merge — it polled mergeability
	// then called gh pr merge successfully. That success IS the merge confirmation,
	// so merged := (merr == nil) is the honest terminal signal. We deliberately do
	// NOT gate the outcome on a follow-up prGateway.State read: GitHub computes PR
	// state asynchronously, so a just-merged PR can still read "not merged" for a
	// moment — re-checking would reintroduce the exact async-lag false-negative the
	// resolver tolerates. (If State happens to report a more specific base branch,
	// we use it for a tighter summary, but never to overturn the merge.)
	if _, b, serr := s.d.prGateway.State(rctx, prURL); serr == nil && b != "" {
		base = b
	}
	mStatus, mSummary := resolveMergeOutcome(true, base)
	_ = s.d.store.MarkRunEnded(rctx, resolveID, mStatus, mSummary)
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunEnded, Data: map[string]any{
		"run_id": resolveID, "task_id": task.ID, "status": mStatus, "summary": mSummary,
	}})
	if cur, gerr := s.d.store.GetTask(rctx, task.ID); gerr == nil && cur.GateState == sequence.GateMergeFailed {
		// Defense-in-depth: a merge_failed task must not be re-armed by a resolve
		// outcome. (Normally unreachable — manual resolve is refused and the auto
		// detector never picks merge_failed — but keep state consistent.)
		log.Printf("dispatchResolveRun: task %s is merge_failed (terminal); leaving gate untouched", task.ID)
		return nil
	}
	if uerr := s.d.store.UpdateTaskGateState(rctx, task.ID, sequence.GateAwaitingMerge); uerr != nil {
		log.Printf("sequence: set awaiting_merge after resolve for %s: %v", task.ID, uerr)
	}
	// The resolve fixed the conflict and pushed — the branch content changed, so
	// prior failed attempts no longer apply. Reset the circuit-breaker counter so
	// the freshly-resolved merge gets a clean cap budget.
	s.d.mergeAttempts.reset(task.ID)
	_ = s.d.store.UpdateTaskStatus(rctx, task.ID, "done")
	s.emitGateChanged(proj, task.ID, sequence.GateAwaitingMerge)
	return nil
}

// parkResolveNeedsAttention marks the task needs_attention with the gate at
// awaiting_merge after a resolve run that could not be carried all the way to a
// completed merge (exhausted, no recoverable PR URL, or the re-merge failed).
// Setting needs_attention keeps the task VISIBLE and re-resolvable — the manual
// `hive resolve` / resolve.now guard requires needs_attention, so without this
// the task would keep its prior status (e.g. "running") and silently refuse a
// manual retry.
func (s *Scheduler) parkResolveNeedsAttention(ctx context.Context, proj *store.Project, taskID string) {
	// Never downgrade a TERMINAL merge_failed gate back to awaiting_merge — that
	// re-arms the auto-merge loop the circuit breaker exists to stop. (A
	// merge_failed task is recovered only via `hive merge retry`.)
	if cur, err := s.d.store.GetTask(ctx, taskID); err == nil && cur.GateState == sequence.GateMergeFailed {
		_ = s.d.store.UpdateTaskStatus(ctx, taskID, "needs_attention")
		return
	}
	_ = s.d.store.UpdateTaskStatus(ctx, taskID, "needs_attention")
	// Persist the gate too (emitGateChanged only publishes the event), so the
	// stored state is self-consistent — status=needs_attention + gate=awaiting_merge.
	_ = s.d.store.UpdateTaskGateState(ctx, taskID, sequence.GateAwaitingMerge)
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskUpdated, Data: map[string]any{
		"task_id": taskID, "status": "needs_attention",
	}})
	s.emitGateChanged(proj, taskID, sequence.GateAwaitingMerge)
}

// parkMergeFailed parks a task TERMINALLY at the merge_failed gate (the circuit
// breaker's final state). Unlike parkResolveNeedsAttention — which re-parks at
// awaiting_merge and so is re-picked by detectMerges every 30s — merge_failed is
// NOT queried by detectMerges, so the task is never re-attempted. It stays
// status=needs_attention so it remains a visible phase blocker for a human to
// confirm + mark done. Used when the merge-attempt cap is exceeded or a resolve
// can't be provisioned (PR branch deleted post-merge).
func (s *Scheduler) parkMergeFailed(ctx context.Context, proj *store.Project, taskID, reason string) {
	_ = s.d.store.UpdateTaskStatus(ctx, taskID, "needs_attention")
	_ = s.d.store.UpdateTaskGateState(ctx, taskID, sequence.GateMergeFailed)
	log.Printf("merge-queue: task %s → merge_failed (terminal): %s", taskID, reason)
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskUpdated, Data: map[string]any{
		"task_id": taskID, "status": "needs_attention",
	}})
	s.emitGateChanged(proj, taskID, sequence.GateMergeFailed)
}

// reMergeAfterResolve merges the PR once GitHub has recomputed mergeability
// following the resolve push. GitHub computes `mergeable` asynchronously, so
// the previous code's immediate Merge raced a stale "UNKNOWN"/"CONFLICTING"
// verdict and spuriously failed with "the merge commit cannot be cleanly
// created" even though the resolution was correct — then parked, and nothing
// retried. We poll mergeability every reMergeInterval up to reMergeTimeout:
// once it reads "MERGEABLE" we attempt the merge (retrying a transient merge
// failure within the remaining budget). "UNKNOWN" (still computing) and a
// just-pushed-stale "CONFLICTING" both mean keep waiting — a genuinely
// unmergeable PR simply exhausts the budget and parks at awaiting_merge, the
// same safe terminal as before. Returns nil only on a confirmed merge.
func (s *Scheduler) reMergeAfterResolve(ctx context.Context, prURL, method string) error {
	wctx, cancel := context.WithTimeout(ctx, s.reMergeTimeout)
	defer cancel()
	lastErr := errors.New("PR mergeability did not settle before timeout")
	for {
		state, err := s.d.prGateway.Mergeable(wctx, prURL)
		switch {
		case err != nil:
			// Transient gateway hiccup — retry within budget. Don't let a
			// deadline/cancellation error overwrite the informative lastErr we'd
			// otherwise report (CONFLICTING / last transient merge failure).
			if wctx.Err() == nil {
				lastErr = err
			}
		case state == "MERGEABLE":
			// Best-effort merge. If wctx is near its deadline the merge may be
			// cancelled mid-flight after GitHub already started it (ambiguous
			// "did it land?"), but the merge-detect poller reconciles an
			// already-merged PR on its next State() tick, so this self-heals to
			// the same safe park rather than a torn state.
			merr := s.d.prGateway.Merge(wctx, prURL, method)
			if merr == nil {
				return nil
			}
			if wctx.Err() == nil {
				lastErr = merr // checks/locks still settling — retry within budget
			}
		case state == "CONFLICTING":
			// Likely the stale pre-recompute verdict right after the push; keep
			// polling. If it never clears, we exhaust the budget below.
			lastErr = fmt.Errorf("PR reports CONFLICTING after resolve: %s", prURL)
		default:
			// "UNKNOWN" or empty: GitHub is still recomputing — keep waiting.
		}
		select {
		case <-wctx.Done():
			return lastErr
		case <-time.After(s.reMergeInterval):
		}
	}
}

// finishChainEnded marks the finish-branch run ended, publishes run.ended,
// and applies the gate transition: done -> satisfied (task done); anything
// else -> gate stays built, task needs_attention (phase blocked).
// worktreePath + branchName carry the finish-branch run's live worktree and
// branch so an auto-merge content conflict can hand them to dispatchResolveRun
// (the resolve pipeline reproduces the merge in that worktree). They are empty
// for the early-failure call sites (no worktree yet), which never reach the
// merge path.
func (s *Scheduler) finishChainEnded(ctx context.Context, finishID string, task *store.Task, proj *store.Project, status, summary, prURL string, prNum int, worktreePath, branchName string) {
	if prURL != "" {
		_ = s.d.store.SetRunPR(ctx, finishID, prURL, prNum)
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventPROpened, Data: map[string]any{
			"task_id": task.ID, "pr_url": prURL, "pr_number": prNum,
		}})
	}
	_ = s.d.store.MarkRunEnded(ctx, finishID, status, summary)
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventRunEnded, Data: map[string]any{
		"run_id": finishID, "task_id": task.ID, "status": status, "summary": summary,
	}})
	if status == "done" {
		policy, perr := s.advancementPolicy(ctx, proj.ID)
		if perr != nil {
			// We could not read the policy (a transient store error — NOT an
			// absent dispatcher, which advancementPolicy maps to pr_opened).
			// Do NOT satisfy: under a merge-gated policy that would advance the
			// phase without the PR ever merging. Freeze the gate at built and
			// surface for attention/retry instead.
			// Keep this an EXPLICIT needs_attention write — do NOT replace with
			// refreshTaskStatus: the build run is already "done" here, so the
			// derive would compute "done" and mask this failure.
			log.Printf("sequence: read advancement policy for %s: %v; leaving needs_attention (gate=built)", task.ID, perr)
			_ = s.d.store.UpdateTaskStatus(ctx, task.ID, "needs_attention")
			s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskUpdated, Data: map[string]any{
				"task_id": task.ID, "status": "needs_attention",
			}})
			s.emitGateChanged(proj, task.ID, sequence.GateBuilt)
			return
		}
		autoIntegrate := s.taskAutoIntegrateForProject(proj.Slug)
		gate := sequence.GateSatisfied
		if policy != "pr_opened" || autoIntegrate {
			gate = sequence.GateAwaitingMerge
		}
		// NO inline merge here. For auto_merge_on_green / task-auto-integrate the
		// gate lands at awaiting_merge and the MERGE QUEUE
		// (detectMerges → checkOneMerge → runQueuedMerge) is the SOLE merger: it
		// serializes merges per feature branch (one merge/resolve in flight at a
		// time), eliminating the simultaneous-merge and resolve races. The task
		// releases its worker slot at awaiting_merge; the reconcile pass drains it.
		if err := s.d.store.UpdateTaskGateState(ctx, task.ID, gate); err != nil {
			log.Printf("sequence: set gate %s for %s: %v", gate, task.ID, err)
		}
		if gate == sequence.GateAwaitingMerge {
			// FRESH entry into awaiting_merge after a successful build/finish — reset
			// the circuit-breaker counter so a re-queued task starts clean.
			s.d.mergeAttempts.reset(task.ID)
		}
		_ = s.d.store.UpdateTaskStatus(ctx, task.ID, "done")
		s.emitGateChanged(proj, task.ID, gate)
		// Low-latency: when the task parks at awaiting_merge, kick the merge queue
		// so the merge starts well before the 30s reconcile poll. Best-effort
		// (non-blocking send + cap-1 buffer); the poll is the backstop.
		if gate == sequence.GateAwaitingMerge {
			s.d.kickMergeQueue()
		}
		return
	}
	// Run is already marked terminal (MarkRunEnded above). DeriveTaskStatus sees
	// gate=built + latest non-abandoned run = needs_attention|error → "needs_attention".
	s.d.refreshTaskStatus(ctx, task.ID)
	s.emitGateChanged(proj, task.ID, sequence.GateBuilt)
}

// advancementPolicy returns the project's configured advancement policy.
// An ABSENT dispatcher row (ErrNotFound) maps to "pr_opened" — a defensible
// default for a project that somehow lost its row, and the historical behavior.
// A transient read error is returned as an error so the caller does NOT satisfy
// the gate (which would prematurely advance a merge-gated phase).
func (s *Scheduler) advancementPolicy(ctx context.Context, projectID string) (string, error) {
	disp, err := s.d.store.GetSequenceDispatcher(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "pr_opened", nil
		}
		return "", err
	}
	if disp == nil || disp.AdvancementPolicy == "" {
		return "pr_opened", nil
	}
	return disp.AdvancementPolicy, nil
}

// mergeMethodForProject is the gh merge strategy for a project's task→feature
// (or sequenced) merges, from its [integration] config. Defaults to "merge".
func (s *Scheduler) mergeMethodForProject(slug string) string {
	return s.d.effectiveConfigForProject(slug).Integration.ResolvedMergeMethod()
}

func (s *Scheduler) emitGateChanged(proj *store.Project, taskID, gate string) {
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventSequenceGateChanged, Data: map[string]any{
		"project_id": proj.ID, "project_slug": proj.Slug, "task_id": taskID, "gate_state": gate,
	}})
}

// dispatchResolveRunManual is the MANUAL conflict-resolver entry point (the
// `hive resolve <task>` / resolve.now path), for a STUCK task whose live
// worktree is GONE. Where the auto path (finishChainEnded → dispatchResolveFn)
// reuses the live finish-branch worktree that still carries the PR-branch
// commits, here we must PROVISION a fresh worktree CHECKED OUT ON THE PR
// BRANCH (not a new branch forked off the target — the resolve pipeline must
// reproduce `git merge <target>` against the PR branch's actual commits to
// recreate the conflict).
//
// wtMgr.Create cannot be used: it always does `git worktree add -b <branch>
// <forkPoint>` where forkPoint is the TARGET/base branch, i.e. it creates a
// FRESH branch with none of the PR-branch commits. So we provision directly:
// fetch the PR branch + target from origin, then `git worktree add <dir>
// <prBranch>` against the project repo. After resolve completes we tear the
// worktree down (the branch itself is left intact — the resolve pipeline has
// already pushed the resolved merge to it).
//
// Once the worktree is in place this REUSES dispatchResolveRun verbatim, so
// the run-accounting, re-merge-on-done, and needs_attention handling are
// identical to the auto path.
func (s *Scheduler) dispatchResolveRunManual(ctx context.Context, task *store.Task, proj *store.Project) error {
	if s.d.pipelines["resolve"] == nil {
		return errors.New("resolve pipeline not registered")
	}
	if proj.RepoPath == nil || strings.TrimSpace(*proj.RepoPath) == "" {
		return fmt.Errorf("project %s has no repo_path; cannot provision worktree", proj.Slug)
	}
	repoPath := *proj.RepoPath

	branchName, err := s.resolvePRBranchForTask(ctx, task)
	if err != nil {
		return err
	}
	targetBranch := s.effectiveWorktreeBaseForProject(proj.Slug)

	// Provision a worktree directory under the same root the manager uses, with
	// a resolve-scoped run id so it never collides with a live run worktree.
	provisionID := newID("resolve-wt")
	wtPath := filepath.Join(s.d.HiveDir(), "worktrees", provisionID)

	if err := s.provisionResolveWorktree(ctx, repoPath, wtPath, branchName, targetBranch); err != nil {
		return err
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, rerr := exec.CommandContext(tctx, "git", "-C", repoPath,
			"worktree", "remove", "--force", wtPath).CombinedOutput(); rerr != nil {
			log.Printf("resolve: teardown worktree %s: %v\n%s", wtPath, rerr, out)
		}
	}()

	return s.dispatchResolveRun(ctx, task, proj, wtPath, branchName)
}

// resolvePRBranchForTask determines the branch the task's PR lives on. Prefers
// the explicit metadata branch_name (Linear's canonical branch / operator
// override), then falls back to the most recent run row that recorded a branch
// (the finish-branch run that opened the PR). Errors when neither is available
// — without the PR branch there is nothing to reproduce the conflict against.
func (s *Scheduler) resolvePRBranchForTask(ctx context.Context, task *store.Task) (string, error) {
	if b := branchNameFromTaskMetadata(task); b != "" {
		return b, nil
	}
	runs, err := s.d.store.ListRunsByTask(ctx, task.ID)
	if err != nil {
		return "", fmt.Errorf("look up runs for task %s: %w", task.ID, err)
	}
	for _, r := range runs {
		if strings.TrimSpace(r.BranchName) != "" {
			return r.BranchName, nil
		}
	}
	return "", fmt.Errorf("no PR branch found for task %s (no metadata branch_name and no run recorded a branch)", task.ID)
}

// provisionResolveWorktree fetches the PR branch + target from origin and adds
// a worktree checked out on the PR branch's tip. It prefers origin/<prBranch>
// (the pushed PR head) so the worktree carries the PR's actual commits even if
// no local ref exists; falls back to a local <prBranch> ref when origin is
// unavailable. The target branch is fetched too so the resolve pipeline's
// `git merge <target>` resolves against an up-to-date base.
func (s *Scheduler) provisionResolveWorktree(ctx context.Context, repoPath, wtPath, prBranch, targetBranch string) error {
	// Best-effort fetch of both refs; offline / no-remote falls through to local refs.
	if out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
		"fetch", "--quiet", "origin", prBranch, targetBranch).CombinedOutput(); err != nil {
		log.Printf("resolve: fetch origin %s %s: %v\n%s (continuing with local refs)",
			prBranch, targetBranch, err, out)
	}

	// Prefer the freshly-fetched origin head of the PR branch; fall back to a
	// local ref of the same name. git worktree add on a remote-tracking ref
	// creates a local branch tracking it, which is exactly what we want.
	startPoint := prBranch
	if refExistsGit(ctx, repoPath, "refs/remotes/origin/"+prBranch) {
		startPoint = "origin/" + prBranch
	}

	args := []string{"-C", repoPath, "worktree", "add"}
	if startPoint == "origin/"+prBranch {
		// Create a local branch named after the PR branch tracking origin/<prBranch>.
		args = append(args, "-B", prBranch, wtPath, startPoint)
	} else {
		args = append(args, wtPath, startPoint)
	}
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		// Self-heal the worktree-leak: a leaked worktree from a TERMINAL run can
		// still hold prBranch ("branch is already used by worktree at <path>"),
		// blocking the add. If that worktree is under Hive's worktrees root and
		// its run is not active, reap it and retry once. This is the manual
		// `git worktree remove` that previously had to be done by hand.
		if cp := worktreeInUsePath(string(out)); cp != "" && s.reapStaleWorktree(ctx, repoPath, cp) {
			if out2, err2 := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err2 != nil {
				return fmt.Errorf("git worktree add %s on %s (after reaping %s): %w\n%s", wtPath, startPoint, cp, err2, out2)
			}
			return nil
		}
		return fmt.Errorf("git worktree add %s on %s: %w\n%s", wtPath, startPoint, err, out)
	}
	return nil
}

// worktreeInUseRe extracts the conflicting worktree path from git's
// "fatal: '<branch>' is already used by worktree at '<path>'" error.
var worktreeInUseRe = regexp.MustCompile(`already used by worktree at '([^']+)'`)

func worktreeInUsePath(gitErr string) string {
	if m := worktreeInUseRe.FindStringSubmatch(gitErr); len(m) == 2 {
		return m[1]
	}
	return ""
}

// reapStaleWorktree removes a conflicting worktree IFF it is under Hive's
// worktrees root AND its run is not active (terminal, or no run row at all).
// Returns true only when the worktree was actually removed. Conservative by
// design: it refuses any path outside the worktrees root and any worktree
// whose run is still running/dispatched/queued, so it can never yank an
// in-flight run's checkout.
func (s *Scheduler) reapStaleWorktree(ctx context.Context, repoPath, wtPath string) bool {
	root := filepath.Clean(filepath.Join(s.d.HiveDir(), "worktrees"))
	cleaned := filepath.Clean(wtPath)
	// Accept ONLY a path strictly under the worktrees root — this rejects both
	// paths outside the root AND the root directory itself (root lacks the
	// "root/" prefix), and the sibling "<root>-evil" (no "root/" prefix either).
	if !strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
		log.Printf("resolve: refusing to reap worktree path not strictly under %s: %s", root, wtPath)
		return false
	}
	// The worktree dir basename is the owning run's id (e.g. run-<id>). Reap
	// ONLY when the run is in a known TERMINAL state — fail safe: an unknown /
	// in-flight status (pending/running/dispatched/queued, or any future
	// status) REFUSES the reap, so we can never yank a live run's checkout. A
	// missing run row (orphan worktree) is reapable.
	runID := filepath.Base(cleaned)
	if run, err := s.d.store.GetRun(ctx, runID); err == nil && run != nil && !isRunTerminal(run.Status) {
		log.Printf("resolve: refusing to reap worktree of non-terminal run %s (status=%s)", runID, run.Status)
		return false
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", wtPath).CombinedOutput(); err != nil {
		log.Printf("resolve: reap stale worktree %s: %v\n%s", wtPath, err, out)
		return false
	}
	_, _ = exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "prune").CombinedOutput()
	log.Printf("resolve: reaped stale worktree %s (was blocking provision)", wtPath)
	return true
}

// isRunTerminal reports whether a run status is a known TERMINAL state. Used as
// a fail-safe allowlist for worktree reaping: only terminal runs' worktrees may
// be reaped; "pending" (the InsertRun default, held while the worktree exists
// but before MarkRunStarted), "running", and any unknown/future status are NOT
// terminal and so refuse the reap.
func isRunTerminal(status string) bool {
	switch status {
	case "done", "needs_attention", "error", "abandoned":
		return true
	default:
		return false
	}
}

// refExistsGit reports whether a git ref resolves in the repo.
func refExistsGit(ctx context.Context, repoPath, ref string) bool {
	return exec.CommandContext(ctx, "git", "-C", repoPath,
		"rev-parse", "--verify", "--quiet", ref).Run() == nil
}
