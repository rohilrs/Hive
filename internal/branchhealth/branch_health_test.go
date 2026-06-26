package branchhealth

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInit builds a throwaway repo on branch "target" with one commit.
func gitInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("checkout", "-q", "-b", "target")
	writeAndCommit(t, dir, "base.txt", "base\n", "base")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeAndCommit(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-q", "-m", msg)
}

func TestCheckFeatureBranch_BehindAndAhead(t *testing.T) {
	dir := gitInit(t)
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	writeAndCommit(t, dir, "f.txt", "feat\n", "feature work") // feature +1 ahead
	gitRun(t, dir, "checkout", "-q", "target")
	writeAndCommit(t, dir, "t.txt", "more\n", "target work") // target +1 (feature now behind)

	rep, err := CheckFeatureBranch(dir, "feature", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Behind != 1 {
		t.Errorf("Behind = %d, want 1", rep.Behind)
	}
	if rep.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", rep.Ahead)
	}
	if rep.Clean {
		t.Error("a stale (behind) branch should not be Clean")
	}
}

func TestCheckFeatureBranch_CleanWhenUpToDate(t *testing.T) {
	dir := gitInit(t)
	gitRun(t, dir, "checkout", "-q", "-b", "feature") // identical to target
	rep, err := CheckFeatureBranch(dir, "feature", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Behind != 0 || rep.Ahead != 0 || !rep.Clean {
		t.Errorf("up-to-date feature should be clean: %+v", rep)
	}
}

func TestCheckFeatureBranch_DetectsConflict(t *testing.T) {
	dir := gitInit(t)
	// both branches edit base.txt differently → real conflict on merge
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	writeAndCommit(t, dir, "base.txt", "feature-change\n", "feature edits base")
	gitRun(t, dir, "checkout", "-q", "target")
	writeAndCommit(t, dir, "base.txt", "target-change\n", "target edits base")

	rep, err := CheckFeatureBranch(dir, "feature", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ConflictPaths) == 0 {
		t.Errorf("expected conflict on base.txt, got none: %+v", rep)
	}
	found := false
	for _, p := range rep.ConflictPaths {
		if strings.Contains(p, "base.txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("ConflictPaths %v should include base.txt", rep.ConflictPaths)
	}
}

// TestCheckFeatureBranch_IgnoresSpecifiedDirtyPath verifies that
// ignoreDirtyPath excludes the reconciler-owned roadmap file from the
// Dirty/Clean computation while still flagging any other uncommitted work.
func TestCheckFeatureBranch_IgnoresSpecifiedDirtyPath(t *testing.T) {
	const roadmap = "docs/superpowers/roadmaps/slug.md"

	// Build a repo where feature == target (otherwise clean), then commit a
	// baseline roadmap file so subsequent edits show as " M" (modified tracked).
	dir := gitInit(t)
	gitRun(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.MkdirAll(filepath.Join(dir, "docs", "superpowers", "roadmaps"), 0755); err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, dir, roadmap, "initial roadmap\n", "add roadmap baseline")

	// Sync target to the same commit so Behind == 0.
	gitRun(t, dir, "checkout", "-q", "target")
	gitRun(t, dir, "merge", "--ff-only", "feature")
	gitRun(t, dir, "checkout", "-q", "feature")

	// Now dirty the roadmap without committing (the reconciler pattern).
	if err := os.WriteFile(filepath.Join(dir, roadmap), []byte("reconciler output\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Without exclusion: roadmap counts → Dirty=true.
	rep, err := CheckFeatureBranch(dir, "feature", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Dirty {
		t.Errorf("expected Dirty=true when roadmap is dirty and not excluded; got %+v", rep)
	}

	// With exclusion: roadmap ignored → Dirty=false, Clean=true.
	rep2, err := CheckFeatureBranch(dir, "feature", "target", roadmap)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Dirty {
		t.Errorf("expected Dirty=false when roadmap is excluded; got %+v", rep2)
	}
	if !rep2.Clean {
		t.Errorf("expected Clean=true when only dirty path is excluded; got %+v", rep2)
	}

	// Also dirty a second real file: roadmap excluded but real file still flags Dirty.
	if err := os.WriteFile(filepath.Join(dir, "src.txt"), []byte("real work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rep3, err := CheckFeatureBranch(dir, "feature", "target", roadmap)
	if err != nil {
		t.Fatal(err)
	}
	if !rep3.Dirty {
		t.Errorf("expected Dirty=true when real file is also dirty (roadmap excluded); got %+v", rep3)
	}
}
