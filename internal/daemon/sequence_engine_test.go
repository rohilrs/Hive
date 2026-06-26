package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// fakeResolvePipeline stands in for the real resolve pipeline so dispatchResolveRun
// can be exercised end-to-end (post-resolve merge/park branches) without a live
// git worktree.
type fakeResolvePipeline struct {
	status  string
	summary string
}

func (f *fakeResolvePipeline) Name() string { return "resolve" }
func (f *fakeResolvePipeline) Run(_ context.Context, _ *pipeline.Run) (*pipeline.Result, error) {
	return &pipeline.Result{Status: f.status, Summary: f.summary}, nil
}

func TestFinishChainEndedGateTransitions(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "seqp", Name: "S", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	mkRun := func(taskID string) *store.Task {
		task := &store.Task{ID: taskID, ProjectID: "p1", Source: "inbox", Title: taskID, Status: "running", GateState: sequence.GateBuilt}
		if err := d.store.InsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := d.store.InsertRun(ctx, &store.Run{ID: "run-" + taskID, TaskID: taskID, ProjectID: "p1", Pipeline: "finish-branch", Status: "running"}); err != nil {
			t.Fatal(err)
		}
		return task
	}
	proj, err := d.store.GetProjectBySlug(ctx, "seqp")
	if err != nil {
		t.Fatal(err)
	}

	// No dispatcher row for p1 -> advancementPolicy falls back to pr_opened -> satisfied.
	tk := mkRun("t-ok")
	d.scheduler.finishChainEnded(ctx, "run-t-ok", tk, proj, "done", "ok", "https://github.com/o/r/pull/1", 1, "", "")
	got, _ := d.store.GetTask(ctx, "t-ok")
	if got.GateState != sequence.GateSatisfied || got.Status != "done" {
		t.Errorf("done: gate=%q status=%q, want satisfied/done", got.GateState, got.Status)
	}

	tk2 := mkRun("t-fail")
	d.scheduler.finishChainEnded(ctx, "run-t-fail", tk2, proj, "needs_attention", "ci red", "", 0, "", "")
	got2, _ := d.store.GetTask(ctx, "t-fail")
	if got2.GateState != sequence.GateBuilt || got2.Status != "needs_attention" {
		t.Errorf("fail: gate=%q status=%q, want built/needs_attention", got2.GateState, got2.Status)
	}
}

// TestFinishChainEndedPolicyAware verifies the policy-aware landing:
// merge-gated policies (human_merge, auto_merge_on_green, manual) park the gate
// at awaiting_merge (the merge detector/queue flips it to satisfied on the actual
// PR merge). Post-merge-queue (Task 5) finishChainEnded NEVER merges inline — not
// even for auto_merge_on_green — so NO policy triggers a gh pr merge here.
func TestFinishChainEndedPolicyAware(t *testing.T) {
	const prURL = "https://github.com/o/r/pull/1"
	for _, policy := range []string{"human_merge", "auto_merge_on_green", "manual"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			d := newTestDaemon(t)
			ctx := context.Background()
			projID := "p-" + policy
			slug := "seqp-" + policy
			if err := d.store.InsertProject(ctx, &store.Project{ID: projID, Slug: slug, Name: "S", Status: "active"}); err != nil {
				t.Fatal(err)
			}
			if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: projID, Status: "active", AdvancementPolicy: policy}); err != nil {
				t.Fatal(err)
			}
			if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: projID, Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}); err != nil {
				t.Fatal(err)
			}
			if err := d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: projID, Pipeline: "finish-branch", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			proj, err := d.store.GetProjectBySlug(ctx, slug)
			if err != nil {
				t.Fatal(err)
			}
			task, err := d.store.GetTask(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}

			stub := &stubGateway{}
			d.prGateway = stub

			d.scheduler.finishChainEnded(ctx, "r1", task, proj, "done", "ok", prURL, 1, "", "")

			got, _ := d.store.GetTask(ctx, "t1")
			if got.GateState != sequence.GateAwaitingMerge {
				t.Errorf("gate=%q, want awaiting_merge", got.GateState)
			}
			if got.Status != "done" {
				t.Errorf("status=%q, want done", got.Status)
			}

			// finishChainEnded no longer merges inline for ANY policy — the queue
			// owns the merge (Task 5). So no policy may attempt a gh pr merge here.
			if len(stub.merges) != 0 {
				t.Errorf("merges=%v, want no merge for policy %q (the queue owns the merge)", stub.merges, policy)
			}
		})
	}
}

