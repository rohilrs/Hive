package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
)

// recordingBuild is a fake build pipeline that captures the *pipeline.Run it
// receives and returns a fixed Result, so the test can assert what the
// childRunner constructed for the child Build pass.
type recordingBuild struct {
	gotRun *pipeline.Run
	result *pipeline.Result
}

func (r *recordingBuild) Name() string { return "build" }

func (r *recordingBuild) Run(ctx context.Context, run *pipeline.Run) (*pipeline.Result, error) {
	r.gotRun = run
	return r.result, nil
}

func TestChildRunnerRunChildFix(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "a", Name: "A", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "t1", ProjectID: "p1", Source: "inbox", Title: "do work", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{
		ID: "parent-run", TaskID: "t1", ProjectID: "p1",
		Pipeline: "finish-branch", Status: "running",
	}); err != nil {
		t.Fatal(err)
	}

	// Swap the registered build pipeline for a recorder.
	rec := &recordingBuild{result: &pipeline.Result{Status: "done", Summary: "fixed"}}
	d.pipelines["build"] = rec

	parentPR := &pipeline.Run{
		ID:           "parent-run",
		Task:         &store.Task{ID: "t1", ProjectID: "p1", Title: "do work", Body: "orig body"},
		Project:      &store.Project{ID: "p1", Slug: "a", Name: "A"},
		WorktreePath: "/tmp/wt-parent",
		BranchName:   "br",
		Pipeline:     "finish-branch",
	}

	cr := &childRunner{d: d}
	res, err := cr.RunChildFix(ctx, parentPR, "test", "FAIL: x_test.go:10")
	if err != nil {
		t.Fatalf("RunChildFix: %v", err)
	}
	if res == nil || res.Status != "done" {
		t.Fatalf("Result.Status = %v, want done", res)
	}

	if rec.gotRun == nil {
		t.Fatal("recorder captured no run")
	}
	if rec.gotRun.WorktreePath != "/tmp/wt-parent" {
		t.Errorf("child WorktreePath = %q, want parent's /tmp/wt-parent", rec.gotRun.WorktreePath)
	}
	if rec.gotRun.Task.Body == "orig body" {
		t.Error("child Task.Body was not overridden with a fix prompt")
	}
	if parentPR.Task.Body != "orig body" {
		t.Errorf("parent Task.Body = %q, want unchanged \"orig body\" (shallow copy must not mutate parent)", parentPR.Task.Body)
	}
	if rec.gotRun.BranchName != "br" {
		t.Errorf("child BranchName = %q, want parent's br", rec.gotRun.BranchName)
	}

	// The child run row must persist with ParentRunID + Pipeline "build".
	childRow, err := d.store.GetRun(ctx, rec.gotRun.ID)
	if err != nil {
		t.Fatalf("GetRun(child %q): %v", rec.gotRun.ID, err)
	}
	if childRow.ParentRunID != "parent-run" {
		t.Errorf("child ParentRunID = %q, want parent-run", childRow.ParentRunID)
	}
	if childRow.Pipeline != "build" {
		t.Errorf("child Pipeline = %q, want build", childRow.Pipeline)
	}
	if childRow.TaskID != "t1" {
		t.Errorf("child TaskID = %q, want t1 (reuse parent task)", childRow.TaskID)
	}
}

