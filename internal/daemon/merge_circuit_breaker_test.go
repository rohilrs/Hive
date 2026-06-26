package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

// TestMergeAttemptTracker pins the circuit-breaker counter's two operations:
// bump increments per task (independently) and returns the new count; reset
// clears a task back to zero so a re-queued task starts clean.
func TestMergeAttemptTracker(t *testing.T) {
	tr := newMergeAttemptTracker()

	if got := tr.bump("a"); got != 1 {
		t.Errorf("first bump of a = %d, want 1", got)
	}
	if got := tr.bump("a"); got != 2 {
		t.Errorf("second bump of a = %d, want 2", got)
	}
	// A different task counts independently.
	if got := tr.bump("b"); got != 1 {
		t.Errorf("first bump of b = %d, want 1 (independent of a)", got)
	}
	if got := tr.bump("a"); got != 3 {
		t.Errorf("third bump of a = %d, want 3", got)
	}

	tr.reset("a")
	if got := tr.bump("a"); got != 1 {
		t.Errorf("bump after reset of a = %d, want 1", got)
	}
	// reset(a) must not have touched b.
	if got := tr.bump("b"); got != 2 {
		t.Errorf("bump of b after reset(a) = %d, want 2 (untouched)", got)
	}

	// reset of an unknown key is a no-op (no panic).
	tr.reset("never-seen")
}

// TestParkMergeFailedSetsTerminalGate confirms parkMergeFailed sets the TERMINAL
// merge_failed gate + needs_attention status (so detectMerges never re-picks it
// but it stays a visible phase blocker for a human).
func TestParkMergeFailedSetsTerminalGate(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "mf", Slug: "mf", Name: "mf", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "mf-t", ProjectID: "mf", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	proj, _ := d.store.GetProjectBySlug(ctx, "mf")

	d.scheduler.parkMergeFailed(ctx, proj, "mf-t", "gave up after 3 failed merge attempts")

	got, _ := d.store.GetTask(ctx, "mf-t")
	if got.GateState != sequence.GateMergeFailed {
		t.Errorf("gate = %q, want merge_failed (terminal)", got.GateState)
	}
	if got.Status != "needs_attention" {
		t.Errorf("status = %q, want needs_attention (visible blocker)", got.Status)
	}
}

// TestCheckOneMergeCapStopsDispatching drives checkOneMerge end-to-end: with the
// PR never merging and auto-merge ON, each pass dispatches one merge worker and
// bumps the counter. After mergeAttemptCap dispatches the next pass exceeds the
// cap and parks the task TERMINALLY at merge_failed instead of dispatching again
// — the circuit breaker. We make Merge() succeed (returns nil) so the worker
// leaves the task at awaiting_merge without parking, keeping each pass clean and
// the guard freed for the next pass.
func TestCheckOneMergeCapStopsDispatching(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "cb", Slug: "cb", Name: "cb", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	// auto_merge_on_green ⇒ autoMergePolicy true ⇒ checkOneMerge dispatches.
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "cb", Status: "active", AdvancementPolicy: "auto_merge_on_green"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "cb-t", ProjectID: "cb", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "cb-fin", TaskID: "cb-t", ProjectID: "cb", Pipeline: "finish-branch", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SetRunPR(ctx, "cb-fin", "https://github.com/o/r/pull/1", 1); err != nil {
		t.Fatal(err)
	}

	// merged:false so checkOneMerge always takes the dispatch path; Merge() returns
	// nil so the spawned worker is a no-op park-wise (leaves awaiting_merge).
	stub := &stubGateway{merged: false}
	d.prGateway = stub

	proj, _ := d.store.GetProjectBySlug(ctx, "cb")
	tk, _ := d.store.GetTask(ctx, "cb-t")
	branch := d.scheduler.effectiveWorktreeBaseForProject("cb") // "main"
	key := mergeGuardKey("cb", branch)

	// Drive exactly mergeAttemptCap passes that each dispatch a worker. Wait for the
	// guard to free after each (the worker releases it on return) so the next pass
	// can acquire.
	for i := 0; i < mergeAttemptCap; i++ {
		d.checkOneMerge(ctx, tk, proj)
		waitGuardFree(t, d, key)
		if got, _ := d.store.GetTask(ctx, "cb-t"); got.GateState != sequence.GateAwaitingMerge {
			t.Fatalf("pass %d: gate=%q, want still awaiting_merge (under cap)", i, got.GateState)
		}
	}
	mergesAtCap := stub.mergeCount()

	// The next pass bumps to mergeAttemptCap+1 (> cap) ⇒ parks merge_failed, does
	// NOT dispatch a worker, and releases the guard inline.
	d.checkOneMerge(ctx, tk, proj)

	got, _ := d.store.GetTask(ctx, "cb-t")
	if got.GateState != sequence.GateMergeFailed {
		t.Fatalf("after exceeding cap: gate=%q, want merge_failed", got.GateState)
	}
	if got.Status != "needs_attention" {
		t.Errorf("after exceeding cap: status=%q, want needs_attention", got.Status)
	}
	// No new worker dispatched on the capped pass: merge count unchanged.
	if stub.mergeCount() != mergesAtCap {
		t.Errorf("capped pass must not dispatch a merge worker: merges %d -> %d", mergesAtCap, stub.mergeCount())
	}
	// Guard freed inline (no worker spawned), so it must be acquirable now.
	if !d.mergeGuard.tryAcquire(key) {
		t.Error("capped pass must release the guard it acquired")
	}
	d.mergeGuard.release(key)

	// A merge_failed task is no longer queried by detectMerges (it only lists
	// awaiting_merge), so a fresh detectMerges pass must NOT re-pick it: the gate
	// stays merge_failed and no further merge is attempted.
	mergesBefore := stub.mergeCount()
	d.detectMerges(ctx)
	waitGuardFree(t, d, key)
	if got, _ := d.store.GetTask(ctx, "cb-t"); got.GateState != sequence.GateMergeFailed {
		t.Errorf("merge_failed task must stay terminal across detectMerges; got %q", got.GateState)
	}
	if stub.mergeCount() != mergesBefore {
		t.Errorf("detectMerges must not re-pick a merge_failed task: merges %d -> %d", mergesBefore, stub.mergeCount())
	}
}