// TestFinishChainEnded_AutoIntegrateMergeFailureNeedsAttention confirms that,
// post-merge-queue (Task 5), finishChainEnded does NOT merge inline — so it
// cannot observe a merge failure and cannot park needs_attention itself. An
// auto-integrate task whose finish-branch run is done simply lands at
// awaiting_merge (status done, slot released); the QUEUE then attempts the merge
// and parks needs_attention on a non-conflict failure (covered by the
// runQueuedMerge tests). The gateway is given a failing mergeErr purely to assert
// finishChainEnded never calls Merge.
func TestFinishChainEnded_AutoIntegrateMergeFailureNeedsAttention(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	writePerProjectConfig(t, d.HiveDir(), "ai", "[integration]\nfeature_branch = \"spec/feat\"\ntask_auto_integrate = true\n")
	rp := t.TempDir()
	proj := &store.Project{ID: "pa", Slug: "ai", Name: "AI", Status: "active", RepoPath: &rp}
	_ = d.store.InsertProject(ctx, proj)
	task := &store.Task{ID: "ta", ProjectID: "pa", Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}
	_ = d.store.InsertTask(ctx, task)
	_ = d.store.InsertRun(ctx, &store.Run{ID: "ra", TaskID: "ta", ProjectID: "pa", Pipeline: "finish-branch", Status: "running"})
	stub := &stubGateway{mergeErr: errors.New("branch protection: required checks")}
	d.prGateway = stub

	d.scheduler.finishChainEnded(ctx, "ra", task, proj, "done", "branch finished", "https://github.com/o/r/pull/5", 5, "", "")

	if len(stub.merges) != 0 {
		t.Errorf("finishChainEnded attempted %d merges, want 0 (the queue owns the merge)", len(stub.merges))
	}
	got, _ := d.store.GetTask(ctx, "ta")
	if got.Status != "done" {
		t.Errorf("status=%q, want done (parked at awaiting_merge; the queue handles merge failure)", got.Status)
	}
	if got.GateState != sequence.GateAwaitingMerge {
		t.Errorf("gate=%q, want awaiting_merge", got.GateState)
	}
}

// TestSequenceEngineAutoDispatchesResolveOnConflict verifies that finishChainEnded
// NO LONGER merges (or dispatches a resolve) inline. After the merge-queue refactor
// (Task 5), an auto-integrate task whose finish-branch run is done simply parks at
// awaiting_merge — releasing its worker slot — and the merge QUEUE
// (checkOneMerge → runQueuedMerge) owns the merge + conflict-resolve. So even with
// [pipelines.resolve] auto = true and a conflict-classified merge error available,
// finishChainEnded itself must NOT call prGateway.Merge nor dispatchResolveFn.
//
// The merge/resolve-on-conflict behavior is now tested by the queue tests
// (TestRunQueuedMergeClassifies + TestCheckOneMergeDispatchesQueuedMerge).
func TestSequenceEngineAutoDispatchesResolveOnConflict(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	writePerProjectConfig(t, d.HiveDir(), "rc", "[integration]\nfeature_branch = \"spec/feat\"\ntask_auto_integrate = true\n[pipelines.resolve]\nauto = true\n")
	rp := t.TempDir()
	proj := &store.Project{ID: "prc", Slug: "rc", Name: "RC", Status: "active", RepoPath: &rp}
	_ = d.store.InsertProject(ctx, proj)
	task := &store.Task{ID: "trc", ProjectID: "prc", Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}
	_ = d.store.InsertTask(ctx, task)
	_ = d.store.InsertRun(ctx, &store.Run{ID: "rrc", TaskID: "trc", ProjectID: "prc", Pipeline: "finish-branch", Status: "running"})
	// A conflict-classified merge error is available, but finishChainEnded must
	// never attempt the merge — so this error must never surface.
	stub := &stubGateway{mergeErr: errors.New("gh pr merge https://…: exit status 1: Pull Request is not mergeable: conflict")}
	d.prGateway = stub

	var resolveCalls int
	d.scheduler.dispatchResolveFn = func(_ context.Context, tk *store.Task, _ *store.Project, _, _ string) error {
		resolveCalls++
		return nil
	}

	d.scheduler.finishChainEnded(ctx, "rrc", task, proj, "done", "branch finished", "https://github.com/o/r/pull/9", 9, "/wt/path", "hive/run-rrc")

	if len(stub.merges) != 0 {
		t.Errorf("finishChainEnded attempted %d merges, want 0 (the queue owns the merge)", len(stub.merges))
	}
	if resolveCalls != 0 {
		t.Errorf("dispatchResolve called %d times, want 0 (the queue owns conflict-resolve)", resolveCalls)
	}
	got, _ := d.store.GetTask(ctx, "trc")
	if got.GateState != sequence.GateAwaitingMerge {
		t.Errorf("gate=%q, want awaiting_merge", got.GateState)
	}
	if got.Status != "done" {
		t.Errorf("status=%q, want done (parked at awaiting_merge, slot released)", got.Status)
	}
}