func TestChildRunIndividuallyCancelable(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	// Seed project + task; record parent shape we'll pass to RunChildFix.
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p-cc", Slug: "p-cc", Name: "CC"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t-cc", ProjectID: "p-cc", Title: "cc", Body: "x", Priority: "P1", Status: "running", Pipeline: "finish-branch"}); err != nil {
		t.Fatal(err)
	}
	parentTask, _ := d.store.GetTask(ctx, "t-cc")
	parentProj, _ := d.store.GetProject(ctx, "p-cc")
	parent := &pipeline.Run{
		ID: "parent-cc", Task: parentTask, Project: parentProj,
		WorktreePath: t.TempDir(), BranchName: "main", Pipeline: "finish-branch",
	}

	// Register a stub Build pipeline that blocks until ctx cancels.
	blocker := &blockingBuildPipeline{started: make(chan struct{})}
	d.pipelines["build"] = blocker

	c := &childRunner{d: d}

	// Launch RunChildFix in a goroutine; capture the child's runID once it's inserted.
	resultCh := make(chan error, 1)
	go func() {
		_, err := c.RunChildFix(ctx, parent, "test-gate", "test output")
		resultCh <- err
	}()

	// Wait until the stub Build pipeline is in its blocking state — by
	// then RunChildFix has inserted the child row and registered its cancel.
	select {
	case <-blocker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocker never started")
	}

	// Locate the child run ID by listing runs with the parent_run_id we set.
	allRunning, _ := d.store.ListAllRunningRuns(ctx)
	var childID string
	for _, r := range allRunning {
		if r.ParentRunID == "parent-cc" {
			childID = r.ID
			break
		}
	}
	if childID == "" {
		t.Fatal("child run not found in running set; RunChildFix may not have inserted")
	}

	// Cancel ONLY the child via the daemon's cancel registry.
	if !d.cancelRun(childID) {
		t.Fatalf("d.cancelRun(%q) returned false; child cancel was not registered", childID)
	}

	// Child should return with context.Canceled within 1s.
	select {
	case err := <-resultCh:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("RunChildFix returned err=%v, want context.Canceled", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("RunChildFix didn't return after child cancel")
	}

	// Parent ctx not canceled.
	if ctx.Err() != nil {
		t.Errorf("parent ctx unexpectedly canceled: %v", ctx.Err())
	}
}

func TestChildCancelUnregisteredOnReturn(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p-uo", Slug: "p-uo", Name: "UO"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t-uo", ProjectID: "p-uo", Title: "uo", Body: "x", Priority: "P1", Status: "running", Pipeline: "finish-branch"}); err != nil {
		t.Fatal(err)
	}
	parentTask, _ := d.store.GetTask(ctx, "t-uo")
	parentProj, _ := d.store.GetProject(ctx, "p-uo")
	parent := &pipeline.Run{
		ID: "parent-uo", Task: parentTask, Project: parentProj,
		WorktreePath: t.TempDir(), BranchName: "main", Pipeline: "finish-branch",
	}

	// Stub Build pipeline that returns immediately with done.
	d.pipelines["build"] = &fastDonePipeline{}

	c := &childRunner{d: d}
	_, err := c.RunChildFix(ctx, parent, "test-gate", "out")
	if err != nil {
		t.Fatalf("RunChildFix returned err=%v, want nil", err)
	}

	// Find the child run ID and verify its cancel is unregistered.
	allRunning, _ := d.store.ListAllRunningRuns(ctx)
	for _, r := range allRunning {
		if r.ParentRunID == "parent-uo" {
			t.Fatalf("child run %s still in running set after RunChildFix returned", r.ID)
		}
	}
	// The completed child's cancel should not be in d.runCancels.
	d.runCancelsMu.Lock()
	defer d.runCancelsMu.Unlock()
	for runID := range d.runCancels {
		// Any registered cancel for a finished child indicates a leak.
		if r, _ := d.store.GetRun(ctx, runID); r != nil && r.ParentRunID == "parent-uo" {
			t.Errorf("child %s cancel still registered after return", runID)
		}
	}
}

// blockingBuildPipeline is a Pipeline stub that blocks until ctx cancels.
type blockingBuildPipeline struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingBuildPipeline) Name() string { return "build" }
func (b *blockingBuildPipeline) Run(ctx context.Context, run *pipeline.Run) (*pipeline.Result, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// fastDonePipeline is a Pipeline stub that returns done immediately.
type fastDonePipeline struct{}

func (*fastDonePipeline) Name() string { return "build" }
func (*fastDonePipeline) Run(_ context.Context, _ *pipeline.Run) (*pipeline.Result, error) {
	return &pipeline.Result{Status: "done", Summary: "ok"}, nil
}
