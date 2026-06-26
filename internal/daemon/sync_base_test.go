package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localBehind returns how many commits local <base> is behind origin/<base>.
func localBehind(t *testing.T, repo, base string) string {
	t.Helper()
	return strings.TrimSpace(mustGit(t, repo, "rev-list", "--count", base+".."+"origin/"+base))
}

// setupBehindRepo builds a bare origin + clone repoA, pushes base "feat", then a
// second clone advances origin/feat by one real commit. repoA is left checked
// out on "feat" (now one commit behind origin) but has NOT fetched yet.
func setupBehindRepo(t *testing.T) (origin, repoA string) {
	t.Helper()
	origin = t.TempDir()
	mustGit(t, "", "init", "-q", "--bare", "-b", "main", origin)
	repoA = t.TempDir()
	mustGit(t, "", "clone", "-q", origin, repoA)
	writeF(t, repoA, "f.txt", "base\n")
	// A tracked roadmap at the conventional path — present on the base commit so
	// origin/feat carries it too (matches the reconciler rewriting a tracked file).
	mkRoadmap(t, repoA, "someslug", "# roadmap (committed baseline)\n")
	mustGit(t, repoA, "add", "-A")
	mustGit(t, repoA, "commit", "-qm", "base")
	mustGit(t, repoA, "branch", "feat")
	mustGit(t, repoA, "push", "-q", "origin", "main", "feat")

	// Second clone advances origin/feat with a real commit.
	repoB := t.TempDir()
	mustGit(t, "", "clone", "-q", origin, repoB)
	mustGit(t, repoB, "checkout", "-q", "feat")
	writeF(t, repoB, "f.txt", "advanced on origin\n")
	mustGit(t, repoB, "add", "-A")
	mustGit(t, repoB, "commit", "-qm", "origin advance")
	mustGit(t, repoB, "push", "-q", "origin", "feat")

	// repoA: be on feat (currently behind origin/feat once it fetches).
	mustGit(t, repoA, "checkout", "-q", "feat")
	return origin, repoA
}

func TestSyncLocalBaseFastForwardsWhenBehind(t *testing.T) {
	_, repoA := setupBehindRepo(t)
	d := newSyncTestDaemon(t)

	d.syncLocalBaseAfterMerge(repoA, "feat", "someslug")

	if got := localBehind(t, repoA, "feat"); got != "0" {
		t.Fatalf("local feat should be 0 behind origin after FF, got %q behind", got)
	}
}

func TestSyncLocalBaseStashesReconcilerRoadmap(t *testing.T) {
	_, repoA := setupBehindRepo(t)
	d := newSyncTestDaemon(t)

	// The reconciler rewrites a TRACKED roadmap file every loop, leaving it
	// perpetually modified. Reproduce that: the file already exists on the base
	// commit, so origin/feat carries it too; then dirty it locally.
	mkRoadmap(t, repoA, "someslug", "# roadmap (uncommitted reconciler churn)\n")

	d.syncLocalBaseAfterMerge(repoA, "feat", "someslug")

	if got := localBehind(t, repoA, "feat"); got != "0" {
		t.Fatalf("FF should still succeed past the reconciler roadmap; got %q behind", got)
	}
}

func TestSyncLocalBaseSkipsOnOtherDirtyFile(t *testing.T) {
	_, repoA := setupBehindRepo(t)
	d := newSyncTestDaemon(t)

	// A dirty NON-roadmap file: the user's real work — must not be disturbed.
	writeF(t, repoA, "src.txt", "user work in progress\n")

	d.syncLocalBaseAfterMerge(repoA, "feat", "someslug")

	if got := localBehind(t, repoA, "feat"); got == "0" {
		t.Fatalf("FF must be SKIPPED when a non-roadmap file is dirty; feat was advanced anyway")
	}
}

func TestSyncLocalBaseSkipsWhenDiverged(t *testing.T) {
	_, repoA := setupBehindRepo(t)
	d := newSyncTestDaemon(t)

	// Give repoA a local-only commit on feat (ahead) while origin is ahead too
	// (behind) => diverged. The sync must not move local.
	writeF(t, repoA, "local.txt", "local only\n")
	mustGit(t, repoA, "add", "-A")
	mustGit(t, repoA, "commit", "-qm", "local-only commit")

	before := strings.TrimSpace(mustGit(t, repoA, "rev-parse", "feat"))

	d.syncLocalBaseAfterMerge(repoA, "feat", "someslug")

	after := strings.TrimSpace(mustGit(t, repoA, "rev-parse", "feat"))
	if before != after {
		t.Fatalf("diverged feat must NOT move: before=%s after=%s", before, after)
	}
}

func TestDirtyPathsExcluding(t *testing.T) {
	repo := t.TempDir()
	mustGit(t, "", "init", "-q", "-b", "main", repo)
	writeF(t, repo, "f.txt", "base\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-qm", "base")

	// Two dirty files.
	writeF(t, repo, "keep.txt", "dirty A\n")
	writeF(t, repo, "exclude.txt", "dirty B\n")

	got := dirtyPathsExcluding(repo, "exclude.txt")
	if len(got) != 1 || got[0] != "keep.txt" {
		t.Fatalf("dirtyPathsExcluding = %v, want [keep.txt]", got)
	}
}

// mkRoadmap writes the reconciler-owned roadmap file at the conventional path,
// creating parent dirs.
func mkRoadmap(t *testing.T, repo, slug, content string) {
	t.Helper()
	dir := filepath.Join(repo, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
