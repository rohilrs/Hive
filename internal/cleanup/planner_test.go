package cleanup

import (
	"testing"
	"time"
)

func ts(min int) time.Time { return time.Date(2026, 6, 3, 0, min, 0, 0, time.UTC) }

func ids(items []ReclaimItem) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it.RunID] = true
	}
	return m
}

func TestBuildPlanRetentionAndSafety(t *testing.T) {
	now := ts(100)
	runs := []RunInfo{
		{ID: "run-1", Status: "done", CreatedAt: ts(10), RepoPath: "/r", BranchName: "hive/1"},
		{ID: "run-2", Status: "done", CreatedAt: ts(20), RepoPath: "/r", BranchName: "hive/2"},
		{ID: "run-3", Status: "running", CreatedAt: ts(30), RepoPath: "/r"},
		{ID: "run-4", Status: "needs_attention", CreatedAt: ts(40), RepoPath: "/r"},
		{ID: "run-5", Status: "pending", CreatedAt: ts(50), ParentRunID: "run-1", RepoPath: "/r"},
	}
	dirs := []DirInfo{
		{RunID: "run-1", Worktree: "/h/worktrees/run-1", Scratch: "/h/run-1", Mtime: ts(10)},
		{RunID: "run-2", Worktree: "/h/worktrees/run-2", Scratch: "/h/run-2", Mtime: ts(20)},
		{RunID: "run-3", Scratch: "/h/run-3", Mtime: ts(30)},
		{RunID: "run-4", Scratch: "/h/run-4", Mtime: ts(40)},
		{RunID: "run-99", Scratch: "/h/run-99", Mtime: ts(5)},
		{RunID: "run-98", Scratch: "/h/run-98", Mtime: ts(95)},
	}
	ret := Retention{KeepLastRuns: 2, OrphanGrace: 30 * time.Minute, Branches: true}
	plan := BuildPlan(runs, dirs, ret, now)
	got := ids(plan.Reclaim)

	want := map[string]bool{"run-2": true, "run-99": true}
	if len(got) != len(want) {
		t.Fatalf("reclaim set = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("expected %s reclaimable; got %v", id, got)
		}
	}
	if got["run-1"] {
		t.Error("run-1 has an in-flight child-fix; must be protected")
	}
	for _, it := range plan.Reclaim {
		if it.RunID == "run-2" && (it.BranchName != "hive/2" || it.RepoPath != "/r") {
			t.Errorf("run-2 item missing branch/repo: %+v", it)
		}
	}
}
