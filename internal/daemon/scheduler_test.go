package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/conflict"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// TestSchedulerReEvaluatesPendingWithWaitingOn exercises the re-eval flow:
//   - rB is pending+waiting_on rA (which holds a.go in the guard)
//   - first reEvalQueuedRuns: rB still blocked → status unchanged, waiting_on stays
//   - rA completes: guard.Release + MarkRunEnded
//   - second reEvalQueuedRuns: rB is now eligible → waiting_on cleared, run started
func TestSchedulerReEvaluatesPendingWithWaitingOn(t *testing.T) {
	hiveDir := t.TempDir()
	cfg := config.Default()
	// Enable conflict guard so reEvalQueuedRuns does not short-circuit.
	cfg.ConflictGuard.Enabled = true

	guard := conflict.NewGuard()

	d, err := New(Config{
		HiveDir: hiveDir,
		Cfg:     cfg,
		Adapter: noopAdapter{},
		Guard:   guard,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Start gives the daemon a context + scheduler. We need d.ctx for
	// executePipeline, so we start the daemon (without waiting for the
	// socket — the scheduler loop is what matters here).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()

	// Wait for Start to finish wiring (sets d.ctx + d.scheduler + ...)
	// before reading those fields or calling Stop, per the Start contract.
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	storeCtx := context.Background()

	// Insert a project (no real repo needed; the noop adapter ignores the path).
	repoPath := t.TempDir()
	if err := d.store.InsertProject(storeCtx, &store.Project{
		ID:       "p",
		Slug:     "p",
		Name:     "P",
		Status:   "active",
		RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert task + run for rA ("running", holds a.go in the guard).
	if err := d.store.InsertTask(storeCtx, &store.Task{
		ID:        "tA",
		ProjectID: "p",
		Source:    "inbox",
		Title:     "A",
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(storeCtx, &store.Run{
		ID:        "rA",
		TaskID:    "tA",
		ProjectID: "p",
		Pipeline:  "build",
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.MarkRunStarted(storeCtx, "rA"); err != nil {
		t.Fatal(err)
	}
	// Reserve a.go for rA in the guard.
	guard.CheckAndReserve("rA", []string{"a.go"})

	// Insert task + run for rB, pending+waiting_on rA, overlapping a.go.
	if err := d.store.InsertTask(storeCtx, &store.Task{
		ID:        "tB",
		ProjectID: "p",
		Source:    "inbox",
		Title:     "B",
		Status:    "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(storeCtx, &store.Run{
		ID:        "rB",
		TaskID:    "tB",
		ProjectID: "p",
		Pipeline:  "build",
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}
	pred := &predictor.Result{Files: []string{"a.go", "b.go"}}
	predJSON, _ := json.Marshal(pred)
	if err := d.store.PutPredictionJSON(storeCtx, "rB", predJSON); err != nil {
		t.Fatal(err)
	}
	if err := d.store.SetWaitingOn(storeCtx, "rB", []string{"rA"}); err != nil {
		t.Fatal(err)
	}

	scheduler := d.scheduler

	// --- First re-eval: rB is still blocked (rA holds a.go). ---
	scheduler.reEvalQueuedRuns(storeCtx)

	rB, err := d.store.GetRun(storeCtx, "rB")
	if err != nil {
		t.Fatal(err)
	}
	if rB.Status != "pending" {
		t.Errorf("rB.Status=%s want pending while rA still holds a.go", rB.Status)
	}
	waitingOn, err := d.store.GetWaitingOn(storeCtx, "rB")
	if err != nil {
		t.Fatal(err)
	}
	if len(waitingOn) != 1 || waitingOn[0] != "rA" {
		t.Errorf("rB.waiting_on=%v want [rA]", waitingOn)
	}

	// --- Simulate rA completion: Release guard + MarkRunEnded. ---
	guard.Release("rA")
	if err := d.store.MarkRunEnded(storeCtx, "rA", "done", "approved"); err != nil {
		t.Fatal(err)
	}

	// --- Second re-eval: rB is now eligible. ---
	scheduler.reEvalQueuedRuns(storeCtx)

	// executePipeline runs in a goroutine; poll until rB transitions out of
	// pending (MarkRunStarted sets it to running) or 2 s elapses.
	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		rB, _ = d.store.GetRun(storeCtx, "rB")
		if rB.Status != "pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rB.Status != "running" && rB.Status != "done" && rB.Status != "needs_attention" {
		t.Errorf("rB.Status=%s want running/done/needs_attention after rA freed", rB.Status)
	}
	waitingOn, err = d.store.GetWaitingOn(storeCtx, "rB")
	if err != nil {
		t.Fatal(err)
	}
	if len(waitingOn) != 0 {
		t.Errorf("rB.waiting_on=%v want empty after dispatch", waitingOn)
	}
}

// TestDispatchPersistsPredictorMetrics verifies that dispatch writes a
// PredictorMetric row after Predict returns a non-nil Result.
func TestDispatchPersistsPredictorMetrics(t *testing.T) {
	d := newTestDaemon(t)
	d.guard = conflict.NewGuard()
	d.predictor = &fakePredictor{
		result: &predictor.Result{
			Files: []string{"a.go"},
			Metrics: predictor.Metrics{
				HaikuLatency:   1200 * time.Millisecond,
				FetchLatency:   45 * time.Millisecond,
				CandidateCount: 7,
				InlineCount:    5,
				OverflowCount:  2,
				Truncated:      false,
				Error:          "",
			},
		},
	}

	ctx := context.Background()
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "test", Status: "pending"})

	runID, err := d.scheduler.dispatch(ctx, "t1", "p", "build")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rows, err := d.store.ListPredictorMetrics(ctx, store.ListPredictorMetricsFilter{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d metric rows want 1", len(rows))
	}
	got := rows[0]
	if got.RunID != runID {
		t.Errorf("RunID=%s want %s", got.RunID, runID)
	}
	if got.HaikuLatencyMS != 1200 {
		t.Errorf("HaikuLatencyMS=%d want 1200", got.HaikuLatencyMS)
	}
	if got.CandidateCount != 7 || got.InlineCount != 5 || got.OverflowCount != 2 {
		t.Errorf("counts mismatch: %+v", got)
	}
}

// TestDispatchPersistsMetricsForBackToBackDispatches is a regression
// test for the 2b.5 smoke observation that only ~50% of runs got a
// metrics row when the scheduler tick dispatched two tasks back-to-back
// under the same scheduler.mu hold. Reproduces by dispatching two
// tasks serially against the same daemon and asserting BOTH metric
// rows exist.
func TestDispatchPersistsMetricsForBackToBackDispatches(t *testing.T) {
	d := newTestDaemon(t)
	d.guard = conflict.NewGuard()
	d.predictor = &fakePredictor{
		result: &predictor.Result{
			Files:   []string{"a.go"},
			Metrics: predictor.Metrics{HaikuLatency: 100 * time.Millisecond, CandidateCount: 1},
		},
	}

	ctx := context.Background()
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "first", Status: "pending"})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t2", ProjectID: "p", Source: "inbox", Title: "second", Status: "pending"})

	run1, err := d.scheduler.dispatch(ctx, "t1", "p", "build")
	if err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	run2, err := d.scheduler.dispatch(ctx, "t2", "p", "build")
	if err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}

	rows, err := d.store.ListPredictorMetrics(ctx, store.ListPredictorMetricsFilter{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d metric rows want 2 (run1=%s run2=%s)", len(rows), run1, run2)
	}
	gotIDs := map[string]bool{rows[0].RunID: true, rows[1].RunID: true}
	if !gotIDs[run1] || !gotIDs[run2] {
		t.Errorf("metric rows for run IDs %v don't cover both %s and %s", gotIDs, run1, run2)
	}
}

// TestDispatchPersistsPredictionInShadowMode regression-tests the
// 2b.5 shadow-mode quirk: when [conflict_guard] enabled=false but
// the predictor is on, runs.prediction must still be populated (it
// used to be NULL because PutPredictionJSON lived inside the
// conflict_guard branch).
func TestDispatchPersistsPredictionInShadowMode(t *testing.T) {
	d := newTestDaemon(t)
	d.guard = conflict.NewGuard()
	// Shadow mode: guard wired but disabled in config.
	d.cfg.Cfg.ConflictGuard.Enabled = false
	d.predictor = &fakePredictor{
		result: &predictor.Result{
			Files:   []string{"a.go"},
			Metrics: predictor.Metrics{HaikuLatency: 100 * time.Millisecond, CandidateCount: 1},
		},
	}

	ctx := context.Background()
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "shadow", Status: "pending"})

	runID, err := d.scheduler.dispatch(ctx, "t1", "p", "build")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// runs.prediction must be non-NULL even though guard is off.
	predJSON, err := d.store.GetPredictionJSON(ctx, runID)
	if err != nil {
		t.Fatalf("GetPredictionJSON: %v (expected non-NULL prediction in shadow mode)", err)
	}
	if len(predJSON) == 0 {
		t.Errorf("prediction JSON is empty; want non-empty Result")
	}

	// Metrics row should also be present (predictor enabled).
	rows, _ := d.store.ListPredictorMetrics(ctx, store.ListPredictorMetricsFilter{ProjectID: "p"})
	if len(rows) != 1 {
		t.Errorf("got %d metric rows, want 1", len(rows))
	}

	// waiting_on must remain empty (guard skipped, no queueing possible).
	waiting, _ := d.store.GetWaitingOn(ctx, runID)
	if len(waiting) != 0 {
		t.Errorf("waiting_on=%v, want empty in shadow mode", waiting)
	}
}

// newTestDaemon builds a minimal Daemon with a real store and noop adapter,
// wired for unit tests. The scheduler field is set directly so tests can
// call dispatch without starting the full daemon loop. A background context
// is wired into d.ctx so executePipeline goroutines don't panic on nil ctx.
func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	hiveDir := t.TempDir()
	cfg := config.Default()
	cfg.ConflictGuard.Enabled = true

	d, err := New(Config{
		HiveDir: hiveDir,
		Cfg:     cfg,
		Adapter: noopAdapter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	d.scheduler = NewScheduler(d)
	return d
}

// initGitRepo creates a temporary directory, initialises a git repo with a
// single commit on main, and returns the path. dispatch needs a real git repo
// because worktree.Manager.Create runs git-worktree-add.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "--allow-empty", "-m", "init")
	return dir
}

// fakePredictor returns the same canned Result every time.
type fakePredictor struct {
	result *predictor.Result
}

func (f *fakePredictor) Predict(_ context.Context, _, _, _ string) (*predictor.Result, error) {
	return f.result, nil
}

func ptr[T any](v T) *T { return &v }

// TestDispatchPersistsConfigSnapshot verifies the foundational
// tuning-data capture: dispatch writes the effective config to
// runs.config_snapshot. Asserts the snapshot exists and contains a
// recognizable field from the live config.
func TestDispatchPersistsConfigSnapshot(t *testing.T) {
	d := newTestDaemon(t)
	d.predictor = &fakePredictor{
		result: &predictor.Result{
			Files:   []string{"a.go"},
			Metrics: predictor.Metrics{HaikuLatency: 50 * time.Millisecond, CandidateCount: 1},
		},
	}

	ctx := context.Background()
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "test", Status: "pending"})

	runID, err := d.scheduler.dispatch(ctx, "t1", "p", "build")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	payload, err := d.store.GetConfigSnapshot(ctx, runID)
	if err != nil {
		t.Fatalf("GetConfigSnapshot: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("snapshot payload empty")
	}
	// The payload should contain a recognizable field from the live config.
	// Predictor is the most distinctive (has nested fields), check for it.
	if !bytes.Contains(payload, []byte(`"Predictor"`)) {
		t.Errorf("snapshot missing Predictor field; got: %s", payload)
	}
}

// errorPipeline is a minimal pipeline.Pipeline impl that always
// returns an error from Run, used to exercise executePipeline's
// error-handling branch.
type errorPipeline struct{}

func (errorPipeline) Name() string { return "build" }
func (errorPipeline) Run(_ context.Context, _ *pipeline.Run) (*pipeline.Result, error) {
	return nil, fmt.Errorf("synthetic pipeline error for test")
}

// TestExecutePipelineComputesAccuracy verifies that after a run's
// pipeline finishes (successful or otherwise), executePipeline launches
// the accuracy goroutine which persists a row.
func TestExecutePipelineComputesAccuracy(t *testing.T) {
	d := newTestDaemon(t)
	d.predictor = &fakePredictor{
		result: &predictor.Result{
			Files:   []string{"a.go", "b.go"},
			Metrics: predictor.Metrics{HaikuLatency: 100 * time.Millisecond, CandidateCount: 2},
		},
	}

	ctx := context.Background()
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "test", Status: "pending"})

	runID, err := d.scheduler.dispatch(ctx, "t1", "p", "build")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// dispatch launched executePipeline in a goroutine; the test fake
	// adapter completes quickly. Poll for the accuracy row to appear.
	deadline := time.Now().Add(5 * time.Second)
	var row *store.PredictorAccuracy
	for time.Now().Before(deadline) {
		row, err = d.store.GetPredictorAccuracy(ctx, runID)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if row == nil {
		t.Fatalf("accuracy row never appeared for run %s", runID)
	}
	// The fake worker makes no edits (noop adapter), so we expect
	// SkippedReason="no_edits".
	if row.SkippedReason != "no_edits" {
		t.Errorf("SkippedReason=%q want no_edits (fake worker makes no edits)", row.SkippedReason)
	}
}

// TestExecutePipelineComputesAccuracyOnErrorPath verifies the
// error-path branch of executePipeline also fires the accuracy
// goroutine. Forces a pipeline error by using a fake adapter
// configured to return a non-verdict error.
func TestExecutePipelineComputesAccuracyOnErrorPath(t *testing.T) {
	d := newTestDaemon(t)
	d.predictor = &fakePredictor{
		result: &predictor.Result{
			Files:   []string{"a.go"},
			Metrics: predictor.Metrics{HaikuLatency: 50 * time.Millisecond, CandidateCount: 1},
		},
	}
	// Swap pipeline for one that returns an error.
	d.pipelines["build"] = errorPipeline{}

	ctx := context.Background()
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "test", Status: "pending"})

	runID, err := d.scheduler.dispatch(ctx, "t1", "p", "build")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var row *store.PredictorAccuracy
	for time.Now().Before(deadline) {
		row, err = d.store.GetPredictorAccuracy(ctx, runID)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if row == nil {
		t.Fatalf("accuracy row never appeared for errored run %s", runID)
	}
	// errorPipeline's worker did nothing, so we expect no_edits.
	if row.SkippedReason != "no_edits" {
		t.Errorf("SkippedReason=%q want no_edits", row.SkippedReason)
	}
}

