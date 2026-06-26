package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func run(id, status string, ended int64) *store.Run {
	r := &store.Run{ID: id, Status: status}
	if ended != 0 {
		t := time.Unix(ended, 0).UTC()
		r.EndedAt = &t
	}
	return r
}

func runAt(id, status string, ended, created int64) *store.Run {
	r := run(id, status, ended)
	r.CreatedAt = time.Unix(created, 0).UTC()
	return r
}

func TestDeriveTaskStatus(t *testing.T) {
	cases := []struct {
		name string
		gate string
		runs []*store.Run
		want string
	}{
		{"empty", "", nil, "pending"},
		{"running dominates", "", []*store.Run{run("a", "done", 10), run("b", "running", 0)}, "running"},
		{"gate satisfied overrides needs_attention", sequence.GateSatisfied,
			[]*store.Run{run("a", "needs_attention", 10)}, "done"},
		{"latest done", "", []*store.Run{run("a", "needs_attention", 10), run("b", "done", 20)}, "done"},
		{"latest needs_attention", "", []*store.Run{run("a", "done", 10), run("b", "needs_attention", 20)}, "needs_attention"},
		{"error maps to needs_attention", "", []*store.Run{run("a", "error", 10)}, "needs_attention"},
		{"abandoned superseded ignored", "", []*store.Run{run("a", "done", 10), run("b", "abandoned", 20)}, "done"},
		{"all abandoned -> pending", "", []*store.Run{run("a", "abandoned", 10), run("b", "abandoned", 20)}, "pending"},
		// Fix 1: equal ended_at tie-break via CreatedAt (later created wins).
		{"equal ended tie-break by CreatedAt", "",
			[]*store.Run{
				runAt("a", "needs_attention", 10, 1),
				runAt("b", "done", 10, 2),
			}, "done"},
		// Fix 1: running should dominate even when gate is satisfied.
		{"running beats satisfied gate", sequence.GateSatisfied,
			[]*store.Run{run("a", "running", 0), run("b", "done", 5)}, "running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveTaskStatus(c.gate, c.runs); got != c.want {
				t.Errorf("DeriveTaskStatus(%q)=%q, want %q", c.gate, got, c.want)
			}
		})
	}
}

// TestRefreshTaskStatus verifies the effectful helper: it loads a task's gate
// + runs, derives the correct status, writes it only on change, publishes
// exactly one EventTaskUpdated (with project_id) on change, and publishes NO
// further event on a second call when the status is already correct.
//
// Scenario: task seeded as "needs_attention" with GateState=satisfied and one
// "done" run → first refreshTaskStatus must update status to "done" and fire
// one event. Second call must be a no-op (no event, status unchanged).
func TestRefreshTaskStatus(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()

	// Insert supporting project (InsertTask requires a valid project_id FK).
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "p1", Name: "P", Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}

	// Seed task: stale status "needs_attention", gate already satisfied.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID:        "task-1",
		ProjectID: "p1",
		Title:     "test task",
		Pipeline:  "build",
		Status:    "needs_attention",
		GateState: sequence.GateSatisfied,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	// Seed one terminal "done" run for the task.
	endedAt := time.Now()
	if err := d.store.InsertRun(ctx, &store.Run{
		ID:        "run-1",
		TaskID:    "task-1",
		ProjectID: "p1",
		Pipeline:  "build",
		Status:    "done",
		EndedAt:   &endedAt,
	}); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	// Subscribe BEFORE the first call so we catch the event it publishes.
	ch, cancelSub := d.bus.Subscribe()
	defer cancelSub()

	// Act: first call — status changes needs_attention → done.
	d.refreshTaskStatus(ctx, "task-1")

	// Assert: task status updated to "done" (gate satisfied overrides stale needs_attention).
	got, err := d.store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("task status = %q, want %q", got.Status, "done")
	}

	// Assert: exactly one EventTaskUpdated published, carrying task_id,
	// status="done", and project_id="p1".
	ev := waitForEventType(t, ch, rpc.EventTaskUpdated, 2*time.Second)
	if taskID, _ := ev.Data["task_id"].(string); taskID != "task-1" {
		t.Errorf("event task_id=%q, want %q", taskID, "task-1")
	}
	if status, _ := ev.Data["status"].(string); status != "done" {
		t.Errorf("event status=%q, want %q", status, "done")
	}
	if projectID, _ := ev.Data["project_id"].(string); projectID != "p1" {
		t.Errorf("event project_id=%q, want %q", projectID, "p1")
	}

	// Assert write-on-change guard: second call with status already "done" must
	// publish NO further EventTaskUpdated. Drain the channel for a short window
	// to confirm silence.
	d.refreshTaskStatus(ctx, "task-1")

	timeout := time.After(100 * time.Millisecond)
	for {
		select {
		case extra := <-ch:
			if extra.Type == rpc.EventTaskUpdated {
				t.Errorf("unexpected second EventTaskUpdated published (guard failed): %+v", extra.Data)
			}
			// other event types (e.g. resync) are fine — keep draining
		case <-timeout:
			// no spurious event: guard is working
			goto guardOK
		}
	}
guardOK:

	// Also confirm store is still "done" after the no-op second call.
	got2, err := d.store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask (2nd): %v", err)
	}
	if got2.Status != "done" {
		t.Errorf("task status after no-op = %q, want %q", got2.Status, "done")
	}
}
