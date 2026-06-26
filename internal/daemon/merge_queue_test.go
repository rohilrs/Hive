package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

func TestMergeInFlightGuard(t *testing.T) {
	g := newMergeGuard()
	if !g.tryAcquire("spec/feat") {
		t.Fatal("first acquire should succeed")
	}
	if g.tryAcquire("spec/feat") {
		t.Error("second acquire of the same branch must fail (in flight)")
	}
	if !g.tryAcquire("other/branch") {
		t.Error("a different branch must acquire independently")
	}
	g.release("spec/feat")
	if !g.tryAcquire("spec/feat") {
		t.Error("after release, the branch must acquire again")
	}
}

// TestKickMergeQueueNonBlocking pins the low-latency kick's two invariants:
// (1) it lands a wake-up in the cap-1 buffer that reconcileLoop can drain, and
// (2) it never blocks or panics even when the buffer is already full (a second
// back-to-back kick is silently dropped via the select-default). A dropped kick
// is benign — one pending kick already coalesces into one detectMerges pass.
func TestKickMergeQueueNonBlocking(t *testing.T) {
	d := newTestDaemon(t)

	// First kick fills the cap-1 buffer.
	d.kickMergeQueue()
	// Second kick MUST NOT block (buffer full → default branch drops it). If the
	// send were blocking this call would deadlock and the test would time out.
	d.kickMergeQueue()

	// Exactly one wake-up is buffered (the two kicks coalesced).
	select {
	case <-d.kickMerge:
	default:
		t.Fatal("expected a buffered kick to drain")
	}
	select {
	case <-d.kickMerge:
		t.Fatal("expected only ONE buffered kick (second should have been dropped)")
	default:
	}

	// After draining, the channel is reusable: a fresh kick lands again.
	d.kickMergeQueue()
	select {
	case <-d.kickMerge:
	default:
		t.Fatal("a post-drain kick should buffer again")
	}
}

// queuedMergeFixture seeds a project + an awaiting_merge task + a done
// finish-branch run carrying a PR, then acquires the guard exactly as the
// queue's dispatcher (checkOneMerge, Task 4) would before spawning the worker.
// It returns the loaded project/task so runQueuedMerge can be driven directly.
func queuedMergeFixture(t *testing.T, d *Daemon, slug, branch, projConfig string) (*store.Project, *store.Task) {
	t.Helper()
	ctx := context.Background()
	if projConfig != "" {
		writePerProjectConfig(t, d.HiveDir(), slug, projConfig)
	}
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: slug, Slug: slug, Name: slug, Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: slug + "-fin", TaskID: slug + "-t", ProjectID: slug, Pipeline: "finish-branch", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SetRunPR(ctx, slug+"-fin", "https://github.com/o/r/pull/1", 1); err != nil {
		t.Fatal(err)
	}
	// The dispatcher acquires the guard before spawning the worker; mirror that so
	// the worker's deferred release is exercised against a held guard.
	if !d.mergeGuard.tryAcquire(branch) {
		t.Fatalf("guard for %q should be free at fixture setup", branch)
	}
	proj, err := d.store.GetProjectBySlug(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := d.store.GetTask(ctx, slug+"-t")
	if err != nil {
		t.Fatal(err)
	}
	return proj, tk
}

// TestRunQueuedMergeCleanMerge: a clean merge does NOT park needs_attention,
// attempts exactly one merge, and releases the guard on return.
func TestRunQueuedMergeCleanMerge(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	proj, tk := queuedMergeFixture(t, d, "clean", "spec/feat", "")

	stub := &stubGateway{}
	d.prGateway = stub

	d.runQueuedMerge(ctx, tk, proj, "spec/feat")

	if got, _ := d.store.GetTask(ctx, tk.ID); got.Status == "needs_attention" {
		t.Errorf("clean merge must not park needs_attention; got %s", got.Status)
	}
	if len(stub.merges) != 1 {
		t.Errorf("expected exactly 1 merge attempt, got %d", len(stub.merges))
	}
	// Guard released after the worker returns.
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Error("guard must be released after the worker finishes")
	}
}

// TestRunQueuedMergeAlreadyMerged: an already-merged error is success-equivalent
// — no resolve, no needs_attention, one merge attempt, guard released.
func TestRunQueuedMergeAlreadyMerged(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	// resolveAuto ON so we can prove already-merged does NOT take the resolve path
	// even when resolving is enabled.
	proj, tk := queuedMergeFixture(t, d, "amerged", "spec/feat", "[pipelines.resolve]\nauto = true\n")

	stub := &stubGateway{mergeErr: errors.New("gh pr merge https://…: exit status 1: Pull request #5 is already merged")}
	d.prGateway = stub

	d.runQueuedMerge(ctx, tk, proj, "spec/feat")

	if got, _ := d.store.GetTask(ctx, tk.ID); got.Status == "needs_attention" {
		t.Errorf("already-merged is success-equivalent; must not park needs_attention, got %s", got.Status)
	}
	if len(stub.merges) != 1 {
		t.Errorf("expected exactly 1 merge attempt (no resolve re-merge), got %d", len(stub.merges))
	}
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Error("guard must be released after the worker finishes")
	}
}