// TestDispatchHonorsPerProjectPredictorDisable verifies that a per-project
// config override (predictor.enabled=false) suppresses prediction and metrics
// persistence without blocking the run itself.
func TestDispatchHonorsPerProjectPredictorDisable(t *testing.T) {
	d := newTestDaemon(t)
	d.guard = conflict.NewGuard()
	d.predictor = &fakePredictor{result: &predictor.Result{Files: []string{"a.go"}}}

	ctx := context.Background()
	// Write a per-project config that disables the predictor for project
	// 'noisy'. The global config has predictor.enabled=true (from defaults).
	projDir := filepath.Join(d.cfg.HiveDir, "projects", "noisy")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	overrideTOML := "[predictor]\nenabled = false\n"
	if err := os.WriteFile(filepath.Join(projDir, "config.toml"), []byte(overrideTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	tmpRepo := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "noisy", Slug: "noisy", Name: "Noisy", Status: "active", RepoPath: &tmpRepo})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "noisy", Source: "inbox", Title: "test", Status: "pending"})

	runID, err := d.scheduler.dispatch(ctx, "t1", "noisy", "build")
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// With predictor disabled for this project, no metrics should be persisted.
	rows, _ := d.store.ListPredictorMetrics(ctx, store.ListPredictorMetricsFilter{ProjectID: "noisy"})
	if len(rows) != 0 {
		t.Errorf("got %d metric rows; want 0 (predictor disabled per-project)", len(rows))
	}
	// Run should still have dispatched.
	run, err := d.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" && run.Status != "done" && run.Status != "needs_attention" {
		t.Errorf("run.Status=%s; expected dispatched (running/done/needs_attention)", run.Status)
	}
}

