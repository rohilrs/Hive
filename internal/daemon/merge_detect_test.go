package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

type stubGateway struct {
	merged  bool
	baseRef string
	err     error
	// mu guards merges so the merge-queue worker (a goroutine) and a polling
	// test can touch it without a data race. Synchronous tests may still read
	// merges directly after the call returns.
	mu       sync.Mutex
	merges   []string
	mergeErr error

	// reMergeAfterResolve simulation:
	// mergeableSeq returns successive Mergeable() verdicts (last value repeats);
	// empty defaults to "MERGEABLE". mergeableErr forces a Mergeable() error.
	// mergeFailN fails the first N Merge() calls with mergeErr then succeeds
	// (simulates checks/locks still settling); 0 means mergeErr applies to all
	// calls (existing behaviour).
	mergeableSeq   []string
	mergeableErr   error
	mergeableCalls int
	mergeFailN     int
	// mergeableErrN errors the first N Mergeable() calls with mergeableErr then
	// behaves normally (simulates a transient gh hiccup that recovers). 0 means
	// mergeableErr (if set) applies to all calls.
	mergeableErrN int
}

func (g *stubGateway) State(_ context.Context, _ string) (bool, string, error) {
	return g.merged, g.baseRef, g.err
}
func (g *stubGateway) Mergeable(_ context.Context, _ string) (string, error) {
	i := g.mergeableCalls
	g.mergeableCalls++
	if g.mergeableErr != nil {
		if g.mergeableErrN == 0 || i < g.mergeableErrN {
			return "", g.mergeableErr
		}
		// past the transient-error window — fall through to the normal verdict
	}
	if len(g.mergeableSeq) == 0 {
		return "MERGEABLE", nil
	}
	if i >= len(g.mergeableSeq) {
		i = len(g.mergeableSeq) - 1
	}
	return g.mergeableSeq[i], nil
}
func (g *stubGateway) Merge(_ context.Context, prURL, _ string) error {
	g.mu.Lock()
	g.merges = append(g.merges, prURL)
	n := len(g.merges)
	g.mu.Unlock()
	if g.mergeFailN > 0 {
		if n <= g.mergeFailN {
			return g.mergeErr
		}
		return nil
	}
	return g.mergeErr
}

func (g *stubGateway) OpenPR(_ context.Context, _, _, _, _, _ string, _ bool) (string, error) {
	return "https://github.com/stub/pr/1", nil
}

// mergeCount returns the number of Merge calls so far, race-free — for tests
// that observe a merge attempt made by the merge-queue worker goroutine.
func (g *stubGateway) mergeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.merges)
}

func TestDetectMergesSatisfiesOnMerge(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "seqp", Name: "S", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "p1", Status: "active", AdvancementPolicy: "human_merge"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "r1", TaskID: "t1", ProjectID: "p1", Pipeline: "finish-branch", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	_ = d.store.SetRunPR(ctx, "r1", "https://github.com/o/r/pull/7", 7)

	d.prGateway = &stubGateway{merged: false}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "t1"); got.GateState != sequence.GateAwaitingMerge {
		t.Fatalf("unmerged: gate=%q, want awaiting_merge", got.GateState)
	}
	d.prGateway = &stubGateway{merged: true, baseRef: "develop"}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "t1"); got.GateState != sequence.GateAwaitingMerge {
		t.Fatalf("wrong base: gate=%q, want awaiting_merge", got.GateState)
	}
	d.prGateway = &stubGateway{merged: true, baseRef: "main"}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "t1"); got.GateState != sequence.GateSatisfied {
		t.Fatalf("merged: gate=%q, want satisfied", got.GateState)
	}

	// Paused dispatcher: with the dispatcher filter removed, the merge IS now
	// recorded (gate -> satisfied). Phase advancement is separately gated by the
	// scheduler (which skips paused dispatchers), so this is safe. (Phase 2)
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t2", ProjectID: "p1", Source: "inbox", Title: "y", Status: "done", GateState: sequence.GateAwaitingMerge}); err != nil {
		t.Fatal(err)
	}
	_ = d.store.InsertRun(ctx, &store.Run{ID: "r2", TaskID: "t2", ProjectID: "p1", Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(ctx, "r2", "https://github.com/o/r/pull/8", 8)
	_ = d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "p1", Status: "paused", AdvancementPolicy: "human_merge"})
	d.prGateway = &stubGateway{merged: true, baseRef: "main"}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "t2"); got.GateState != sequence.GateSatisfied {
		t.Fatalf("paused project merge: gate=%q, want satisfied (filter removed)", got.GateState)
	}
}