// TestRunQueuedMergeOtherError: a non-conflict, non-already-merged failure
// (e.g. branch protection) parks needs_attention without dispatching a resolve.
func TestRunQueuedMergeOtherError(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	proj, tk := queuedMergeFixture(t, d, "other", "spec/feat", "[pipelines.resolve]\nauto = true\n")

	stub := &stubGateway{mergeErr: errors.New("gh pr merge https://…: exit status 1: refusing to merge: branch protection rules not satisfied")}
	d.prGateway = stub

	d.runQueuedMerge(ctx, tk, proj, "spec/feat")

	got, _ := d.store.GetTask(ctx, tk.ID)
	if got.Status != "needs_attention" {
		t.Errorf("a non-conflict merge failure must park needs_attention; got %s", got.Status)
	}
	// Branch-protection is NOT a conflict, so the resolver must not have re-merged.
	if len(stub.merges) != 1 {
		t.Errorf("expected exactly 1 merge attempt (no resolve), got %d", len(stub.merges))
	}
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Error("guard must be released after the worker finishes")
	}
}

// TestRunQueuedMergeConflictDispatchesResolve: a content conflict with resolveAuto
// ON takes the resolve branch (dispatchResolveRunManual). With no recoverable PR
// branch the manual dispatch errors out and — because retrying a resolve that
// can't even be PROVISIONED is futile (e.g. the PR branch was deleted post-merge)
// — the worker parks TERMINALLY at merge_failed (NOT awaiting_merge, which would
// re-loop every 30s). resolveAuto being ON is required to reach this branch; we
// assert no double-merge and the guard is released. dispatchResolveRunManual is
// synchronous, so the guard is provably held for the whole call and freed on return.
func TestRunQueuedMergeConflictDispatchesResolve(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	proj, tk := queuedMergeFixture(t, d, "conflict", "spec/feat", "[pipelines.resolve]\nauto = true\n")

	// gh's content-conflict message; classified by isMergeConflictErr.
	stub := &stubGateway{mergeErr: errors.New("gh pr merge https://…: exit status 1: Pull Request is not mergeable: the merge commit cannot be cleanly created")}
	d.prGateway = stub

	// No metadata branch and no run recorded a branch ⇒ resolvePRBranchForTask
	// fails fast inside dispatchResolveRunManual, so it returns an error WITHOUT
	// provisioning a worktree or running the pipeline — exercising the resolve
	// branch + its terminal park-on-dispatch-error fallback deterministically.
	d.runQueuedMerge(ctx, tk, proj, "spec/feat")

	got, _ := d.store.GetTask(ctx, tk.ID)
	if got.Status != "needs_attention" {
		t.Errorf("conflict whose resolve could not dispatch must park needs_attention; got %s", got.Status)
	}
	if got.GateState != sequence.GateMergeFailed {
		t.Errorf("resolve-could-not-be-provisioned must park TERMINALLY at merge_failed; got %s", got.GateState)
	}
	// Exactly one merge attempt by the worker; the resolve never reached a
	// re-merge (it failed to dispatch), so no double-merge.
	if len(stub.merges) != 1 {
		t.Errorf("expected exactly 1 merge attempt, got %d", len(stub.merges))
	}
	// Guard released only after the (synchronous) resolve attempt returned.
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Error("guard must be released after the worker (and its resolve attempt) finishes")
	}
}

// TestRunQueuedMergeConflictNoResolveAuto: a content conflict with resolveAuto OFF
// must NOT take the resolve path — it parks needs_attention directly. This pins
// down that resolveAuto gates the resolve branch (distinguishing it from the
// other-error park).
func TestRunQueuedMergeConflictNoResolveAuto(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	proj, tk := queuedMergeFixture(t, d, "noauto", "spec/feat", "") // resolve auto defaults off

	stub := &stubGateway{mergeErr: errors.New("gh pr merge https://…: exit status 1: Pull Request is not mergeable: the merge commit cannot be cleanly created")}
	d.prGateway = stub

	d.runQueuedMerge(ctx, tk, proj, "spec/feat")

	got, _ := d.store.GetTask(ctx, tk.ID)
	if got.Status != "needs_attention" {
		t.Errorf("conflict with resolveAuto off must park needs_attention; got %s", got.Status)
	}
	if len(stub.merges) != 1 {
		t.Errorf("expected exactly 1 merge attempt, got %d", len(stub.merges))
	}
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Error("guard must be released after the worker finishes")
	}
}

// TestRunQueuedMergeNoPR: a task with no PR returns immediately (no merge, no
// park) and releases the guard.
func TestRunQueuedMergeNoPR(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "nopr", Slug: "nopr", Name: "nopr", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "nopr-t", ProjectID: "nopr", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Fatal("guard should be free")
	}
	proj, _ := d.store.GetProjectBySlug(ctx, "nopr")
	tk, _ := d.store.GetTask(ctx, "nopr-t")

	stub := &stubGateway{}
	d.prGateway = stub
	d.runQueuedMerge(ctx, tk, proj, "spec/feat")

	if len(stub.merges) != 0 {
		t.Errorf("no PR ⇒ no merge attempt, got %d", len(stub.merges))
	}
	if got, _ := d.store.GetTask(ctx, tk.ID); got.Status == "needs_attention" {
		t.Errorf("no-PR is a no-op, not a park; got %s", got.Status)
	}
	if !d.mergeGuard.tryAcquire("spec/feat") {
		t.Error("guard must be released after the worker finishes")
	}
}