// TestTickAutoDispatchGate verifies the Phase 3.7.2 flag: tick only
// auto-dispatches pending tasks when Scheduler.AutoDispatch is true.
func TestTickAutoDispatchGate(t *testing.T) {
	ctx := context.Background()

	// Default config: AutoDispatch is false (zero value).
	d := newTestDaemon(t)
	repoPath := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active", RepoPath: &repoPath})
	_ = d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p", Source: "inbox", Title: "test", Status: "pending"})

	// Tick with auto-dispatch off → task stays pending, no run created.
	d.scheduler.tick(ctx)
	running, _ := d.store.ListRunningRuns(ctx)
	if len(running) != 0 {
		t.Errorf("auto-dispatch off: expected 0 running runs, got %d", len(running))
	}

	// Enable auto-dispatch + tick again → task dispatched.
	d.cfg.Cfg.Scheduler.AutoDispatch = true
	d.scheduler.tick(ctx)
	// Poll briefly — dispatch launches a goroutine; the run row is
	// created synchronously in dispatch before the goroutine starts.
	pending, _ := d.store.ListPendingTasks(ctx)
	if len(pending) != 0 {
		t.Errorf("auto-dispatch on: expected task claimed (0 pending), got %d", len(pending))
	}
}

// resumeTestSeed inserts a project + task + run row matching the
// pattern needed for Scheduler.Resume tests. Returns the runID for
// convenience. Worktree directory at <hiveDir>/worktrees/<runID> is
// created on disk so the os.Stat eligibility check passes; tests
// that want the missing-worktree path call rmWorktree(d, runID).
func resumeTestSeed(t *testing.T, d *Daemon, runID, status string, parentRunID string) string {
	t.Helper()
	ctx := context.Background()
	repoPath := t.TempDir()
	// Init a git repo at repoPath so executePipeline's worktree-using
	// path doesn't immediately blow up when the goroutine kicks off.
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repoPath
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P", RepoPath: &repoPath}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Body: "x", Status: status}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: runID, TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: status, ParentRunID: parentRunID}); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(d.HiveDir(), "worktrees", runID)
	if err := os.MkdirAll(wt, 0700); err != nil {
		t.Fatal(err)
	}
	return runID
}

func rmWorktree(t *testing.T, d *Daemon, runID string) {
	t.Helper()
	wt := filepath.Join(d.HiveDir(), "worktrees", runID)
	_ = os.RemoveAll(wt)
}

func TestResumeRejectsUnknownRun(t *testing.T) {
	d := newTestDaemon(t)
	err := d.scheduler.Resume(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for unknown run")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("err=%q, want 'run not found' substring", err)
	}
}

func TestResumeRejectsPendingRun(t *testing.T) {
	d := newTestDaemon(t)
	resumeTestSeed(t, d, "r1", "pending", "")
	err := d.scheduler.Resume(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "not resumable") {
		t.Errorf("err=%v, want 'not resumable' substring", err)
	}
}

func TestResumeRejectsRunningRun(t *testing.T) {
	d := newTestDaemon(t)
	resumeTestSeed(t, d, "r1", "running", "")
	err := d.scheduler.Resume(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "not resumable") {
		t.Errorf("err=%v, want 'not resumable' substring", err)
	}
}

func TestResumeRejectsDoneRun(t *testing.T) {
	d := newTestDaemon(t)
	resumeTestSeed(t, d, "r1", "done", "")
	err := d.scheduler.Resume(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "not resumable") {
		t.Errorf("err=%v, want 'not resumable' substring", err)
	}
}

func TestResumeRejectsChildRun(t *testing.T) {
	d := newTestDaemon(t)
	resumeTestSeed(t, d, "r1", "needs_attention", "parent-run-id")
	err := d.scheduler.Resume(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "child run") {
		t.Errorf("err=%v, want 'child run' substring", err)
	}
}

func TestResumeRejectsMissingWorktree(t *testing.T) {
	d := newTestDaemon(t)
	resumeTestSeed(t, d, "r1", "needs_attention", "")
	rmWorktree(t, d, "r1")
	err := d.scheduler.Resume(context.Background(), "r1")
	if err == nil || !strings.Contains(err.Error(), "worktree missing") {
		t.Errorf("err=%v, want 'worktree missing' substring", err)
	}
}

