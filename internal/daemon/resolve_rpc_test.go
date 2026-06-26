package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// TestHandleResolveNowMissingTaskIDReturnsInvalidParams pins the param guard:
// resolve.now with no task_id must reject before touching the store.
func TestHandleResolveNowMissingTaskIDReturnsInvalidParams(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	_, rpcErr := srv.handleResolveNow(context.Background(), json.RawMessage(`{}`))
	if rpcErr == nil {
		t.Fatal("expected ErrInvalidParams for missing task_id")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want %d", rpcErr.Code, rpc.ErrInvalidParams)
	}
}

// TestHandleResolveNowUnknownTaskReturnsTaskNotFound pins that a non-existent
// task surfaces ErrTaskNotFound (so the CLI tells the operator the id is wrong
// rather than failing opaquely deeper in provisioning).
func TestHandleResolveNowUnknownTaskReturnsTaskNotFound(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	_, rpcErr := srv.handleResolveNow(context.Background(), json.RawMessage(`{"task_id":"ghost"}`))
	if rpcErr == nil {
		t.Fatal("expected error for unknown task")
	}
	if rpcErr.Code != rpc.ErrTaskNotFound {
		t.Errorf("code=%d, want %d (ErrTaskNotFound); message=%q", rpcErr.Code, rpc.ErrTaskNotFound, rpcErr.Message)
	}
}

// TestHandleResolveNowRejectsRunningTask pins the status guard: resolve.now must
// reject tasks whose status is "running" or "done" with ErrInvalidParams,
// because those tasks are not stuck (the resolver only applies to
// needs_attention/awaiting-merge tasks). Without the guard, a concurrent
// running task would get a second goroutine dispatched against it.
func TestHandleResolveNowRejectsRunningTask(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)
	srv := NewRPCServer(d)

	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p1", Name: "P1", Status: "active"}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	for _, status := range []string{"running", "done"} {
		taskID := "task-" + status
		if err := d.store.InsertTask(ctx, &store.Task{
			ID: taskID, ProjectID: "p1", Source: "inbox",
			Title: "task", Body: "", Priority: "P1",
			Status: status, Pipeline: "build",
		}); err != nil {
			t.Fatalf("InsertTask(%s): %v", status, err)
		}
		params, _ := json.Marshal(map[string]string{"task_id": taskID})
		_, rpcErr := srv.handleResolveNow(ctx, params)
		if rpcErr == nil {
			t.Fatalf("status=%s: expected ErrInvalidParams, got nil error", status)
		}
		if rpcErr.Code != rpc.ErrInvalidParams {
			t.Errorf("status=%s: code=%d, want %d (ErrInvalidParams); message=%q",
				status, rpcErr.Code, rpc.ErrInvalidParams, rpcErr.Message)
		}
	}
}

// resolveNowFixture seeds a project (with a configured feature branch) plus a
// needs_attention task eligible for manual resolve, and returns the server +
// the task's feature branch.
func resolveNowFixture(t *testing.T, slug, branch string) (*RPCServer, *Daemon, string) {
	t.Helper()
	ctx := context.Background()
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	writePerProjectConfig(t, d.HiveDir(), slug, "[integration]\nfeature_branch = \""+branch+"\"\n")
	rp := initGitRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: slug, Slug: slug, Name: slug, Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x",
		Status: "needs_attention", GateState: sequence.GateAwaitingMerge,
		Pipeline: "build", Priority: "P1",
	}); err != nil {
		t.Fatal(err)
	}
	// Return the composite merge-guard key the production path uses
	// (mergeGuardKey(slug, branch)), so guard-state simulation matches.
	return srv, d, mergeGuardKey(slug, branch)
}

// TestHandleResolveNowGuardHeldReturnsInProgress pins the merge-queue interlock:
// when a queue merge holds the mergeGuard for the task's feature branch, a manual
// `hive resolve` (resolve.now) must reject with the "merge already in progress"
// error and must NOT dispatch (the guard stays held — we never released it).
func TestHandleResolveNowGuardHeldReturnsInProgress(t *testing.T) {
	srv, d, guardKey := resolveNowFixture(t, "held", "spec/feat")

	// Simulate an auto-queue merge in flight for this (project, branch).
	if !d.mergeGuard.tryAcquire(guardKey) {
		t.Fatal("guard should be free at setup")
	}

	params, _ := json.Marshal(map[string]string{"task_id": "held-t"})
	_, rpcErr := srv.handleResolveNow(context.Background(), params)
	if rpcErr == nil {
		t.Fatal("expected an in-progress error while the guard is held")
	}
	if rpcErr.Code != rpc.ErrInvalidParams {
		t.Errorf("code=%d, want %d; message=%q", rpcErr.Code, rpc.ErrInvalidParams, rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "merge is already in progress") {
		t.Errorf("message=%q, want it to mention merge in progress", rpcErr.Message)
	}
	// The RPC must NOT have released the guard it never owned: the in-flight
	// merge still holds it (proof no dispatch happened, and no double-release).
	if d.mergeGuard.tryAcquire(guardKey) {
		t.Error("guard must still be held by the in-flight merge (RPC must not touch it)")
	}
}

// TestHandleResolveNowAcquiresAndReleasesGuard pins the happy path: with the
// guard free, resolve.now acquires it, dispatches the (synchronous) manual
// resolve on a tracked goroutine, and releases the guard when that goroutine
// returns. Here resolvePRBranchForTask fails fast (no recoverable PR branch), so
// dispatchResolveRunManual returns quickly without provisioning — the observable
// is that the guard is acquired during dispatch and freed afterward.
func TestHandleResolveNowAcquiresAndReleasesGuard(t *testing.T) {
	srv, d, guardKey := resolveNowFixture(t, "free", "spec/feat")

	params, _ := json.Marshal(map[string]string{"task_id": "free-t"})
	_, rpcErr := srv.handleResolveNow(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("expected success while the guard is free, got %v", rpcErr)
	}

	// The dispatch runs on a tracked goroutine that defers release(guardKey). Poll
	// until the guard frees — proving it was both acquired and released.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if d.mergeGuard.tryAcquire(guardKey) {
			d.mergeGuard.release(guardKey)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guard was not released after the manual resolve goroutine finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestResolvePRBranchForTaskPrefersMetadata pins branch resolution priority:
// an explicit metadata branch_name wins (Linear's canonical branch / operator
// override) over any run-recorded branch.
func TestResolvePRBranchForTaskPrefersMetadata(t *testing.T) {
	d := newTestDaemon(t)
	task := &store.Task{
		ID:       "task-1",
		Metadata: map[string]any{"branch_name": "rohil/HBA-7-fix"},
	}
	got, err := d.scheduler.resolvePRBranchForTask(context.Background(), task)
	if err != nil {
		t.Fatalf("resolvePRBranchForTask: %v", err)
	}
	if got != "rohil/HBA-7-fix" {
		t.Errorf("branch=%q, want metadata branch", got)
	}
}