// TestSequenceEngineNoResolveOnNonConflictMergeError verifies that, post-merge-
// queue (Task 5), finishChainEnded itself never merges or dispatches a resolve —
// regardless of [pipelines.resolve] auto. It parks the auto-integrate task at
// awaiting_merge (status done) and the QUEUE owns the merge: a non-conflict merge
// failure parking needs_attention is now the queue's job (runQueuedMerge tests).
func TestSequenceEngineNoResolveOnNonConflictMergeError(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	writePerProjectConfig(t, d.HiveDir(), "ncm", "[integration]\nfeature_branch = \"spec/feat\"\ntask_auto_integrate = true\n[pipelines.resolve]\nauto = true\n")
	rp := t.TempDir()
	proj := &store.Project{ID: "pncm", Slug: "ncm", Name: "NCM", Status: "active", RepoPath: &rp}
	_ = d.store.InsertProject(ctx, proj)
	task := &store.Task{ID: "tncm", ProjectID: "pncm", Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}
	_ = d.store.InsertTask(ctx, task)
	_ = d.store.InsertRun(ctx, &store.Run{ID: "rncm", TaskID: "tncm", ProjectID: "pncm", Pipeline: "finish-branch", Status: "running"})
	stub := &stubGateway{mergeErr: errors.New("gh pr merge https://…: exit status 1: required status checks not green")}
	d.prGateway = stub

	var resolveCalls int
	d.scheduler.dispatchResolveFn = func(_ context.Context, tk *store.Task, _ *store.Project, _, _ string) error {
		resolveCalls++
		return nil
	}

	d.scheduler.finishChainEnded(ctx, "rncm", task, proj, "done", "branch finished", "https://github.com/o/r/pull/10", 10, "/wt/path", "hive/run-rncm")

	if len(stub.merges) != 0 {
		t.Errorf("finishChainEnded attempted %d merges, want 0 (the queue owns the merge)", len(stub.merges))
	}
	if resolveCalls != 0 {
		t.Errorf("dispatchResolve called %d times, want 0 (the queue owns resolve)", resolveCalls)
	}
	got, _ := d.store.GetTask(ctx, "tncm")
	if got.Status != "done" {
		t.Errorf("status=%q, want done (parked at awaiting_merge; the queue handles the merge failure)", got.Status)
	}
	if got.GateState != sequence.GateAwaitingMerge {
		t.Errorf("gate=%q, want awaiting_merge", got.GateState)
	}
}

// TestFinishChainEnded_EmitsPROpened verifies that finishChainEnded publishes a
// task.pr_opened event (with the correct task_id and pr_number) when a prURL is
// present — i.e. the finish-branch run succeeded and created a PR.
func TestFinishChainEnded_EmitsPROpened(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	if err := d.store.InsertProject(ctx, &store.Project{ID: "pp", Slug: "demo", Name: "Demo", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "tp", ProjectID: "pp", Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "run-f", TaskID: "tp", ProjectID: "pp", Pipeline: "finish-branch", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	proj, err := d.store.GetProjectBySlug(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.store.GetTask(ctx, "tp")
	if err != nil {
		t.Fatal(err)
	}

	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	const prURL = "https://github.com/o/r/pull/7"
	d.scheduler.finishChainEnded(ctx, "run-f", task, proj, "done", "ok", prURL, 7, "", "")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for task.pr_opened event")
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed")
			}
			if ev.Type != rpc.EventPROpened {
				continue
			}
			taskID, _ := ev.Data["task_id"].(string)
			prNum, _ := ev.Data["pr_number"].(int)
			if taskID != "tp" {
				t.Errorf("pr_opened task_id=%q, want %q", taskID, "tp")
			}
			if prNum != 7 {
				t.Errorf("pr_opened pr_number=%d, want 7", prNum)
			}
			return // success
		}
	}
}

// fastReMergeScheduler returns a daemon whose scheduler polls mergeability on a
// sub-millisecond cadence so reMergeAfterResolve tests don't wait real seconds.
func fastReMergeScheduler(t *testing.T) *Daemon {
	t.Helper()
	d := newTestDaemon(t)
	d.scheduler.reMergeInterval = time.Millisecond
	d.scheduler.reMergeTimeout = 50 * time.Millisecond
	return d
}