func TestResumeAcceptsNeedsAttentionAndMarksRunning(t *testing.T) {
	d := newTestDaemon(t)
	resumeTestSeed(t, d, "r1", "needs_attention", "")

	// Fix 2: Ensure all goTracked goroutines complete before t.TempDir
	// cleanup removes the store DB out from under them.
	t.Cleanup(func() {
		d.wg.Wait()
	})

	// Subscribe to events BEFORE Resume so we can observe the
	// run.started publish that markRunStartedAndPublish does
	// synchronously (before spawning the goroutine).
	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	if err := d.scheduler.Resume(context.Background(), "r1"); err != nil {
		t.Fatalf("Resume returned err: %v", err)
	}

	// Drain events looking for run.started for r1 within a short
	// window. The publish is synchronous in markRunStartedAndPublish
	// so it should already be in the channel by the time Resume
	// returns. 1s timeout is generous.
	deadline := time.After(1 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("did not observe run.started event for r1 within 1s")
		case evt, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed")
			}
			if evt.Type != rpc.EventRunStarted {
				continue
			}
			runID, _ := evt.Data["run_id"].(string)
			if runID == "r1" {
				return // success
			}
		}
	}
}

// TestSchedulerLastTickAtUpdatedOnTick verifies tick() stamps lastTickAt at
// entry, so doctor.health's "scheduler wedged?" check has a fresh signal.
// Drives tick() directly via the unexported method (in-package test) without
// running Loop, so the assertion is deterministic.
func TestSchedulerLastTickAtUpdatedOnTick(t *testing.T) {
	d := newTestDaemon(t)
	s := NewScheduler(d)
	before := s.LastTickAt()
	s.tick(context.Background())
	after := s.LastTickAt()
	if after.IsZero() {
		t.Errorf("LastTickAt zero after tick; expected set")
	}
	if !before.IsZero() && !after.After(before) {
		t.Errorf("LastTickAt before=%v not before after=%v", before, after)
	}
}

// TestSchedulerLastTickAtStampsAtEntry is a regression test for the
// INVARIANT that lastTickAt is stamped at tick() ENTRY, not exit.
// Doctor's wedge-detection logic uses LastTickAt() to flag a hung tick
// body; if a future refactor moved the stamp below s.mu's locked region
// (the long-running portion of tick()), then during a wedge LastTickAt()
// would return the PREVIOUS tick's timestamp — exactly when freshness
// matters most.
//
// Approach: hold s.mu from outside, then call tick() in a goroutine.
// tick()'s structure is:
//
//	(stamp at entry)  ← what we're testing
//	s.mu.Lock()       ← blocks here while we hold the mutex
//	... locked body ...
//	(no stamp here)
//
// If the stamp is at entry, LastTickAt() goes non-zero immediately even
// though tick() is blocked on s.mu. If a regression moved the stamp into
// or after the locked body, LastTickAt() would stay zero until we release
// s.mu, which is the failure mode this test catches.
func TestSchedulerLastTickAtStampsAtEntry(t *testing.T) {
	d := newTestDaemon(t)
	s := NewScheduler(d)

	// Block the locked region of tick() by holding s.mu ourselves.
	s.mu.Lock()

	tickReturned := make(chan struct{})
	go func() {
		s.tick(context.Background())
		close(tickReturned)
	}()

	// Poll for LastTickAt to become non-zero. With the stamp at entry,
	// this happens within microseconds — way before the test's 1s budget.
	// If the stamp were below s.mu.Lock(), it would never fire here
	// because the goroutine is parked waiting for the mutex.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if !s.LastTickAt().IsZero() {
			break
		}
		select {
		case <-tickReturned:
			// Shouldn't happen — we're holding s.mu, so tick can't return.
			s.mu.Unlock()
			t.Fatal("tick returned while s.mu was held; test setup invariant broken")
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}

	if s.LastTickAt().IsZero() {
		s.mu.Unlock() // release before failing so the goroutine doesn't leak
		<-tickReturned
		t.Fatal("LastTickAt() still zero while tick() is blocked on s.mu; " +
			"stamp must be at tick() ENTRY (before s.mu.Lock), not in or after the locked body")
	}

	// Release the mutex so tick can proceed and the goroutine exits.
	s.mu.Unlock()
	select {
	case <-tickReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not return within 2s after releasing s.mu")
	}
}

// TestDependenciesSatisfied verifies the dependency gate: a task with a
// depends_on entry is held until that dep's gate reads satisfied; a task
// with no deps is always ready.
func TestDependenciesSatisfied(t *testing.T) {
	d := newTestDaemon(t)
	s := NewScheduler(d)
	ctx := context.Background()

	if err := d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "p", Name: "P", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// Dependency task, inserted with a non-satisfied gate.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "dep1", ProjectID: "p", Source: "inbox", Title: "dep", Status: "pending",
		GateState: sequence.GateAwaitingMerge,
	}); err != nil {
		t.Fatal(err)
	}

	// Dependent task carries depends_on as it is actually stored: a comma-joined
	// string of task IDs (survives MergeTaskMetadata flattening).
	dependent := &store.Task{
		ID: "t1", ProjectID: "p", Source: "inbox", Title: "dependent", Status: "pending",
		Metadata: map[string]any{"depends_on": "dep1"},
	}

	// Dep not yet satisfied → held.
	if s.dependenciesSatisfied(ctx, dependent) {
		t.Errorf("expected NOT satisfied while dep1 gate=%q", sequence.GateAwaitingMerge)
	}

	// Flip the dep to satisfied → ready.
	if err := d.store.UpdateTaskGateState(ctx, "dep1", sequence.GateSatisfied); err != nil {
		t.Fatal(err)
	}
	if !s.dependenciesSatisfied(ctx, dependent) {
		t.Errorf("expected satisfied after dep1 gate flipped to satisfied")
	}

	// A task with no depends_on is always ready.
	noDeps := &store.Task{ID: "t2", ProjectID: "p", Source: "inbox", Title: "free", Status: "pending"}
	if !s.dependenciesSatisfied(ctx, noDeps) {
		t.Errorf("task with no depends_on should be ready")
	}

	// Fails closed when a dep can't be read (missing task → GetTask error).
	missing := &store.Task{
		ID: "t3", ProjectID: "p", Source: "inbox", Title: "broken", Status: "pending",
		Metadata: map[string]any{"depends_on": []any{"ghost"}},
	}
	if s.dependenciesSatisfied(ctx, missing) {
		t.Errorf("expected NOT satisfied (fail-closed) when a dep can't be read")
	}

	// A dep that finished as terminal "done" with gate STILL GateNone (an
	// audit/plan task that opened no PR, so the merge gate never advanced) must
	// satisfy its dependents — otherwise the whole phase stalls behind it.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "audit", ProjectID: "p", Source: "inbox", Title: "audit", Status: "done",
		GateState: sequence.GateNone,
	}); err != nil {
		t.Fatal(err)
	}
	afterAudit := &store.Task{
		ID: "t4", ProjectID: "p", Source: "inbox", Title: "needs audit", Status: "pending",
		Metadata: map[string]any{"depends_on": "audit"},
	}
	if !s.dependenciesSatisfied(ctx, afterAudit) {
		t.Errorf("a done dep with GateNone (no-PR audit task) should satisfy dependents")
	}
}