// TestDetectMergesNonSequencedNoDispatcher confirms a task with no sequenced
// dispatcher at all (e.g. a non-sequenced project) is still polled + satisfied.
func TestDetectMergesNonSequencedNoDispatcher(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "np", Slug: "nonseq", Name: "N", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	// No UpsertSequenceDispatcher for "np" — non-sequenced project.
	if err := d.store.InsertTask(ctx, &store.Task{ID: "nt", ProjectID: "np", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge}); err != nil {
		t.Fatal(err)
	}
	_ = d.store.InsertRun(ctx, &store.Run{ID: "nr", TaskID: "nt", ProjectID: "np", Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(ctx, "nr", "https://github.com/o/r/pull/9", 9)
	d.prGateway = &stubGateway{merged: true, baseRef: "main"} // effective target defaults to "main"
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "nt"); got.GateState != sequence.GateSatisfied {
		t.Fatalf("non-seq no-dispatcher: gate=%q, want satisfied", got.GateState)
	}
}

// TestMergeDetectFlipsStrandedTaskToDone confirms that after detectMerges flips
// a task's gate to satisfied, the task status is immediately refreshed to "done"
// — even if the task was previously stranded at "needs_attention".
func TestMergeDetectFlipsStrandedTaskToDone(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "pm", Slug: "seqm", Name: "M", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "pm", Status: "active", AdvancementPolicy: "human_merge"}); err != nil {
		t.Fatal(err)
	}
	// Task stranded at needs_attention with gate awaiting_merge (PR already opened).
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "tm", ProjectID: "pm", Source: "inbox", Title: "x",
		Status: "needs_attention", GateState: sequence.GateAwaitingMerge,
	}); err != nil {
		t.Fatal(err)
	}
	// A terminal finish-branch run exists (the run that opened the PR).
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: "rm", TaskID: "tm", ProjectID: "pm", Pipeline: "finish-branch", Status: "done",
	}); err != nil {
		t.Fatal(err)
	}
	_ = d.store.SetRunPR(ctx, "rm", "https://github.com/o/r/pull/42", 42)

	// Before merge: status stays needs_attention.
	d.prGateway = &stubGateway{merged: false}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "tm"); got.Status != "needs_attention" {
		t.Fatalf("before merge: status=%q, want needs_attention", got.Status)
	}

	// After merge: gate satisfied → status must flip to done.
	d.prGateway = &stubGateway{merged: true, baseRef: "main"}
	d.detectMerges(ctx)
	got, _ := d.store.GetTask(ctx, "tm")
	if got.GateState != sequence.GateSatisfied {
		t.Fatalf("after merge: gate=%q, want satisfied", got.GateState)
	}
	if got.Status != "done" {
		t.Fatalf("after merge: status=%q, want done (refreshTaskStatus must follow gate flip)", got.Status)
	}
}

// TestDetectMergesSatisfiesOnFeatureBranchBase confirms that when a project has
// an integration feature branch set, the merge detector uses that branch as the
// expected PR base — not the target branch. A merge into the target branch alone
// must NOT satisfy; a merge into the feature branch must satisfy.
func TestDetectMergesSatisfiesOnFeatureBranchBase(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	// Per-project config sets a feature branch → PRs target it, not the target branch.
	writePerProjectConfig(t, d.HiveDir(), "featp", "[integration]\nfeature_branch = \"spec/feat\"\n")
	rp := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p9", Slug: "featp", Name: "F", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "tf", ProjectID: "p9", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge}); err != nil {
		t.Fatal(err)
	}
	_ = d.store.InsertRun(ctx, &store.Run{ID: "rf", TaskID: "tf", ProjectID: "p9", Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(ctx, "rf", "https://github.com/o/r/pull/9", 9)

	// Merged into the TARGET branch "main" — but the PR base is the feature
	// branch, so this must NOT satisfy.
	d.prGateway = &stubGateway{merged: true, baseRef: "main"}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "tf"); got.GateState != sequence.GateAwaitingMerge {
		t.Fatalf("merge into target (not the feature-branch PR base): gate=%q, want still awaiting_merge", got.GateState)
	}
	// Merged into the feature branch — the actual PR base → satisfy.
	d.prGateway = &stubGateway{merged: true, baseRef: "spec/feat"}
	d.detectMerges(ctx)
	if got, _ := d.store.GetTask(ctx, "tf"); got.GateState != sequence.GateSatisfied {
		t.Fatalf("merge into feature branch (PR base): gate=%q, want satisfied", got.GateState)
	}
}

// TestCheckOneMergeDispatchesQueuedMerge confirms that for an auto-integrate
// project whose PR is NOT yet merged, checkOneMerge dispatches the guarded merge
// worker (the queue) which attempts the merge. The worker runs in a goroutine
// (under d.ctx), so we poll for the merge attempt with waitFor.
func TestCheckOneMergeDispatchesQueuedMerge(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	rp := initGitRepo(t)
	writePerProjectConfig(t, d.HiveDir(), "qp", "[integration]\nfeature_branch = \"spec/feat\"\ntask_auto_integrate = true\n")
	_ = d.store.InsertProject(ctx, &store.Project{ID: "qp", Slug: "qp", Name: "Q", Status: "active", RepoPath: &rp})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "qt", ProjectID: "qp", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateAwaitingMerge, Pipeline: "build", Priority: "P1"})
	_ = d.store.InsertRun(ctx, &store.Run{ID: "qf", TaskID: "qt", ProjectID: "qp", Pipeline: "finish-branch", Status: "done"})
	_ = d.store.SetRunPR(ctx, "qf", "https://github.com/o/r/pull/9", 9)

	// State: not merged → checkOneMerge should dispatch a merge worker that
	// attempts the merge (clean, no mergeErr → success-equivalent).
	stub := &stubGateway{merged: false}
	d.prGateway = stub
	proj, _ := d.store.GetProjectBySlug(ctx, "qp")
	tk, _ := d.store.GetTask(ctx, "qt")
	d.checkOneMerge(ctx, tk, proj)
	// The worker runs (goroutine) and attempts the merge; wait briefly.
	waitFor(t, func() bool { return stub.mergeCount() >= 1 }, 2*time.Second)
}
