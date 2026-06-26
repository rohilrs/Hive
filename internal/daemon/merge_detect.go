package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// detectMerges runs one merge-detection pass over ALL tasks in gate
// awaiting_merge, regardless of dispatch mode (Phase 2: previously sequenced-
// dispatcher-only). On a confirmed merge into the target branch it flips the
// gate to satisfied; the scheduler separately advances sequenced phases.
// Errors are per-task and non-fatal: one gh/auth/network failure is logged and
// the others proceed.
func (d *Daemon) detectMerges(ctx context.Context) {
	tasks, err := d.store.ListTasksByGateState(ctx, sequence.GateAwaitingMerge)
	if err != nil {
		log.Printf("merge-detect: list awaiting_merge: %v", err)
		return
	}
	projCache := map[string]*store.Project{}
	for _, task := range tasks {
		proj, ok := projCache[task.ProjectID]
		if !ok {
			p, perr := d.store.GetProject(ctx, task.ProjectID)
			if perr != nil {
				log.Printf("merge-detect: project %s: %v", task.ProjectID, perr)
				projCache[task.ProjectID] = nil
				continue
			}
			proj = p
			projCache[task.ProjectID] = p
		}
		if proj == nil {
			continue
		}
		d.checkOneMerge(ctx, task, proj)
	}
}

// checkOneMerge polls a single awaiting_merge task's PR and, if merged into the
// PR base branch, flips its gate to satisfied + emits sequence.gate_changed.
// The PR base is the feature branch when the integration loop is active, else
// the target branch — matching the base set by finish-branch when it opened the
// PR. Using effectiveWorktreeBaseForProject keeps detector and PR-opener in sync:
// for projects without a feature branch the resolver falls back to the target
// branch, so sequenced/non-feature projects behave identically.
func (d *Daemon) checkOneMerge(ctx context.Context, task *store.Task, proj *store.Project) {
	prURL, _, err := d.store.PRForTask(ctx, task.ID)
	if err != nil {
		log.Printf("merge-detect: PRForTask %s: %v", task.ID, err)
		return
	}
	if prURL == "" {
		log.Printf("merge-detect: task %s awaiting_merge but no PR recorded; skipping", task.ID)
		return
	}
	merged, baseRef, err := d.prGateway.State(ctx, prURL)
	if err != nil {
		log.Printf("merge-detect: state %s: %v", prURL, err)
		return
	}
	if !merged {
		// Not merged yet. If the project auto-merges and no merge is in flight for
		// this feature branch, dispatch one merge worker (the merge queue). The
		// guard makes this one-at-a-time per branch; the worker runs in a goroutine
		// (under the DAEMON ctx) so this reconcile pass doesn't block on a possibly
		// long resolve.
		if d.scheduler.taskAutoIntegrateForProject(proj.Slug) || d.autoMergePolicy(ctx, proj) {
			branch := d.scheduler.effectiveWorktreeBaseForProject(proj.Slug)
			key := mergeGuardKey(proj.Slug, branch)
			if branch != "" && d.mergeGuard.tryAcquire(key) {
				// Circuit breaker: count this dispatch; once we exceed the cap,
				// stop re-queuing this task forever and park it terminally at
				// merge_failed so a human can confirm + clear it.
				if d.mergeAttempts.bump(task.ID) > mergeAttemptCap {
					d.mergeGuard.release(key) // not dispatching — release the guard
					d.mergeAttempts.reset(task.ID)
					d.scheduler.parkMergeFailed(ctx, proj, task.ID,
						fmt.Sprintf("gave up after %d failed merge attempts", mergeAttemptCap))
					return
				}
				task := task // capture for the goroutine
				d.goTracked(func() { d.runQueuedMerge(d.ctx, task, proj, key) })
			}
		}
		return
	}
	base := d.scheduler.effectiveWorktreeBaseForProject(proj.Slug)
	// Fail closed: only satisfy on a confirmed merge into the PR base branch. An
	// empty baseRef (malformed/partial gh output) does NOT count as a match.
	if baseRef != base {
		log.Printf("merge-detect: %s merged into %q, want PR base %q; not satisfying %s", prURL, baseRef, base, task.ID)
		return
	}
	if err := d.store.UpdateTaskGateState(ctx, task.ID, sequence.GateSatisfied); err != nil {
		log.Printf("merge-detect: set satisfied %s: %v", task.ID, err)
		return
	}
	d.mergeAttempts.reset(task.ID)    // merge confirmed; clear the attempt counter
	d.refreshTaskStatus(ctx, task.ID) // status follows the merge to done
	d.scheduler.emitGateChanged(proj, task.ID, sequence.GateSatisfied)
	d.bus.Publish(rpc.EventMessage{Type: rpc.EventTaskMerged, Data: map[string]any{
		"task_id": task.ID, "pr_url": prURL,
	}})
	log.Printf("merge-detect: %s satisfied (PR %s merged into %s)", task.ID, prURL, base)

	// Clean up the merged PR's worktree + branch (we no longer pass
	// gh --delete-branch; see pr_gateway.Merge). Without this, merged task
	// branches accumulate locally AND on origin.
	d.cleanupMergedBranch(ctx, task, proj)

	// Fast-forward the LOCAL base to origin/<base> so the user's checkout doesn't
	// silently drift one commit behind per merged task. Best-effort + safe-guarded
	// (never FFs over divergence or non-roadmap dirty work).
	if proj.RepoPath != nil && *proj.RepoPath != "" {
		d.syncLocalBaseAfterMerge(*proj.RepoPath, base, proj.Slug)
	}
}