// insertRecoveryTestRun seeds a project + task + pending run with the given
// runID so the orphan-worker sweep tests can stamp worker_pid and observe
// the kill + clear. The repoPath is a throwaway tempdir — no git init
// needed because the sweep never calls into the pipeline/worktree code.
func insertRecoveryTestRun(t *testing.T, d *Daemon, runID string) {
	t.Helper()
	ctx := context.Background()
	repoPath := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p-recov", Slug: "p-recov", Name: "P", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		// Project may already exist from a sibling test in the same daemon —
		// recovery tests use fresh daemons via newTestDaemon, so this is
		// defensive only.
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "task-" + runID, ProjectID: "p-recov", Source: "inbox", Title: "recov", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: runID, TaskID: "task-" + runID, ProjectID: "p-recov", Pipeline: "build", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRecoverOrphanedWorkersKillsAlivePID forks a real sleep(60) child as
// a process-group leader (matching how subprocess.go spawns workers),
// writes its PID to runs.worker_pid, runs the sweep, and asserts the
// child is dead + the column is cleared.
func TestRecoverOrphanedWorkersKillsAlivePID(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	d := newTestDaemon(t)
	s := NewScheduler(d)

	insertRecoveryTestRun(t, d, "r-alive")
	if err := d.store.SetRunWorkerPID(context.Background(), "r-alive", pid); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}

	s.recoverOrphanedWorkers(context.Background())

	// Reap the (now-killed) child so it doesn't sit as a zombie.
	_ = cmd.Wait()

	// kill(pid, 0) should ESRCH once the process is gone.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("pid %d still alive after sweep", pid)
	}

	rows, err := d.store.ListRunsWithWorkerPID(context.Background())
	if err != nil {
		t.Fatalf("ListRunsWithWorkerPID: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after sweep, got %d", len(rows))
	}
}

// TestRecoverOrphanedWorkersHandlesDeadPID forks+reaps a `true` so the
// PID is guaranteed dead, then runs the sweep against that dead PID.
// The sweep must not crash and must clear the column.
func TestRecoverOrphanedWorkersHandlesDeadPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	deadPID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait true: %v", err)
	}

	d := newTestDaemon(t)
	s := NewScheduler(d)

	insertRecoveryTestRun(t, d, "r-dead")
	if err := d.store.SetRunWorkerPID(context.Background(), "r-dead", deadPID); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}

	s.recoverOrphanedWorkers(context.Background()) // must not panic

	rows, err := d.store.ListRunsWithWorkerPID(context.Background())
	if err != nil {
		t.Fatalf("ListRunsWithWorkerPID: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after sweep, got %d", len(rows))
	}
}

// TestRecoverOrphanedWorkersPublishesEvent subscribes to the event bus
// BEFORE running the sweep, then asserts a worker.orphan_killed frame
// fires with run_id + pid + was_alive.
func TestRecoverOrphanedWorkersPublishesEvent(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	d := newTestDaemon(t)
	s := NewScheduler(d)

	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	insertRecoveryTestRun(t, d, "r-event")
	if err := d.store.SetRunWorkerPID(context.Background(), "r-event", pid); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}

	s.recoverOrphanedWorkers(context.Background())
	_ = cmd.Wait()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no worker.orphan_killed event published within 2s")
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed")
			}
			if ev.Type != rpc.EventWorkerOrphanKilled {
				// Tolerate other events (e.g. heartbeats) and keep draining.
				continue
			}
			gotRunID, _ := ev.Data["run_id"].(string)
			if gotRunID != "r-event" {
				t.Errorf("event run_id=%q, want r-event", gotRunID)
			}
			// Event Data goes through the bus as map[string]any without
			// JSON marshaling, so pid stays as int.
			gotPID, _ := ev.Data["pid"].(int)
			if gotPID != pid {
				t.Errorf("event pid=%v (type %T), want %d", ev.Data["pid"], ev.Data["pid"], pid)
			}
			wasAlive, _ := ev.Data["was_alive"].(bool)
			if !wasAlive {
				t.Errorf("event was_alive=%v, want true (sleep child was alive)", ev.Data["was_alive"])
			}
			return
		}
	}
}

// TestKillProcessGroupAliveReturnsTrueNil exercises the happy path of
// the new (bool, error) signature: a live process-group leader gets
// killed cleanly, returns (true, nil).
func TestKillProcessGroupAliveReturnsTrueNil(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	wasAlive, err := killProcessGroup(pid)
	if err != nil {
		t.Fatalf("killProcessGroup err=%v, want nil", err)
	}
	if !wasAlive {
		t.Errorf("wasAlive=false, want true (sleep child was alive)")
	}
	_ = cmd.Wait()
}

// TestKillProcessGroupDeadReturnsFalseNil exercises the ESRCH path:
// killing an already-reaped PID returns (false, nil), treated as
// success because the orphan is already gone.
func TestKillProcessGroupDeadReturnsFalseNil(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start true: %v", err)
	}
	deadPID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait true: %v", err)
	}

	wasAlive, err := killProcessGroup(deadPID)
	if err != nil {
		t.Errorf("killProcessGroup err=%v, want nil for already-dead pid", err)
	}
	if wasAlive {
		t.Errorf("wasAlive=true, want false for already-dead pid")
	}
}

// TestKillProcessGroupEPERM_NotUnitTestable documents WHY there is no
// direct unit test for the EPERM path. Reproducing EPERM from syscall.Kill
// requires a PID owned by a different uid (or a different user namespace),
// which in turn requires either:
//  1. Running the test as root and forking a setuid child (test runners
//     aren't root, and this would taint the host).
//  2. Spawning a child in a user namespace with a distinct uid map (Linux-
//     only, requires CAP_SYS_ADMIN or unprivileged userns support, plus
//     non-trivial namespace plumbing that adds more risk than the test
//     removes).
//  3. Sending a signal to a system process like pid=1 (Linux refuses for
//     kernel processes but returns EPERM for init on most distros — fragile
//     across CI environments and ethically dubious to depend on).
//
// The EPERM-handling branches in recoverOrphanedWorkers and the synthesis
// in killProcessGroup are short, linear, and reviewed by inspection. The
// signature change (now returns an explicit error) is what surfaces the
// bug class — callers can no longer accidentally treat EPERM as a benign
// "already dead." The two happy-path tests above pin the (bool, error)
// shape so a future refactor can't quietly regress the contract.
func TestKillProcessGroupEPERM_NotUnitTestable(t *testing.T) {
	t.Skip("EPERM requires a cross-uid PID; documented in this test's comment, not fixturable in unit tests")
}

