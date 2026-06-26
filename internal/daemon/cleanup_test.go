package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/store"
)

func TestHandleCleanupRun(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	rp := t.TempDir()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "p", Name: "P", Status: "active", RepoPath: &rp}); err != nil {
		t.Fatal(err)
	}
	// InsertRun has a task_id FK (foreign_keys=ON), so a task row must
	// exist first. Both runs share it; only the run timestamps matter for
	// retention ordering.
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t", ProjectID: "p1", Source: "inbox", Title: "x", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	// Explicit, ordered CreatedAt so keep-last-1 deterministically retains
	// run-new (the newest) and reclaims run-old.
	mkRun := func(id string, created time.Time) {
		if err := d.store.InsertRun(ctx, &store.Run{ID: id, TaskID: "t", ProjectID: "p1", Pipeline: "build", Status: "done", CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(d.HiveDir(), id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkRun("run-old", time.Now().Add(-2*time.Hour))
	mkRun("run-new", time.Now().Add(-1*time.Hour))

	call := func(params map[string]any) map[string]any {
		raw, _ := json.Marshal(params)
		out, rerr := srv.handleCleanupRun(ctx, raw)
		if rerr != nil {
			t.Fatalf("cleanup: %v", rerr)
		}
		var m map[string]any
		_ = json.Unmarshal(out, &m)
		return m
	}

	// Dry run: reports 1 reclaimable run but leaves both dirs on disk.
	dry := call(map[string]any{"dry_run": true, "keep_last": 1})
	if int(dry["runs"].(float64)) != 1 {
		t.Errorf("dry-run runs = %v, want 1", dry["runs"])
	}
	n := 0
	for _, id := range []string{"run-old", "run-new"} {
		if _, err := os.Stat(filepath.Join(d.HiveDir(), id)); err == nil {
			n++
		}
	}
	if n != 2 {
		t.Errorf("dry-run removed dirs; %d/2 remain", n)
	}

	// Real run: reclaims exactly the one past-retention run; newest survives.
	real := call(map[string]any{"dry_run": false, "keep_last": 1})
	if int(real["runs"].(float64)) != 1 {
		t.Errorf("real runs = %v, want 1", real["runs"])
	}
	remaining := 0
	survivor := ""
	for _, id := range []string{"run-old", "run-new"} {
		if _, err := os.Stat(filepath.Join(d.HiveDir(), id)); err == nil {
			remaining++
			survivor = id
		}
	}
	if remaining != 1 {
		t.Errorf("after clean, %d/2 dirs remain, want 1", remaining)
	}
	if survivor != "run-new" {
		t.Errorf("retention kept %q, want run-new (newest)", survivor)
	}
}
