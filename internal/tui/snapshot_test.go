package tui

import (
	"testing"

	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestTaskOrderLabel(t *testing.T) {
	cases := []struct{ phase, index, want string }{
		{"1", "2", "1.2"},
		{"1a", "3", "1a.3"},
		{"1", "", ""}, // missing index → no label
		{"", "2", ""}, // missing phase → no label
		{"", "", ""},
	}
	for _, c := range cases {
		if got := taskOrder(c.phase, c.index); got != c.want {
			t.Errorf("taskOrder(%q,%q)=%q want %q", c.phase, c.index, got, c.want)
		}
	}
}

// TestApplyInitialStateNeedsAttentionNotClobberedByRecentDone: a task the
// daemon reports as needs_attention (e.g. a stuck merge — build runs are done
// but the task row is needs_attention) must keep that status even though its
// done run appears in the recent_done feed. Otherwise the recent_done
// hydration re-hides the task the sequence engine deliberately surfaced.
func TestApplyInitialStateNeedsAttentionNotClobberedByRecentDone(t *testing.T) {
	s := NewSnapshot()
	msg := initialStateMsg{
		Tasks: []rpc.TaskView{
			{ID: "t1", ProjectID: "p1", Title: "Stuck on merge", Status: rpc.TaskStatus("needs_attention")},
		},
		Status: map[string]any{
			// The done run lands in BOTH feeds (as the real daemon.status does);
			// neither may downgrade the authoritative needs_attention status.
			"recent": []any{
				map[string]any{
					"id": "run-1", "task_id": "t1", "pipeline": "finish-branch",
					"status": "done", "task_title": "Stuck on merge", "project_id": "p1",
				},
			},
			"recent_done": []any{
				map[string]any{
					"id": "run-1", "task_id": "t1", "pipeline": "finish-branch",
					"status": "done", "task_title": "Stuck on merge", "project_id": "p1",
				},
			},
		},
	}
	applyInitialState(s, msg)
	if got := s.Tasks["t1"].Status; got != "needs_attention" {
		t.Errorf("task status=%q want needs_attention (recent/recent_done done-run must not clobber the authoritative status)", got)
	}
	// And it must reach the Projects reader for the needs-attention lane.
	rows := s.TasksForProject("p1")
	if len(rows) != 1 || rows[0].Status != "needs_attention" {
		t.Errorf("TasksForProject must surface the needs_attention task; got %+v", rows)
	}
}

// TestApplyInitialStateRecentFeedDoesNotMislabelDoneTask is the regression for
// a TUI mislabel: a task that FAILED a build (a needs_attention run) and was
// then re-run to done + merged must show as done — NOT in the needs-attention
// lane. The bounded "recent" feed still contains the SUPERSEDED needs_attention
// run; the snapshot must not derive task status from it (task.list /
// recent_done are authoritative). 4a.5 hit this: done/satisfied + PR merged,
// yet shown as needs_attention off its earlier failed build run.
func TestApplyInitialStateRecentFeedDoesNotMislabelDoneTask(t *testing.T) {
	s := NewSnapshot()
	doneRun := func(id string) map[string]any {
		return map[string]any{"id": id, "task_id": "t9", "pipeline": "build", "status": "done", "task_title": "Tier 2 tests", "project_id": "p1"}
	}
	msg := initialStateMsg{
		Tasks: []rpc.TaskView{}, // done task → NOT in task.list[pending,needs_attention]
		Status: map[string]any{
			"recent": []any{
				doneRun("run-done"), // the later, successful build
				map[string]any{"id": "run-fail", "task_id": "t9", "pipeline": "build", "status": "needs_attention", "task_title": "Tier 2 tests", "project_id": "p1"},
			},
			"recent_done": []any{doneRun("run-done")},
		},
	}
	applyInitialState(s, msg)
	if tk := s.Tasks["t9"]; tk == nil || tk.Status == "needs_attention" {
		t.Fatalf("done+merged task must NOT be needs_attention off a superseded run; got %+v", tk)
	}
	rows := s.TasksForProject("p1")
	if len(rows) != 1 || rows[0].Status == "needs_attention" {
		t.Errorf("task must not surface in the needs-attention lane; got %+v", rows)
	}
}

func TestSnapshotTaskCreatedCarriesOrder(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventTaskCreated,
		Data: map[string]any{
			"task_id": "t1", "project_id": "p1", "project_slug": "demo",
			"title": "merged", "status": "pending",
			"roadmap_phase": "1", "roadmap_phase_index": "2",
		},
	})
	if s.Tasks["t1"].Order != "1.2" {
		t.Errorf("task Order=%q want 1.2", s.Tasks["t1"].Order)
	}
	rows := s.TasksForProject("p1")
	if len(rows) != 1 || rows[0].Order != "1.2" {
		t.Errorf("TasksForProject must carry Order; got %+v", rows)
	}
}