// TestRecoverOrphanedWorkersRunsAtLoopStartup is the Phase 7 Task 5
// integration test: forks a real sleep(60) child as a Setpgid leader,
// stamps its PID into a running run's worker_pid column, then calls
// Scheduler.Loop with a timeout-canceled context. After Loop returns,
// asserts both (a) the worker is dead — proves recoverOrphanedWorkers
// ran and read a non-NULL worker_pid — and (b) the run is flipped to
// needs_attention — proves recoverStaleRuns also ran. Implicitly pins
// the ordering: if recoverStaleRuns ran first, it would clear/leave
// status alone but the worker_pid is independent so the kill would
// still happen; the real ordering invariant lives in the comment on
// recoverOrphanedWorkers. What this test actually pins is that BOTH
// run at Loop startup, which is the user-visible contract.
func TestRecoverOrphanedWorkersRunsAtLoopStartup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Cleanup signals the pgroup as a backstop in case recovery didn't
	// kill it. The Wait happens in a goroutine below; we deliberately do
	// not call cmd.Wait() here to avoid concurrent-Wait when the test
	// times out and the in-flight goroutine is still blocked on Wait.
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()

	d := newTestDaemon(t)
	s := NewScheduler(d)

	// insertRecoveryTestRun inserts a run with status="running" (and a
	// task in "running") so recoverStaleRuns will flip it to
	// needs_attention.
	insertRecoveryTestRun(t, d, "r-loop")
	if err := d.store.SetRunWorkerPID(context.Background(), "r-loop", pid); err != nil {
		t.Fatalf("SetRunWorkerPID: %v", err)
	}

	// Loop runs forever; cancel after 500ms so the startup block has
	// time to execute but the periodic tick doesn't drag the test out.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s.Loop(ctx) // returns when ctx is canceled

	// Bound cmd.Wait so a regression that disabled recoverOrphanedWorkers
	// can't quietly extend the test to 60s and still pass.
	//
	// Pre-fix, the test asserted (kill -0 != nil) AFTER cmd.Wait. If
	// recovery didn't run, cmd.Wait blocked the full sleep(60) duration;
	// the child then exited naturally; kill -0 returned ESRCH; the
	// assertion was vacuously satisfied. 60s test that "passes."
	//
	// We can't move kill -0 BEFORE cmd.Wait either: SIGKILL on a child
	// whose parent hasn't reaped it leaves a zombie, and kill -0 returns
	// nil on zombies (the PID is still in the process table). So
	// "pre-Wait kill -0" doesn't prove the kill landed.
	//
	// Instead: race cmd.Wait against a 2s timer. If Wait returns fast,
	// recovery killed it. If the timer fires, recovery didn't run.
	// Either way the test completes in ≤2s.
	waitDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		// Reaped fast — confirm it's gone for the audit trail.
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("pid %d still alive after cmd.Wait returned — unexpected", pid)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("cmd.Wait did not return within 2s — recoverOrphanedWorkers likely didn't kill pid %d (regressed?)", pid)
		// Don't block the test; the defer'd Kill(-pid, SIGKILL) at the
		// top will signal the lingering child, and the in-flight Wait
		// goroutine then reaps it after the test function returns. The
		// goroutine is leaked relative to this test but exits cleanly
		// once the SIGKILL lands.
	}

	got, err := d.store.GetRun(context.Background(), "r-loop")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != "needs_attention" {
		t.Errorf("run status=%q, want needs_attention", got.Status)
	}
}

// TestRecoverStaleRunsChildAndParentBothNeedsAttention pins Phase 4.3.1 sub-item
// #1: when a parent finish-branch run + a child fix run share the same
// task_id, recoverStaleRuns must mark BOTH rows needs_attention + emit a
// run.ended event for BOTH (per-run state is correct), and the task
// must end up needs_attention. Previously this was enforced by
// skipTaskUpdate=true on the child's endRun; now endRun calls
// refreshTaskStatus for every run — DeriveTaskStatus inspects all of
// a task's runs so the final derived state is correct regardless of
// which endRun call "wins".
func TestRecoverStaleRunsChildAndParentBothNeedsAttention(t *testing.T) {
	d := newTestDaemon(t)
	s := NewScheduler(d)
	ctx := context.Background()

	// Seed: project + task + parent (root) run + child run (shares task_id, parent_run_id set).
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p-rec", Slug: "p-rec", Name: "Rec"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "task-shared", ProjectID: "p-rec", Title: "shared", Body: "x",
		Priority: "P1", Status: "running", Pipeline: "finish-branch",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: "r-parent", TaskID: "task-shared", ProjectID: "p-rec",
		Pipeline: "finish-branch", Status: "running",
	}); err != nil {
		t.Fatalf("InsertRun parent: %v", err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: "r-child", TaskID: "task-shared", ProjectID: "p-rec",
		Pipeline: "build", Status: "running", ParentRunID: "r-parent",
	}); err != nil {
		t.Fatalf("InsertRun child: %v", err)
	}

	// Subscribe to events so we can count run.ended.
	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	s.recoverStaleRuns(ctx)

	// Both runs marked needs_attention.
	parentRun, _ := d.store.GetRun(ctx, "r-parent")
	if parentRun.Status != "needs_attention" {
		t.Errorf("parent.Status=%q, want needs_attention", parentRun.Status)
	}
	childRun, _ := d.store.GetRun(ctx, "r-child")
	if childRun.Status != "needs_attention" {
		t.Errorf("child.Status=%q, want needs_attention", childRun.Status)
	}

	// Task.status updated to needs_attention. We can't easily count
	// UpdateTaskStatus calls without instrumenting the store, but we
	// can assert the post-state is correct.
	task, _ := d.store.GetTask(ctx, "task-shared")
	if task.Status != "needs_attention" {
		t.Errorf("task.Status=%q, want needs_attention", task.Status)
	}

	// Drain run.ended events with a 1s deadline. Expect exactly 2 (one per
	// run; per-run events are correct). Filter by Type to ignore any
	// unrelated traffic on the bus. Mirrors the drain pattern in
	// TestRecoverOrphanedWorkersPublishesEvent above.
	deadline := time.After(1 * time.Second)
	endedCount := 0
collectLoop:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break collectLoop
			}
			if ev.Type != rpc.EventRunEnded {
				continue
			}
			endedCount++
			if endedCount >= 2 {
				// Drain a small grace window for any extras then stop.
				graceDeadline := time.After(50 * time.Millisecond)
			grace:
				for {
					select {
					case ev2, ok2 := <-ch:
						if !ok2 {
							break grace
						}
						if ev2.Type == rpc.EventRunEnded {
							endedCount++
						}
					case <-graceDeadline:
						break grace
					}
				}
				break collectLoop
			}
		case <-deadline:
			break collectLoop
		}
	}
	if endedCount != 2 {
		t.Errorf("run.ended event count=%d, want 2 (one per run)", endedCount)
	}
}

