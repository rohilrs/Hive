package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/rohilrs/Hive/internal/store"
)

// mergeGuard serializes PR merges per (project, feature branch): at most one
// merge (or merge+resolve) may be in flight for a given key at a time. This is
// the merge queue's serializer — realized as a one-in-flight-per-key flag rather
// than a held lock, so it never blocks a goroutine or holds a worker slot.
type mergeGuard struct {
	mu       sync.Mutex
	inFlight map[string]bool
}

func newMergeGuard() *mergeGuard {
	return &mergeGuard{inFlight: map[string]bool{}}
}

// mergeGuardKey scopes the guard per PROJECT + branch, so two unrelated projects
// that happen to share a branch name (e.g. both default to "main" with no
// feature branch) don't needlessly serialize against each other's merges.
func mergeGuardKey(slug, branch string) string {
	return slug + "\x00" + branch
}

// tryAcquire marks branch as having a merge in flight and returns true; returns
// false (no state change) if one is already in flight.
func (g *mergeGuard) tryAcquire(branch string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight[branch] {
		return false
	}
	g.inFlight[branch] = true
	return true
}

func (g *mergeGuard) release(branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inFlight, branch)
}

// mergeAttemptCap bounds how many times the merge queue will dispatch a merge
// worker for one task before giving up and parking it terminally at
// merge_failed. Without this, a task whose merge can never succeed (e.g. a
// duplicate PR whose branch was deleted post-merge) re-parks at awaiting_merge
// forever and detectMerges re-picks it every 30s — the circuit breaker.
const mergeAttemptCap = 3

// mergeAttemptTracker is an in-memory per-task merge-attempt counter. A
// PERSISTED counter is unnecessary: once a task reaches the merge_failed gate
// (which IS persisted), detectMerges never re-picks it (it only queries
// awaiting_merge), so the counter only needs to survive within one daemon run.
// Mirrors mergeGuard's mutex-guarded map.
type mergeAttemptTracker struct {
	mu sync.Mutex
	n  map[string]int
}

func newMergeAttemptTracker() *mergeAttemptTracker {
	return &mergeAttemptTracker{n: map[string]int{}}
}

// bump increments the task's attempt count and returns the new value.
func (t *mergeAttemptTracker) bump(taskID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n[taskID]++
	return t.n[taskID]
}

// reset clears the task's attempt count (called on success or terminal park so
// a future re-use of the task ID starts clean).
func (t *mergeAttemptTracker) reset(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.n, taskID)
}

// kickMergeQueue wakes the merge queue immediately instead of waiting for the
// 30s reconcile poll. It is a NON-BLOCKING send on the cap-1 kickMerge channel:
// if a kick is already pending (buffer full) the default branch drops this one,
// which is harmless — a single pending kick already coalesces into one
// detectMerges pass that drains every awaiting_merge task. Callers fire this
// best-effort whenever a task transitions to awaiting_merge.
func (d *Daemon) kickMergeQueue() {
	select {
	case d.kickMerge <- struct{}{}:
	default:
	}
}

// runQueuedMerge merges one awaiting_merge task's PR, classifying the outcome.
// It is invoked ONLY while holding the mergeGuard for `branch` (the caller
// acquires before spawning the worker; this worker defers the release). It
// relocates finishChainEnded's old inline merge block into the serialized queue:
//
//   - success / already-merged → leave at awaiting_merge; the universal merge
//     detector (checkOneMerge) confirms the merge → satisfied. This is the single
//     source of truth for "merged" and is idempotent.
//   - content conflict + resolveAuto → dispatch the MANUAL resolve run, which
//     provisions a fresh PR-branch worktree (the finish-branch worktree is long
//     torn down by the time the queue runs) and drives resolve →
//     reMergeAfterResolve → satisfied/park. Because dispatchResolveRunManual runs
//     SYNCHRONOUSLY (see the guard-release note below), the resolve + re-merge
//     complete BEFORE this worker returns, so the branch guard is held through the
//     whole resolve — no other merge for this branch can interleave with it. That
//     is what kills the resolve race.
//   - any other merge failure (branch protection, required checks, auth, …) →
//     park needs_attention (gate stays awaiting_merge so the task is re-resolvable).
//
// Guard-release timing (the crux): dispatchResolveRunManual is fully synchronous
// — it provisions the worktree, calls dispatchResolveRun (which runs the resolve
// pipeline and reMergeAfterResolve on the caller's goroutine and only returns
// after the merge attempt terminates), then tears the worktree down. It does NOT
// spawn a goroutine. Therefore `defer d.mergeGuard.release(branch)` at the top of
// this worker is correct: the worker goroutine blocks through the entire resolve
// + re-merge and releases the guard only once that has terminated. If the resolve
// path were ever made async, this defer would be WRONG and the release would have
// to be moved into the resolve run's terminal path.
// guardKey is the merge-guard key (mergeGuardKey(slug, branch)) the CALLER
// already acquired; this worker owns its release.
func (d *Daemon) runQueuedMerge(ctx context.Context, task *store.Task, proj *store.Project, guardKey string) {
	defer d.mergeGuard.release(guardKey)

	prURL, _, err := d.store.PRForTask(ctx, task.ID)
	if err != nil || prURL == "" {
		// No PR to merge; checkOneMerge already logged/handled the missing-PR case.
		return
	}
	merr := d.prGateway.Merge(ctx, prURL, d.scheduler.mergeMethodForProject(proj.Slug))
	if merr == nil || isAlreadyMergedErr(merr) {
		// Merged (or already merged by a human / external queue / duplicate
		// attempt) — success-equivalent. Leave at awaiting_merge; checkOneMerge
		// confirms the merge → satisfied. Must NOT trigger the resolver.
		return
	}
	if isMergeConflictErr(merr) && d.scheduler.resolveAutoForProject(proj.Slug) {
		// Reproduce + resolve on the PR branch. dispatchResolveRunManual provisions
		// a fresh worktree checked out on the PR branch (the finish-branch worktree
		// is gone) and runs resolve → reMergeAfterResolve → satisfied/park, all
		// SYNCHRONOUSLY on this goroutine — so it runs INSIDE the held guard and the
		// resolve + re-merge are serialized for this branch.
		if derr := d.scheduler.dispatchResolveRunManual(ctx, task, proj); derr != nil {
			// The resolve couldn't even be DISPATCHED (e.g. the worktree can't be
			// provisioned because the PR branch was deleted post-merge —
			// `git worktree add … exit status 128`). Retrying is futile, so park
			// TERMINALLY instead of re-parking awaiting_merge (which would loop every
			// 30s). Deliberate safety choice: do NOT auto-mark satisfied even though
			// branch-gone usually means the PR merged — auto-satisfying risks silently
			// skipping genuinely-unmerged work. merge_failed surfaces it to a human.
			d.mergeAttempts.reset(task.ID)
			d.scheduler.parkMergeFailed(ctx, proj, task.ID,
				fmt.Sprintf("resolve could not be provisioned (branch likely deleted post-merge): %v", derr))
		}
		return
	}
	// Non-conflict merge failure (branch protection, required checks, etc.) → park.
	log.Printf("merge-queue: merge %s (%s): %v; parking needs_attention", task.ID, prURL, merr)
	d.scheduler.parkResolveNeedsAttention(ctx, proj, task.ID)
}