// TestReMergeAfterResolveWaitsForMergeable is the regression for the Phase 3a
// dogfood finding: GitHub computes `mergeable` asynchronously after the resolve
// push, so an immediate merge races a stale "UNKNOWN". reMergeAfterResolve must
// poll until it settles to "MERGEABLE", THEN merge — not bail on the first read.
func TestReMergeAfterResolveWaitsForMergeable(t *testing.T) {
	d := fastReMergeScheduler(t)
	stub := &stubGateway{mergeableSeq: []string{"UNKNOWN", "UNKNOWN", "MERGEABLE"}}
	d.prGateway = stub

	if err := d.scheduler.reMergeAfterResolve(context.Background(), "https://github.com/o/r/pull/1", "squash"); err != nil {
		t.Fatalf("reMergeAfterResolve returned error: %v", err)
	}
	if len(stub.merges) != 1 {
		t.Fatalf("expected exactly 1 merge after mergeability settled, got %d (%v)", len(stub.merges), stub.merges)
	}
	if stub.mergeableCalls < 3 {
		t.Errorf("expected to poll mergeability through UNKNOWN (>=3 reads), got %d", stub.mergeableCalls)
	}
}

// TestReMergeAfterResolveRetriesTransientMergeFailure: once MERGEABLE, a merge
// can still transiently fail (checks/index locks settling). We retry within the
// budget rather than parking on the first failure.
func TestReMergeAfterResolveRetriesTransientMergeFailure(t *testing.T) {
	d := fastReMergeScheduler(t)
	stub := &stubGateway{
		mergeableSeq: []string{"MERGEABLE"},
		mergeFailN:   2,
		mergeErr:     errors.New("gh pr merge: exit status 1: Base branch was modified. Review and try the merge again."),
	}
	d.prGateway = stub

	if err := d.scheduler.reMergeAfterResolve(context.Background(), "https://github.com/o/r/pull/2", "squash"); err != nil {
		t.Fatalf("reMergeAfterResolve returned error after retries: %v", err)
	}
	if len(stub.merges) != 3 {
		t.Errorf("expected 3 merge attempts (2 fail, 1 succeed), got %d", len(stub.merges))
	}
}