// TestRecoverStaleRunsRootStillUpdatesTask pins that a stale ROOT run
// (no parent_run_id) drives the task's status update via refreshTaskStatus.
// Confirms the common case: a single stale root run → task becomes needs_attention.
func TestRecoverStaleRunsRootStillUpdatesTask(t *testing.T) {
	d := newTestDaemon(t)
	s := NewScheduler(d)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p-root", Slug: "p-root", Name: "Root"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "task-root", ProjectID: "p-root", Title: "root", Body: "x",
		Priority: "P1", Status: "running", Pipeline: "build",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: "r-root", TaskID: "task-root", ProjectID: "p-root",
		Pipeline: "build", Status: "running",
	}); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	s.recoverStaleRuns(ctx)

	run, _ := d.store.GetRun(ctx, "r-root")
	if run.Status != "needs_attention" {
		t.Errorf("run.Status=%q, want needs_attention", run.Status)
	}
	task, _ := d.store.GetTask(ctx, "task-root")
	if task.Status != "needs_attention" {
		t.Errorf("task.Status=%q, want needs_attention", task.Status)
	}
}

// TestSchedulerCapacityCountsRootsOnly is Phase 4.3.1 #3 contract:
// capacity() must count only root runs (subprocess-worker holders);
// a finish-branch parent with an in-flight child fix occupies ONE
// slot, not two.
func TestSchedulerCapacityCountsRootsOnly(t *testing.T) {
	d := newTestDaemon(t)
	s := NewScheduler(d)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p-cap", Slug: "p-cap", Name: "Cap"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t-cap", ProjectID: "p-cap", Title: "t", Body: "x", Priority: "P1", Status: "running", Pipeline: "finish-branch"}); err != nil {
		t.Fatal(err)
	}
	// Compute the delta from the initial (empty) capacity to avoid
	// depending on the MaxWorkers default.
	initialCap, err := s.capacity(ctx)
	if err != nil {
		t.Fatalf("initial capacity: %v", err)
	}

	// Insert 1 root run + 1 child run sharing the same task.
	if err := d.store.InsertRun(ctx, &store.Run{ID: "r-cap-parent", TaskID: "t-cap", ProjectID: "p-cap", Pipeline: "finish-branch", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "r-cap-child", TaskID: "t-cap", ProjectID: "p-cap", Pipeline: "build", Status: "running", ParentRunID: "r-cap-parent"}); err != nil {
		t.Fatal(err)
	}

	afterCap, err := s.capacity(ctx)
	if err != nil {
		t.Fatalf("after capacity: %v", err)
	}
	if delta := initialCap - afterCap; delta != 1 {
		t.Errorf("capacity delta = %d, want 1 (only the root counts)", delta)
	}
}

// setupSchedulerWith2Projects builds a test daemon with 2 active
// projects (slug "proj-a", "proj-b") backed by real git repos, plus 1
// pending task per project ("t-a", "t-b"). Returns the daemon for tests
// to mutate config and tick.
func setupSchedulerWith2Projects(t *testing.T) *Daemon {
	t.Helper()
	ctx := context.Background()
	d := newTestDaemon(t)
	repoA := initGitRepo(t)
	repoB := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: "proj-a", Slug: "proj-a", Name: "A", Status: "active", RepoPath: &repoA}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertProject(ctx, &store.Project{ID: "proj-b", Slug: "proj-b", Name: "B", Status: "active", RepoPath: &repoB}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t-a", ProjectID: "proj-a", Source: "inbox", Title: "task a", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t-b", ProjectID: "proj-b", Source: "inbox", Title: "task b", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	return d
}

// writePerProjectConfig writes a TOML body to
// <HiveDir>/projects/<slug>/config.toml. Creates the parent dirs as
// needed (matches what `hive project add` would create).
func writePerProjectConfig(t *testing.T, hiveDir, slug, body string) {
	t.Helper()
	dir := filepath.Join(hiveDir, "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pendingTaskIDs returns the IDs of currently-pending tasks, for
// asserting that some tasks queued (didn't dispatch).
func pendingTaskIDs(t *testing.T, d *Daemon) []string {
	t.Helper()
	pending, err := d.store.ListPendingTasks(context.Background())
	if err != nil {
		t.Fatalf("ListPendingTasks: %v", err)
	}
	ids := make([]string, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.ID)
	}
	return ids
}

// runningProjectIDs returns the project IDs of currently-running runs.
func runningProjectIDs(t *testing.T, d *Daemon) []string {
	t.Helper()
	runs, err := d.store.ListRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ProjectID)
	}
	return ids
}

// TestSchedulerTickPerProjectAutoDispatchOverrideOn pins: with the
// global setting off, a project that opts in via its per-project
// config.toml still gets its tasks dispatched while other projects
// (which inherit the global off) keep their tasks queued.
func TestSchedulerTickPerProjectAutoDispatchOverrideOn(t *testing.T) {
	ctx := context.Background()
	d := setupSchedulerWith2Projects(t)
	d.cfg.Cfg.Scheduler.AutoDispatch = false // global off

	writePerProjectConfig(t, d.HiveDir(), "proj-a", "[scheduler]\nauto_dispatch = true\n")

	d.scheduler.tick(ctx)

	running := runningProjectIDs(t, d)
	if len(running) != 1 || running[0] != "proj-a" {
		t.Errorf("expected exactly 1 running run for proj-a, got %v", running)
	}
	pending := pendingTaskIDs(t, d)
	if len(pending) != 1 || pending[0] != "t-b" {
		t.Errorf("expected t-b to stay pending, got pending=%v", pending)
	}
}

// TestSchedulerTickPerProjectAutoDispatchOverrideOff pins: with the
// global setting on, a project that opts OUT via its per-project
// config.toml has its tasks queue while other projects (inheriting
// global on) dispatch.
func TestSchedulerTickPerProjectAutoDispatchOverrideOff(t *testing.T) {
	ctx := context.Background()
	d := setupSchedulerWith2Projects(t)
	d.cfg.Cfg.Scheduler.AutoDispatch = true // global on

	writePerProjectConfig(t, d.HiveDir(), "proj-b", "[scheduler]\nauto_dispatch = false\n")

	d.scheduler.tick(ctx)

	running := runningProjectIDs(t, d)
	if len(running) != 1 || running[0] != "proj-a" {
		t.Errorf("expected exactly 1 running run for proj-a, got %v", running)
	}
	pending := pendingTaskIDs(t, d)
	if len(pending) != 1 || pending[0] != "t-b" {
		t.Errorf("expected t-b to stay pending (opted out), got pending=%v", pending)
	}
}

