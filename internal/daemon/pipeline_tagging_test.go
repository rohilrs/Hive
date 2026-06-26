package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// rpcCallExpectErr issues an RPC and returns the error message, failing
// the test if the call unexpectedly succeeds. Mirrors rpcCall but does
// not t.Fatal on an RPC-level error envelope.
func rpcCallExpectErr(t *testing.T, sockPath, method string, params map[string]any) string {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, _ := json.Marshal(map[string]any{"id": "req-1", "method": method, "params": params})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write rpc request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read rpc response: %v", err)
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("unmarshal rpc response: %v: %s", err, string(buf[:n]))
	}
	if resp.Error == nil {
		t.Fatalf("rpc %s: expected error, got success", method)
	}
	return resp.Error.Message
}

func TestAddTaskRejectsUnknownPipeline(t *testing.T) {
	hiveDir := t.TempDir()
	d, err := New(Config{HiveDir: hiveDir, Cfg: config.Default(), Adapter: noopAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	// Unknown pipeline is rejected.
	msg := rpcCallExpectErr(t, sockPath, rpc.MethodAddTask, map[string]any{
		"project_slug": "a", "title": "x", "pipeline": "nonsense",
	})
	if msg == "" {
		t.Error("expected non-empty error message for unknown pipeline")
	}

	// "build" (the only registered pipeline) is accepted.
	ok := rpcCall(t, sockPath, rpc.MethodAddTask, map[string]any{
		"project_slug": "a", "title": "y", "pipeline": "build",
	})
	if ok["task_id"] == nil {
		t.Errorf("build pipeline should be accepted; got %#v", ok)
	}
}

// TestAddTaskThreadsMetadata pins the Phase 8.B extension: AddTaskParams
// now carries a Metadata map[string]string that is persisted on the task
// row (store column already supports map[string]any; the wire-side narrow
// type avoids accidental interface-typed JSON payloads). Roadmap-
// decomposed tasks rely on this to stamp roadmap_phase / roadmap_path /
// spec_path linkage at insert time so they can be located again later.
func TestAddTaskThreadsMetadata(t *testing.T) {
	hiveDir := t.TempDir()
	d, err := New(Config{HiveDir: hiveDir, Cfg: config.Default(), Adapter: noopAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Start(ctx) }()
	defer d.Stop()
	if !d.WaitReady(5 * time.Second) {
		t.Fatal("daemon did not become ready within 5s")
	}

	sockPath := filepath.Join(hiveDir, "daemon.sock")
	waitFor(t, func() bool { _, err := os.Stat(sockPath); return err == nil }, 3*time.Second)

	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	resp := rpcCall(t, sockPath, rpc.MethodAddTask, map[string]any{
		"project_slug": "a",
		"title":        "with meta",
		"metadata": map[string]string{
			"roadmap_phase": "1",
			"roadmap_path":  "/abs/roadmap.md",
			"spec_path":     "docs/superpowers/specs/x.md",
		},
	})
	taskID, _ := resp["task_id"].(string)
	if taskID == "" {
		t.Fatalf("task.add returned no task_id: %#v", resp)
	}
	got, err := d.store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if v, _ := got.Metadata["roadmap_phase"].(string); v != "1" {
		t.Errorf("metadata.roadmap_phase = %q, want %q", v, "1")
	}
	if v, _ := got.Metadata["roadmap_path"].(string); v != "/abs/roadmap.md" {
		t.Errorf("metadata.roadmap_path = %q, want %q", v, "/abs/roadmap.md")
	}
	if v, _ := got.Metadata["spec_path"].(string); v != "docs/superpowers/specs/x.md" {
		t.Errorf("metadata.spec_path = %q", v)
	}
}

func TestRecoverStaleRunsOnStartup(t *testing.T) {
	hiveDir := t.TempDir()
	d, err := New(Config{HiveDir: hiveDir, Cfg: config.Default(), Adapter: noopAdapter{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Seed a project + task + a run left in "running" (as if a previous
	// daemon died mid-run).
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "a", Name: "A", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Title: "x", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertRun(ctx, &store.Run{ID: "run-stale", TaskID: "t1", ProjectID: "p1", Pipeline: "build", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.MarkRunStarted(ctx, "run-stale"); err != nil {
		t.Fatal(err)
	}

	NewScheduler(d).recoverStaleRuns(ctx)

	r, err := d.store.GetRun(ctx, "run-stale")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "needs_attention" {
		t.Errorf("stale run status=%q want needs_attention", r.Status)
	}
}