func TestSnapshotApplyRunStarted(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{"run_id": "r1", "task_id": "t1"},
	})
	r, ok := s.Runs["r1"]
	if !ok {
		t.Fatal("run r1 not added")
	}
	if r.Status != "running" {
		t.Errorf("Status=%q want running", r.Status)
	}
	if r.TaskID != "t1" {
		t.Errorf("TaskID=%q want t1", r.TaskID)
	}
}

func TestSnapshotApplyRunEnded(t *testing.T) {
	s := NewSnapshot()
	s.Runs["r1"] = &RunView{ID: "r1", Status: "running"}
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventRunEnded,
		Data: map[string]any{"run_id": "r1", "status": "done", "summary": "approved on iter 0"},
	})
	if s.Runs["r1"].Status != "done" {
		t.Errorf("Status=%q want done", s.Runs["r1"].Status)
	}
	if s.Runs["r1"].Summary != "approved on iter 0" {
		t.Errorf("Summary=%q", s.Runs["r1"].Summary)
	}
}

func TestSnapshotRunEndedUpdatesTaskStatus(t *testing.T) {
	cases := []struct {
		runStatus string
		wantTask  string
	}{
		{"done", "done"},
		{"abandoned", "needs_attention"},
		{"needs_attention", "needs_attention"},
		{"error", "needs_attention"},
	}
	for _, tc := range cases {
		s := NewSnapshot()
		s.Tasks["t1"] = &TaskView{ID: "t1", Status: "running"}
		s.Runs["r1"] = &RunView{ID: "r1", TaskID: "t1", Status: "running"}
		ApplySnapshot(s, rpc.EventMessage{
			Type: rpc.EventRunEnded,
			Data: map[string]any{"run_id": "r1", "status": tc.runStatus},
		})
		if got := s.Tasks["t1"].Status; got != tc.wantTask {
			t.Errorf("run %s -> task status %q; want %q", tc.runStatus, got, tc.wantTask)
		}
	}
}

func TestSnapshotApplyStageStartedEnded(t *testing.T) {
	s := NewSnapshot()
	s.Runs["r1"] = &RunView{ID: "r1", Status: "running"}
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventStageStarted,
		Data: map[string]any{
			"run_id": "r1", "stage_id": float64(42),
			"name": "implement", "iter": float64(0), "model": "claude-sonnet-4-6",
		},
	})
	if _, ok := s.Stages[42]; !ok {
		t.Fatal("stage 42 not added")
	}
	if s.Stages[42].Name != "implement" {
		t.Errorf("Name=%q", s.Stages[42].Name)
	}
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventStageEnded,
		Data: map[string]any{"run_id": "r1", "stage_id": float64(42), "verdict": "APPROVE"},
	})
	if s.Stages[42].Verdict != "APPROVE" {
		t.Errorf("Verdict=%q want APPROVE", s.Stages[42].Verdict)
	}
	if s.Stages[42].EndedAt == 0 {
		t.Error("EndedAt not set")
	}
}

func TestSnapshotApplyHeartbeat(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventDaemonHeartbeat,
		Data: map[string]any{"ts": float64(1234567890)},
	})
	if s.LastHeartbeat != 1234567890 {
		t.Errorf("LastHeartbeat=%d", s.LastHeartbeat)
	}
}

func TestSnapshotApplyAppendsEventRing(t *testing.T) {
	s := NewSnapshot()
	s.MaxRecentEvents = 3
	for i := 0; i < 5; i++ {
		// Use a recorded event type — heartbeats are intentionally excluded from
		// the ring (see TestRecentEventsExcludesHeartbeats).
		ApplySnapshot(s, rpc.EventMessage{Type: rpc.EventStageStarted, Data: map[string]any{"i": i}})
	}
	if len(s.RecentEvents()) != 3 {
		t.Errorf("len(RecentEvents)=%d want 3 (ring capped)", len(s.RecentEvents()))
	}
}

func TestSnapshotAccessors(t *testing.T) {
	s := NewSnapshot()
	s.Projects["p1"] = &ProjectView{ID: "p1", Slug: "a", Name: "Alpha"}
	s.Tasks["t1"] = &TaskView{ID: "t1", ProjectID: "p1", Title: "do thing"}
	s.Runs["r1"] = &RunView{ID: "r1", TaskID: "t1", Status: "running", Pipeline: "build"}
	s.Stages[1] = &StageView{ID: 1, RunID: "r1", Name: "implement", Iter: 0, StartedAt: 1000}

	if got := s.AllProjects(); len(got) != 1 || got[0].Name != "Alpha" {
		t.Errorf("AllProjects=%v", got)
	}
	if got := s.RunsForProject("p1"); len(got) != 1 || got[0].ID != "r1" {
		t.Errorf("RunsForProject=%v", got)
	}
	if got := s.TasksForProject("p1"); len(got) != 1 || got[0].Title != "do thing" {
		t.Errorf("TasksForProject=%v", got)
	}
	if got := s.ActiveRuns(); len(got) != 1 || got[0].Stage != "implement" {
		t.Errorf("ActiveRuns=%v", got)
	}
}