// TestHandleResolveNowRefusesMergeFailed pins the sticky-terminal invariant at
// the RPC boundary: a manual `hive resolve` on a task parked at the TERMINAL
// merge_failed gate must be refused (it would otherwise run a resolve that
// re-arms the gate back to awaiting_merge and restart the auto-merge loop the
// circuit breaker exists to stop). The refusal points the user at `hive merge
// retry`.
func TestHandleResolveNowRefusesMergeFailed(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "rf", Slug: "rf", Name: "rf", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "rf-t", ProjectID: "rf", Source: "inbox", Title: "x", Status: "needs_attention", GateState: sequence.GateMergeFailed, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}

	s := &RPCServer{d: d}
	params, _ := json.Marshal(ResolveNowParams{TaskID: "rf-t"})
	_, rpcErr := s.handleResolveNow(ctx, params)
	if rpcErr == nil {
		t.Fatal("expected handleResolveNow to refuse a merge_failed task")
	}
	if !strings.Contains(rpcErr.Message, "merge retry") {
		t.Errorf("refusal message %q should point at `hive merge retry`", rpcErr.Message)
	}
}

// TestParkResolveDoesNotDowngradeMergeFailed pins the defense-in-depth guard in
// parkResolveNeedsAttention: it must NEVER downgrade a TERMINAL merge_failed
// gate back to awaiting_merge (which detectMerges would re-pick, re-arming the
// loop). It only refreshes status to needs_attention and leaves the gate sticky.
func TestParkResolveDoesNotDowngradeMergeFailed(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "pr", Slug: "pr", Name: "pr", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "pr-t", ProjectID: "pr", Source: "inbox", Title: "x", Status: "needs_attention", GateState: sequence.GateMergeFailed, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	proj, _ := d.store.GetProjectBySlug(ctx, "pr")

	d.scheduler.parkResolveNeedsAttention(ctx, proj, "pr-t")

	got, _ := d.store.GetTask(ctx, "pr-t")
	if got.GateState != sequence.GateMergeFailed {
		t.Errorf("gate=%q; parkResolveNeedsAttention must NOT downgrade merge_failed to awaiting_merge", got.GateState)
	}
}

// waitGuardFree polls until the merge guard for key is acquirable (the worker
// goroutine has released it) and immediately releases it again, leaving the
// guard free for the caller's next checkOneMerge pass.
func waitGuardFree(t *testing.T, d *Daemon, key string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.mergeGuard.tryAcquire(key) {
			d.mergeGuard.release(key)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("merge guard %q never freed (worker did not release)", key)
}
