package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

// TestMergeRetryRequiresMergeFailed pins the precondition: merge.retry is only a
// recovery path OUT of the terminal merge_failed gate. A task that is not parked
// there (e.g. still awaiting_merge under the normal flow) must be refused so the
// RPC can't be used to skip the circuit breaker or shortcut a live merge.
func TestMergeRetryRequiresMergeFailed(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "rq", Slug: "rq", Name: "rq", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "rq-t", ProjectID: "rq", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}

	s := &RPCServer{d: d}
	params, _ := json.Marshal(MergeRetryParams{TaskID: "rq-t"})
	_, rpcErr := s.handleMergeRetry(ctx, params)
	if rpcErr == nil {
		t.Fatal("expected handleMergeRetry to refuse a task not parked at merge_failed")
	}
	if !strings.Contains(rpcErr.Message, "merge_failed") {
		t.Errorf("refusal message %q should mention merge_failed", rpcErr.Message)
	}
}

// TestMergeRetryPRMergedSatisfies covers the reconcile path: a merge_failed task
// whose PR actually landed into the project's base (the merge queue gave up but
// the PR merged anyway, e.g. a manual merge or a late-settling GitHub state).
// merge.retry sees State→merged into the right base and reconciles the task to
// satisfied rather than re-arming a pointless merge loop.
func TestMergeRetryPRMergedSatisfies(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "ms", Slug: "ms", Name: "ms", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "ms-t", ProjectID: "ms", Source: "inbox", Title: "x", Status: "needs_attention", GateState: sequence.GateMergeFailed, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	// Record a PR for the task the same way the merge pipeline does: a run row
	// carrying a pr_url (PRForTask reads the latest such run).
	if err := d.store.InsertRun(ctx, &store.Run{ID: "ms-fin", TaskID: "ms-t", ProjectID: "ms", Pipeline: "finish-branch", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SetRunPR(ctx, "ms-fin", "https://github.com/o/r/pull/7", 7); err != nil {
		t.Fatal(err)
	}

	// The gateway must report the PR merged into the SAME base the project
	// resolves to, else the "merged into correct base" guard won't fire. Read the
	// effective base and feed it to the stub so the assertion is robust to the
	// default ("main").
	base := d.scheduler.effectiveWorktreeBaseForProject("ms")
	d.prGateway = &stubGateway{merged: true, baseRef: base}

	s := &RPCServer{d: d}
	params, _ := json.Marshal(MergeRetryParams{TaskID: "ms-t"})
	out, rpcErr := s.handleMergeRetry(ctx, params)
	if rpcErr != nil {
		t.Fatalf("handleMergeRetry errored: %v", rpcErr)
	}
	if !strings.Contains(string(out), "satisfied") {
		t.Errorf("result %q should report the satisfied action", string(out))
	}
	got, _ := d.store.GetTask(ctx, "ms-t")
	if got.GateState != sequence.GateSatisfied {
		t.Errorf("gate=%q, want satisfied (PR already merged into base)", got.GateState)
	}
}

// TestMergeRetryPROpenReArms covers the re-arm path: a merge_failed task whose PR
// is still OPEN (not merged). merge.retry resets the attempt counter and flips
// the gate back to awaiting_merge so the auto-resolver/merge queue gets a fresh
// budget. We assert the counter reset by bumping twice beforehand and confirming
// the post-handler bump restarts at 1.
func TestMergeRetryPROpenReArms(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "ra", Slug: "ra", Name: "ra", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "ra-t", ProjectID: "ra", Source: "inbox", Title: "x", Status: "needs_attention", GateState: sequence.GateMergeFailed, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "ra-fin", TaskID: "ra-t", ProjectID: "ra", Pipeline: "finish-branch", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SetRunPR(ctx, "ra-fin", "https://github.com/o/r/pull/8", 8); err != nil {
		t.Fatal(err)
	}

	// PR not merged ⇒ re-arm path.
	d.prGateway = &stubGateway{merged: false}

	// Simulate prior failed attempts so the reset is observable.
	d.mergeAttempts.bump("ra-t")
	d.mergeAttempts.bump("ra-t")

	s := &RPCServer{d: d}
	params, _ := json.Marshal(MergeRetryParams{TaskID: "ra-t"})
	out, rpcErr := s.handleMergeRetry(ctx, params)
	if rpcErr != nil {
		t.Fatalf("handleMergeRetry errored: %v", rpcErr)
	}
	if !strings.Contains(string(out), "rearmed") {
		t.Errorf("result %q should report the rearmed action", string(out))
	}
	got, _ := d.store.GetTask(ctx, "ra-t")
	if got.GateState != sequence.GateAwaitingMerge {
		t.Errorf("gate=%q, want awaiting_merge (re-armed)", got.GateState)
	}
	if n := d.mergeAttempts.bump("ra-t"); n != 1 {
		t.Errorf("attempt counter bump after re-arm = %d, want 1 (counter was reset)", n)
	}
}