func TestSnapshotApplyRunStartedHydratesTaskAndProject(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventRunStarted,
		Data: map[string]any{
			"run_id":       "r1",
			"task_id":      "t1",
			"task_title":   "implement foo",
			"project_id":   "p1",
			"project_slug": "hive",
			"pipeline":     "build",
		},
	})
	if s.Runs["r1"].Pipeline != "build" {
		t.Errorf("pipeline not set: %+v", s.Runs["r1"])
	}
	if s.Tasks["t1"] == nil || s.Tasks["t1"].Title != "implement foo" {
		t.Errorf("task not hydrated: %+v", s.Tasks["t1"])
	}
	if s.Projects["p1"] == nil || s.Projects["p1"].Slug != "hive" {
		t.Errorf("project not hydrated: %+v", s.Projects["p1"])
	}
}

func TestSnapshotPendingApprovals(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventApprovalRequested,
		Data: map[string]any{
			"approval_id": "ap-1", "run_id": "r1", "stage": "implement",
			"tool_name": "Bash", "tool_input": map[string]any{"command": "make all"},
		},
	})
	pend := s.PendingApprovals()
	if len(pend) != 1 {
		t.Fatalf("PendingApprovals len=%d want 1", len(pend))
	}
	if pend[0].ToolName != "Bash" || pend[0].Arg != "make all" || pend[0].Tier != "bash" {
		t.Errorf("unexpected pending: %+v", pend[0])
	}
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventApprovalResolved,
		Data: map[string]any{"approval_id": "ap-1", "decision": "approve"},
	})
	if len(s.PendingApprovals()) != 0 {
		t.Errorf("pending should be cleared after resolve")
	}
}

func TestApplyPendingApprovalHydratesFromStatus(t *testing.T) {
	s := NewSnapshot()
	// Simulates a daemon.status pending_approvals_list entry (no live event).
	s.ApplyPendingApproval(map[string]any{
		"approval_id": "ap-9", "run_id": "r1", "stage": "implement",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "claude --version"},
	})
	pend := s.PendingApprovals()
	if len(pend) != 1 || pend[0].ToolName != "Bash" || pend[0].Arg != "claude --version" {
		t.Fatalf("hydration failed: %+v", pend)
	}
	if !s.PendingApprovalRunIDs()["r1"] {
		t.Error("run r1 should be flagged as needing approval")
	}
}

func TestApplySnapshot_EventProjectCreatedAddsProject(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventProjectCreated,
		Data: map[string]any{
			"project_id": "p1",
			"slug":       "acme",
			"name":       "Acme",
			"repo_path":  "/tmp/acme",
		},
	})
	p, ok := s.Projects["p1"]
	if !ok {
		t.Fatal("project p1 not added")
	}
	if p.Slug != "acme" || p.Name != "Acme" || p.RepoPath != "/tmp/acme" {
		t.Errorf("got %+v; want slug=acme name=Acme repo=/tmp/acme", p)
	}
}

func TestApplySnapshot_EventProjectUpdatedRenames(t *testing.T) {
	s := NewSnapshot()
	s.Projects["p1"] = &ProjectView{ID: "p1", Slug: "acme", Name: "Acme", RepoPath: "/tmp/old"}
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventProjectUpdated,
		Data: map[string]any{
			"project_id": "p1",
			"name":       "Acme Renamed",
			"repo_path":  "/tmp/new",
		},
	})
	p := s.Projects["p1"]
	if p == nil {
		t.Fatal("project p1 vanished on rename")
	}
	if p.Name != "Acme Renamed" {
		t.Errorf("Name=%q want 'Acme Renamed'", p.Name)
	}
	if p.RepoPath != "/tmp/new" {
		t.Errorf("RepoPath=%q want /tmp/new", p.RepoPath)
	}
	// Slug is unchanged (not in event payload).
	if p.Slug != "acme" {
		t.Errorf("Slug=%q want acme (unchanged)", p.Slug)
	}
}

