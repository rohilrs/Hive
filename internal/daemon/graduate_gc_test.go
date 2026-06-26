package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReapGraduateWorktrees(t *testing.T) {
	d := newTestDaemon(t)
	hd := d.HiveDir()
	// A leaked worktree DIR (no real git worktree — exercises the rm fallback).
	leaked := filepath.Join(hd, "graduate-demo-123456")
	if err := os.MkdirAll(leaked, 0o755); err != nil {
		t.Fatal(err)
	}
	// The persisted RESULT files must be preserved (they are files, not dirs).
	resJSON := filepath.Join(hd, "graduate-demo-result.json")
	resMD := filepath.Join(hd, "graduate-demo-result.md")
	for _, p := range []string{resJSON, resMD} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d.reapGraduateWorktrees(0) // minAge 0 = reap all (boot semantics)

	if _, err := os.Stat(leaked); !os.IsNotExist(err) {
		t.Errorf("leaked graduate worktree dir should be reaped; stat err=%v", err)
	}
	for _, p := range []string{resJSON, resMD} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("result file %s must NOT be deleted: %v", p, err)
		}
	}
}

func TestReapGraduateWorktreesRespectsMinAge(t *testing.T) {
	d := newTestDaemon(t)
	fresh := filepath.Join(d.HiveDir(), "graduate-demo-999")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	d.reapGraduateWorktrees(time.Hour) // a just-created dir is younger than 1h → kept
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh graduate worktree must NOT be reaped under minAge: %v", err)
	}
}
