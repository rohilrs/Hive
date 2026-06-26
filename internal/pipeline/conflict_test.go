package pipeline

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// seedConflictRepo creates a repo where "feature" and "target" both edit line 1
// of f.txt differently -> a guaranteed content conflict.
func seedConflictRepo(t *testing.T) string {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "target")
	writeFile(t, dir, "f.txt", "base\ncommon\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	git(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "f.txt", "feature-change\ncommon\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "feature")
	git(t, dir, "checkout", "-q", "target")
	writeFile(t, dir, "f.txt", "target-change\ncommon\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "target")
	git(t, dir, "checkout", "-q", "feature")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := osWriteFile(filepath.Join(dir, name), content); err != nil {
		t.Fatal(err)
	}
}

// seedConflictRepoWithAutoMerge creates a repo where merging "target" into
// "feature" BOTH conflicts on f.txt AND auto-merges other.txt cleanly.
//
//	base:    f.txt = "base\ncommon\n",  other.txt = "l1\nl2\nl3\n"
//	feature: f.txt line1 → "feature\ncommon\n"; other.txt line1 → "FEAT\nl2\nl3\n"
//	target:  f.txt line1 → "target\ncommon\n" (conflicts with feature);
//	         other.txt line3 → "l1\nl2\nTARGET\n" (auto-merges clean w/ feature's line1 edit)
//
// After BuildConflictContext runs the merge, git stages other.txt (auto-merged)
// while leaving f.txt unstaged with conflict markers. The guard under test must
// NOT flag other.txt as an out-of-scope agent edit.
func seedConflictRepoWithAutoMerge(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "target")
	writeFile(t, dir, "f.txt", "base\ncommon\n")
	writeFile(t, dir, "other.txt", "l1\nl2\nl3\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	// feature branch: edit both files on different lines to each
	git(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "f.txt", "feature\ncommon\n")
	writeFile(t, dir, "other.txt", "FEAT\nl2\nl3\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "feature")
	// target branch (off base): edit f.txt line1 (conflicts) and other.txt line3 (auto-merges)
	git(t, dir, "checkout", "-q", "target")
	writeFile(t, dir, "f.txt", "target\ncommon\n")
	writeFile(t, dir, "other.txt", "l1\nl2\nTARGET\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "target")
	git(t, dir, "checkout", "-q", "feature")
	return dir
}

func TestBuildConflictContextCapturesConflict(t *testing.T) {
	dir := seedConflictRepo(t)
	cc, err := BuildConflictContext(dir, "target")
	if err != nil {
		t.Fatalf("BuildConflictContext: %v", err)
	}
	if cc.Clean {
		t.Fatal("expected a conflict, got Clean=true")
	}
	if len(cc.Files) != 1 || cc.Files[0].Path != "f.txt" {
		t.Fatalf("conflicted files = %+v want [f.txt]", cc.Files)
	}
	if !strings.Contains(cc.Files[0].Merged, "<<<<<<<") {
		t.Errorf("Merged should contain conflict markers:\n%s", cc.Files[0].Merged)
	}
	if !strings.Contains(cc.Files[0].OursDiff, "feature-change") {
		t.Errorf("OursDiff should show the feature side; got:\n%s", cc.Files[0].OursDiff)
	}
	if !strings.Contains(cc.Files[0].TheirsDiff, "target-change") {
		t.Errorf("TheirsDiff should show the target side; got:\n%s", cc.Files[0].TheirsDiff)
	}
}

// TestBuildConflictContextFetchesBaseBeforeMerge proves the resolver merges the
// FRESH origin base, not the worktree's stale local ref. Setup: a worktree whose
// local `target` is behind, while `origin/target` carries an extra commit that
// introduces the conflict. If BuildConflictContext merged the local ref it would
// merge cleanly (Clean=true); only by fetching + merging origin/target does the
// origin-only conflicting change appear, producing a real conflict on f.txt.
func TestBuildConflictContextFetchesBaseBeforeMerge(t *testing.T) {
	// origin is a bare repo acting as the remote.
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "target")

	// upstream is a normal clone used to push commits to origin/target.
	upstream := t.TempDir()
	git(t, upstream, "init", "-q", "-b", "target")
	git(t, upstream, "remote", "add", "origin", origin)
	writeFile(t, upstream, "f.txt", "base\ncommon\n")
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-qm", "base")
	git(t, upstream, "push", "-q", "origin", "target")

	// worktree clones origin at the BASE state, then branches feature off it.
	worktree := t.TempDir()
	git(t, worktree, "clone", "-q", origin, ".")
	git(t, worktree, "checkout", "-q", "-b", "feature")
	writeFile(t, worktree, "f.txt", "feature-change\ncommon\n")
	git(t, worktree, "add", "-A")
	git(t, worktree, "commit", "-qm", "feature")

	// Now origin/target advances with a CONFLICTING edit to f.txt line1. The
	// worktree's LOCAL origin/target ref is still at base (no fetch yet), so a
	// merge of the local ref would be clean. Only a fresh fetch sees the conflict.
	writeFile(t, upstream, "f.txt", "target-change\ncommon\n")
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-qm", "target advance")
	git(t, upstream, "push", "-q", "origin", "target")

	cc, err := BuildConflictContext(worktree, "target")
	if err != nil {
		t.Fatalf("BuildConflictContext: %v", err)
	}
	if cc.Clean {
		t.Fatal("expected a conflict against the FRESH origin/target; got Clean=true — stale local ref was merged instead of fetched origin ref")
	}
	if len(cc.Files) != 1 || cc.Files[0].Path != "f.txt" {
		t.Fatalf("conflicted files = %+v want [f.txt]", cc.Files)
	}
	// The conflict-marked file must contain the origin-only "target-change" as the
	// incoming side — this only appears if BuildConflictContext fetched and merged
	// origin/target rather than the worktree's stale local target ref.
	if !strings.Contains(cc.Files[0].Merged, "target-change") {
		t.Errorf("Merged should contain the origin-only target-change (proves fresh fetch + merge of origin/target); got:\n%s", cc.Files[0].Merged)
	}
	if !strings.Contains(cc.Files[0].Merged, "<<<<<<<") {
		t.Errorf("Merged should contain conflict markers; got:\n%s", cc.Files[0].Merged)
	}
	if cc.TargetBranch != "target" {
		t.Errorf("TargetBranch should stay the logical name %q, got %q", "target", cc.TargetBranch)
	}
	// TheirsDiff must be computed against mergeRef (origin/target), not the stale
	// local ref. Since the origin-only "target-change" was never fetched locally
	// before BuildConflictContext ran, a non-empty TheirsDiff containing that string
	// proves the diff was taken against the fresh origin ref.
	if !strings.Contains(cc.Files[0].TheirsDiff, "target-change") {
		t.Errorf("TheirsDiff should show origin-only target-change (proves diff uses mergeRef not stale local ref); got:\n%s", cc.Files[0].TheirsDiff)
	}
}

func TestHasConflictMarkers(t *testing.T) {
	if !hasConflictMarkers("a\n<<<<<<< x\nb\n=======\nc\n>>>>>>> y\n") {
		t.Error("should detect markers")
	}
	if hasConflictMarkers("clean\nfile\n") {
		t.Error("clean file should have no markers")
	}
	if hasConflictMarkers("const x = 1;\n") {
		t.Error("ordinary code is not a conflict")
	}
	if !hasConflictMarkers("<<<<<<< HEAD\nx\n") {
		t.Error("first-line marker should be detected")
	}
}

func seedCleanMergeRepo(t *testing.T) string {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "target")
	writeFile(t, dir, "f.txt", "base\ncommon\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "base")
	git(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, dir, "f.txt", "feature-change\ncommon\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "feature")
	git(t, dir, "checkout", "-q", "target")
	// target edits a DIFFERENT file — no overlap with feature's f.txt change
	writeFile(t, dir, "g.txt", "target-only\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "target")
	git(t, dir, "checkout", "-q", "feature")
	return dir
}

func TestBuildConflictContextCleanMerge(t *testing.T) {
	dir := seedCleanMergeRepo(t)
	cc, err := BuildConflictContext(dir, "target")
	if err != nil {
		t.Fatalf("BuildConflictContext: %v", err)
	}
	if !cc.Clean {
		t.Fatal("expected Clean=true for non-overlapping changes")
	}
	if len(cc.Files) != 0 {
		t.Fatalf("expected no conflicted files, got %+v", cc.Files)
	}
}