func TestApplySnapshot_EventProjectUpdatedAppliesCanSequence(t *testing.T) {
	s := NewSnapshot()
	s.Projects["p1"] = &ProjectView{ID: "p1", Slug: "acme", CanSequence: false}
	// A roadmap save makes the gate pass — the daemon pushes can_sequence:true.
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventProjectUpdated,
		Data: map[string]any{"project_id": "p1", "slug": "acme", "can_sequence": true},
	})
	if !s.Projects["p1"].CanSequence {
		t.Error("CanSequence should flip to true from the event")
	}
	// Absent key must leave it unchanged (other project.updated events omit it).
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventProjectUpdated,
		Data: map[string]any{"project_id": "p1", "name": "Acme"},
	})
	if !s.Projects["p1"].CanSequence {
		t.Error("CanSequence must be preserved when the key is absent")
	}
}

func TestSnapshotTaskIntegrationState(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventTaskCreated,
		Data: map[string]any{"task_id": "t1", "project_id": "p1", "title": "x", "status": "pending"},
	})
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventTaskIntegrating,
		Data: map[string]any{"task_id": "t1"},
	})
	if s.Tasks["t1"].IntegrationState != "integrating" {
		t.Errorf("integrating not applied: %+v", s.Tasks["t1"])
	}
	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventPROpened,
		Data: map[string]any{"task_id": "t1", "pr_url": "https://github.com/o/r/pull/7", "pr_number": float64(7)},
	})
	if s.Tasks["t1"].IntegrationState != "pr_open" || s.Tasks["t1"].PRNumber != 7 || s.Tasks["t1"].PRURL == "" {
		t.Errorf("pr_open not applied: %+v", s.Tasks["t1"])
	}
	ApplySnapshot(s, rpc.EventMessage{Type: rpc.EventTaskMerged, Data: map[string]any{"task_id": "t1"}})
	if s.Tasks["t1"].IntegrationState != "merged" {
		t.Errorf("merged not applied: %+v", s.Tasks["t1"])
	}
}

func TestApplySnapshot_EventProjectUpdatedDeletedClearsProjectAndCascade(t *testing.T) {
	s := NewSnapshot()
	// Project being deleted, with a task + run hanging off it.
	s.Projects["p1"] = &ProjectView{ID: "p1", Slug: "acme", Name: "Acme"}
	s.Tasks["t1"] = &TaskView{ID: "t1", ProjectID: "p1", Title: "task in acme"}
	s.Runs["r1"] = &RunView{ID: "r1", TaskID: "t1", Status: "done"}
	// Sibling project + its task/run that must NOT be touched.
	s.Projects["p2"] = &ProjectView{ID: "p2", Slug: "other", Name: "Other"}
	s.Tasks["t2"] = &TaskView{ID: "t2", ProjectID: "p2", Title: "task in other"}
	s.Runs["r2"] = &RunView{ID: "r2", TaskID: "t2", Status: "done"}

	ApplySnapshot(s, rpc.EventMessage{
		Type: rpc.EventProjectUpdated,
		Data: map[string]any{"project_id": "p1", "deleted": true},
	})

	if _, ok := s.Projects["p1"]; ok {
		t.Error("project p1 still present after deleted=true")
	}
	if _, ok := s.Tasks["t1"]; ok {
		t.Error("task t1 still present after cascade clear")
	}
	if _, ok := s.Runs["r1"]; ok {
		t.Error("run r1 still present after cascade clear")
	}
	// Sibling project untouched.
	if _, ok := s.Projects["p2"]; !ok {
		t.Error("sibling project p2 was cleared")
	}
	if _, ok := s.Tasks["t2"]; !ok {
		t.Error("sibling task t2 was cleared")
	}
	if _, ok := s.Runs["r2"]; !ok {
		t.Error("sibling run r2 was cleared")
	}
}

// TestRecentEventsExcludesHeartbeats is the regression for the events-drill-in
// clearing on a long run: daemon heartbeats (5s cadence) must NOT enter the
// bounded recentEvents ring, or they evict real run/stage events in ~17min.
func TestRecentEventsExcludesHeartbeats(t *testing.T) {
	s := NewSnapshot()
	ApplySnapshot(s, rpc.EventMessage{Type: rpc.EventRunStarted, Data: map[string]any{"run_id": "r1", "task_id": "t1"}})
	for i := 0; i < s.MaxRecentEvents+50; i++ {
		ApplySnapshot(s, rpc.EventMessage{Type: rpc.EventDaemonHeartbeat, Data: map[string]any{"ts": float64(i)}})
	}
	evs := s.RecentEvents()
	for _, e := range evs {
		if e.Type == rpc.EventDaemonHeartbeat {
			t.Fatal("heartbeat leaked into recentEvents ring")
		}
	}
	found := false
	for _, e := range evs {
		if e.Type == rpc.EventRunStarted {
			found = true
		}
	}
	if !found {
		t.Error("run.started was evicted by the heartbeat flood (the bug)")
	}
	if s.LastHeartbeat == 0 {
		t.Error("LastHeartbeat not updated (heartbeat still must drive the liveness indicator)")
	}
}
