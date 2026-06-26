package codeintel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWithRefs builds a repo where main has foo.go (func Alpha) and a branch
// "feat" adds bar.go (func Beta). Returns the repo path.
func repoWithRefs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "t@t.t")
	mustGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package p\nfunc Alpha() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "main: Alpha")
	mustGit(t, dir, "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "bar.go"), []byte("package p\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "feat: Beta")
	mustGit(t, dir, "checkout", "-q", "main") // leave on main
	return dir
}

func TestSearchCode_FindsOnRef(t *testing.T) {
	repo := repoWithRefs(t)
	hits, err := SearchCode(context.Background(), repo, "main", "func Alpha", 50)
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(hits) != 1 || hits[0].File != "foo.go" || hits[0].Line != 2 {
		t.Fatalf("got %+v, want one hit foo.go:2", hits)
	}
}

func TestSearchCode_RespectsRef(t *testing.T) {
	repo := repoWithRefs(t)
	hits, err := SearchCode(context.Background(), repo, "main", "func Beta", 50)
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("Beta should not be on main, got %+v", hits)
	}
	hits, err = SearchCode(context.Background(), repo, "feat", "func Beta", 50)
	if err != nil {
		t.Fatalf("SearchCode on feat: %v", err)
	}
	if len(hits) != 1 || hits[0].File != "bar.go" {
		t.Errorf("Beta should be on feat, got %+v", hits)
	}
}

func TestParseGrep_HandlesColonInPath(t *testing.T) {
	// Synthetic -z record: "<ref>:<file>\x00<lineno>\x00<content>" with a colon in the path.
	out := "main:weird:path.go\x0012\x00func X()\n"
	hits := parseGrep(out, 50)
	if len(hits) != 1 || hits[0].File != "weird:path.go" || hits[0].Line != 12 {
		t.Fatalf("got %+v, want one hit weird:path.go:12", hits)
	}
}

func TestSearchCode_BadRefReturnsError(t *testing.T) {
	repo := repoWithRefs(t)
	if _, err := SearchCode(context.Background(), repo, "no-such-ref-xyz", "func", 50); err == nil {
		t.Fatal("bad ref must return an error")
	}
}

func TestSearchCode_BadRefErrorIncludesStderr(t *testing.T) {
	repo := repoWithRefs(t)
	_, err := SearchCode(context.Background(), repo, "no-such-ref-xyz", "func", 50)
	if err == nil {
		t.Fatal("bad ref must return an error")
	}
	// git writes "fatal: ... no-such-ref-xyz ..." to stderr; it must reach the caller.
	if !strings.Contains(err.Error(), "no-such-ref-xyz") {
		t.Errorf("error should include git's stderr naming the bad ref, got: %v", err)
	}
}

func TestSearchCode_NoMatchIsEmpty(t *testing.T) {
	repo := repoWithRefs(t)
	hits, err := SearchCode(context.Background(), repo, "main", "Nonexistent", 50)
	if err != nil {
		t.Fatalf("no-match must not error: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("want empty, got %+v", hits)
	}
}