// TestSchedulerTickNoPerProjectConfigUsesGlobal pins: with no
// per-project files on disk, all tasks inherit the global setting.
// global on → both dispatch; global off → both queue.
func TestSchedulerTickNoPerProjectConfigUsesGlobal(t *testing.T) {
	ctx := context.Background()

	// Sub-case 1: global on → both projects' tasks dispatch.
	d := setupSchedulerWith2Projects(t)
	d.cfg.Cfg.Scheduler.AutoDispatch = true
	d.scheduler.tick(ctx)
	if pending := pendingTaskIDs(t, d); len(pending) != 0 {
		t.Errorf("global on, no per-project: expected 0 pending, got %v", pending)
	}

	// Sub-case 2: fresh daemon, global off → both tasks queue.
	d2 := setupSchedulerWith2Projects(t)
	d2.cfg.Cfg.Scheduler.AutoDispatch = false
	d2.scheduler.tick(ctx)
	pending2 := pendingTaskIDs(t, d2)
	if len(pending2) != 2 {
		t.Errorf("global off, no per-project: expected 2 pending, got %v", pending2)
	}
	running2 := runningProjectIDs(t, d2)
	if len(running2) != 0 {
		t.Errorf("global off, no per-project: expected 0 running, got %v", running2)
	}
}

func TestEffectiveDispatchModeForProject(t *testing.T) {
	d := newTestDaemon(t)
	d.cfg.Cfg.Scheduler.AutoDispatch = false // global baseline -> manual

	// No per-project file: inherits global (manual).
	if got := d.scheduler.effectiveDispatchModeForProject("proj-none"); got != config.DispatchModeManual {
		t.Errorf("no file: got %q, want manual", got)
	}

	// Explicit dispatch_mode wins.
	writePerProjectConfig(t, d.HiveDir(), "proj-seq", "[scheduler]\ndispatch_mode = \"sequenced\"\n")
	if got := d.scheduler.effectiveDispatchModeForProject("proj-seq"); got != config.DispatchModeSequenced {
		t.Errorf("explicit: got %q, want sequenced", got)
	}

	// Legacy auto_dispatch=true overlays to auto_all.
	writePerProjectConfig(t, d.HiveDir(), "proj-legacy", "[scheduler]\nauto_dispatch = true\n")
	if got := d.scheduler.effectiveDispatchModeForProject("proj-legacy"); got != config.DispatchModeAutoAll {
		t.Errorf("legacy: got %q, want auto_all", got)
	}

	// Malformed TOML fails closed to manual.
	writePerProjectConfig(t, d.HiveDir(), "proj-bad", "[scheduler\nauto_dispatch = true\n")
	if got := d.scheduler.effectiveDispatchModeForProject("proj-bad"); got != config.DispatchModeManual {
		t.Errorf("malformed: got %q, want manual (fail closed)", got)
	}

	// Unrecognized dispatch_mode value fails closed to manual (overlay path
	// skips config.Validate, so a per-project typo must not silently misroute).
	writePerProjectConfig(t, d.HiveDir(), "proj-typo", "[scheduler]\ndispatch_mode = \"bogus\"\n")
	if got := d.scheduler.effectiveDispatchModeForProject("proj-typo"); got != config.DispatchModeManual {
		t.Errorf("bogus mode: got %q, want manual (fail closed on unknown dispatch_mode)", got)
	}
}

func TestEffectiveTargetBranchForProject(t *testing.T) {
	d := newTestDaemon(t)

	// No file -> default main.
	if got := d.scheduler.effectiveTargetBranchForProject("proj-none"); got != "main" {
		t.Errorf("default: got %q, want main", got)
	}

	// Per-project override.
	writePerProjectConfig(t, d.HiveDir(), "proj-stg", "[scheduler]\ntarget_branch = \"staging\"\n")
	if got := d.scheduler.effectiveTargetBranchForProject("proj-stg"); got != "staging" {
		t.Errorf("override: got %q, want staging", got)
	}

	// Malformed fails to main.
	writePerProjectConfig(t, d.HiveDir(), "proj-bad", "[scheduler\ntarget_branch = \"x\"\n")
	if got := d.scheduler.effectiveTargetBranchForProject("proj-bad"); got != "main" {
		t.Errorf("malformed: got %q, want main", got)
	}
}

// TestActivePhaseForSequencedProject exercises the four key branches of
// activePhaseForSequencedProject: no dispatcher row → "", active dispatcher
// → returns the active roadmap phase, all phase-1 tasks gated satisfied →
// advances to phase 2, paused dispatcher → "".
func TestActivePhaseForSequencedProject(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	repo := t.TempDir()
	rp := repo
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "seqp", Name: "S", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, "docs", "superpowers", "roadmaps")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "seqp.md"), []byte("## Phase 1: One\n\n## Phase 2: Two\n"), 0o644)
	mk := func(id, phase string) {
		if err := d.store.InsertTask(ctx, &store.Task{
			ID: id, ProjectID: "p1", Source: "inbox", Title: id, Status: "pending",
			Metadata: map[string]any{"roadmap_phase": phase},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "1")
	mk("b", "1")
	mk("c", "2")

	if got := d.scheduler.activePhaseForSequencedProject(ctx, "p1"); got != "" {
		t.Errorf("no dispatcher: got %q, want \"\"", got)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened"}); err != nil {
		t.Fatal(err)
	}
	if got := d.scheduler.activePhaseForSequencedProject(ctx, "p1"); got != "1" {
		t.Errorf("active: got %q, want 1", got)
	}
	if err := d.store.UpdateTaskGateState(ctx, "a", "satisfied"); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpdateTaskGateState(ctx, "b", "satisfied"); err != nil {
		t.Fatal(err)
	}
	if got := d.scheduler.activePhaseForSequencedProject(ctx, "p1"); got != "2" {
		t.Errorf("advanced: got %q, want 2", got)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "p1", Status: "paused", AdvancementPolicy: "pr_opened"}); err != nil {
		t.Fatal(err)
	}
	if got := d.scheduler.activePhaseForSequencedProject(ctx, "p1"); got != "" {
		t.Errorf("paused: got %q, want \"\"", got)
	}
}

// TestSchedulerTickPerProjectConfigPathErrorFailsClosed pins the
// fail-closed semantics: malformed per-project TOML must NOT dispatch,
// even when the global setting is on. Better to queue than to silently
// dispatch when operator intent is unclear.
func TestSchedulerTickPerProjectConfigPathErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	d := setupSchedulerWith2Projects(t)
	d.cfg.Cfg.Scheduler.AutoDispatch = true // global on; only the helper's fail-closed should block proj-a

	// Garbage TOML — unterminated table header, parse error guaranteed.
	writePerProjectConfig(t, d.HiveDir(), "proj-a", "[scheduler\nauto_dispatch = true\n")

	d.scheduler.tick(ctx)

	// proj-a's task must stay pending (fail-closed). proj-b inherits
	// global=on so its task should dispatch as a sanity check that the
	// rest of the loop still runs.
	pending := pendingTaskIDs(t, d)
	if len(pending) != 1 || pending[0] != "t-a" {
		t.Errorf("expected t-a to stay pending (fail-closed), got pending=%v", pending)
	}
	running := runningProjectIDs(t, d)
	if len(running) != 1 || running[0] != "proj-b" {
		t.Errorf("expected proj-b to still dispatch, got running=%v", running)
	}
}