// TestDispatchResolveRunParksNeedsAttentionWhenReMergeFails: when the resolve
// succeeds (pushes the resolution) but the re-merge can't complete, the task
// must flip to needs_attention — NOT keep its prior "running" status. The old
// code parked only the gate, leaving the task invisible and refusing a manual
// `hive resolve` (whose guard requires needs_attention). Regression for the
// Phase 3a dogfood status-stuck finding.
func TestDispatchResolveRunParksNeedsAttentionWhenReMergeFails(t *testing.T) {
	d := fastReMergeScheduler(t)
	ctx := context.Background()
	rp := t.TempDir()
	proj := &store.Project{ID: "prx", Slug: "rx", Name: "RX", Status: "active", RepoPath: &rp}
	_ = d.store.InsertProject(ctx, proj)
	task := &store.Task{ID: "trx", ProjectID: "prx", Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}
	_ = d.store.InsertTask(ctx, task)
	_ = d.store.InsertRun(ctx, &store.Run{ID: "fin-trx", TaskID: "trx", ProjectID: "prx", Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(ctx, "fin-trx", "https://github.com/o/r/pull/42", 42)
	d.pipelines["resolve"] = &fakeResolvePipeline{status: "done", summary: "resolved"}
	d.prGateway = &stubGateway{mergeableSeq: []string{"UNKNOWN"}} // never settles → re-merge fails

	if err := d.scheduler.dispatchResolveRun(ctx, task, proj, "/wt/path", "hive/run-trx"); err != nil {
		t.Fatalf("dispatchResolveRun returned error: %v", err)
	}
	got, _ := d.store.GetTask(ctx, "trx")
	if got.Status != "needs_attention" {
		t.Errorf("status=%q, want needs_attention (re-merge failed → must stay visible/re-resolvable)", got.Status)
	}
	if got.GateState != sequence.GateAwaitingMerge {
		t.Errorf("gate=%q, want awaiting_merge", got.GateState)
	}
}

// TestDispatchResolveRunCompletesMergeOnSuccess: the happy path — resolve done +
// re-merge succeeds → task done, gate awaiting_merge, exactly one merge.
func TestDispatchResolveRunCompletesMergeOnSuccess(t *testing.T) {
	d := fastReMergeScheduler(t)
	ctx := context.Background()
	rp := t.TempDir()
	proj := &store.Project{ID: "pry", Slug: "ry", Name: "RY", Status: "active", RepoPath: &rp}
	_ = d.store.InsertProject(ctx, proj)
	task := &store.Task{ID: "try", ProjectID: "pry", Source: "inbox", Title: "x", Status: "running", GateState: sequence.GateBuilt}
	_ = d.store.InsertTask(ctx, task)
	_ = d.store.InsertRun(ctx, &store.Run{ID: "fin-try", TaskID: "try", ProjectID: "pry", Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(ctx, "fin-try", "https://github.com/o/r/pull/43", 43)
	d.pipelines["resolve"] = &fakeResolvePipeline{status: "done", summary: "resolved"}
	stub := &stubGateway{} // mergeable defaults MERGEABLE; merge succeeds
	d.prGateway = stub

	if err := d.scheduler.dispatchResolveRun(ctx, task, proj, "/wt/path", "hive/run-try"); err != nil {
		t.Fatalf("dispatchResolveRun returned error: %v", err)
	}
	got, _ := d.store.GetTask(ctx, "try")
	if got.Status != "done" {
		t.Errorf("status=%q, want done (re-merge succeeded)", got.Status)
	}
	if len(stub.merges) != 1 {
		t.Errorf("expected exactly 1 merge, got %d", len(stub.merges))
	}
}

// TestReMergeAfterResolveRecoversFromTransientMergeableError: a transient gh
// failure querying mergeability is retried within budget, not treated as fatal.
func TestReMergeAfterResolveRecoversFromTransientMergeableError(t *testing.T) {
	d := fastReMergeScheduler(t)
	stub := &stubGateway{
		mergeableErr:  errors.New("gh pr view: exit status 1: API rate limit, retry"),
		mergeableErrN: 2, // first 2 reads error, then settle to MERGEABLE (default)
	}
	d.prGateway = stub

	if err := d.scheduler.reMergeAfterResolve(context.Background(), "https://github.com/o/r/pull/4", "squash"); err != nil {
		t.Fatalf("reMergeAfterResolve returned error despite recovery: %v", err)
	}
	if len(stub.merges) != 1 {
		t.Fatalf("expected 1 merge after the Mergeable hiccup cleared, got %d", len(stub.merges))
	}
	if stub.mergeableCalls < 3 {
		t.Errorf("expected to retry past the 2 transient errors (>=3 reads), got %d", stub.mergeableCalls)
	}
}

// TestReMergeAfterResolveTimesOutWhenNeverMergeable: a PR that never leaves
// UNKNOWN (or stays genuinely CONFLICTING) exhausts the budget and returns an
// error so the caller parks at awaiting_merge — the same safe terminal as
// before, never a spurious merge.
func TestReMergeAfterResolveTimesOutWhenNeverMergeable(t *testing.T) {
	d := fastReMergeScheduler(t)
	stub := &stubGateway{mergeableSeq: []string{"UNKNOWN"}} // never settles
	d.prGateway = stub

	if err := d.scheduler.reMergeAfterResolve(context.Background(), "https://github.com/o/r/pull/3", "squash"); err == nil {
		t.Fatal("expected timeout error when mergeability never settles, got nil")
	}
	if len(stub.merges) != 0 {
		t.Errorf("expected NO merge attempt while UNKNOWN, got %d", len(stub.merges))
	}
}

// TestResolveMergeOutcome pins the pure mapping from the post-resolve merge
// attempt to the run's terminal (status, summary): a confirmed merge says
// "merged into <base>" and is done; an unconfirmed one says the PR is still
// CONFLICTING and parks needs_attention. This is the seam that makes a resolve
// run's STORED summary honest — a run that ends with the PR still conflicting
// must never claim it merged.
func TestResolveMergeOutcome(t *testing.T) {
	status, summary := resolveMergeOutcome(true, "chat-test-harness")
	if status != "done" {
		t.Errorf("merged=true status=%q want done", status)
	}
	if !strings.Contains(summary, "merged into chat-test-harness") {
		t.Errorf("merged=true summary=%q must contain \"merged into chat-test-harness\"", summary)
	}

	status, summary = resolveMergeOutcome(false, "chat-test-harness")
	if status != "needs_attention" {
		t.Errorf("merged=false status=%q want needs_attention", status)
	}
	if !strings.Contains(summary, "still CONFLICTING") {
		t.Errorf("merged=false summary=%q must contain \"still CONFLICTING\"", summary)
	}
}
