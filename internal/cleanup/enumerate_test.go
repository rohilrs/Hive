package cleanup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnumerateDirs(t *testing.T) {
	h := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{h}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("run-1")
	mk("worktrees", "run-1")
	mk("worktrees", "run-2")
	mk("projects", "seqp")
	mk("logs")

	dirs, err := EnumerateDirs(h)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DirInfo{}
	for _, d := range dirs {
		byID[d.RunID] = d
	}
	if len(byID) != 2 {
		t.Fatalf("got %d runs, want 2 (run-1, run-2): %+v", len(byID), byID)
	}
	if byID["run-1"].Scratch == "" || byID["run-1"].Worktree == "" {
		t.Errorf("run-1 should have both scratch + worktree: %+v", byID["run-1"])
	}
	if byID["run-2"].Worktree == "" || byID["run-2"].Scratch != "" {
		t.Errorf("run-2 should have worktree only: %+v", byID["run-2"])
	}
}