// cleanupMergedBranch removes the merged task's worktree + local branch and
// deletes the remote branch, so merged PR branches don't pile up (locally or on
// origin). All steps are best-effort: the merge is already confirmed, so a
// cleanup failure is logged, never fatal. The head branch is the build run's
// recorded branch_name (the resolve flow pushes to the SAME branch, so this is
// correct even for resolve-merged tasks).
func (d *Daemon) cleanupMergedBranch(ctx context.Context, task *store.Task, proj *store.Project) {
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return
	}
	repo := *proj.RepoPath
	run, err := d.store.LatestDoneBuildRunForTask(ctx, task.ID)
	if err != nil || run == nil || run.BranchName == "" {
		return // no recorded branch (e.g. non-build task) — nothing to clean up
	}
	branch := run.BranchName
	// ReclaimBranch removes whatever Hive worktree still holds the branch (the
	// build run OR a resolve worktree, found via `git worktree list`) then deletes
	// the local branch — exactly the step gh --delete-branch couldn't do while the
	// worktree held it.
	if rerr := d.wtMgr.ReclaimBranch(ctx, repo, branch); rerr != nil {
		log.Printf("merge cleanup: reclaim local branch %s: %v", branch, rerr)
	}
	// Delete the remote branch (gh no longer does it for us). Best-effort: it may
	// already be gone, or branch protection may forbid deletion.
	if out, derr := gitC(repo, "push", "origin", "--delete", branch); derr != nil {
		log.Printf("merge cleanup: delete remote branch %s: %v (%s)", branch, derr, strings.TrimSpace(out))
	}
}

// autoMergePolicy reports whether the project's sequenced advancement policy is
// auto_merge_on_green — i.e. the queue should drive the merge. It FAILS SAFE: a
// policy-read error returns false (do NOT dispatch a merge on uncertainty), so a
// transient store hiccup can never cause an unintended merge.
func (d *Daemon) autoMergePolicy(ctx context.Context, proj *store.Project) bool {
	policy, err := d.scheduler.advancementPolicy(ctx, proj.ID)
	if err != nil {
		return false
	}
	return policy == "auto_merge_on_green"
}
