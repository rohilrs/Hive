package codeintel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildContext_BoundsSnippets(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "t@t.t")
	mustGit(t, dir, "config", "user.name", "t")
	// A single very long line containing the CamelCase term BigThing.
	long := "BigThing " + strings.Repeat("x", 5000) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "big")

	g := NewGrounder(dir, "main", "", "scavenger", false, 0)
	out := BuildContext(context.Background(), g, "extend `BigThing`")
	if out == "" {
		t.Fatal("expected a context block")
	}
	if len(out) > contextMaxBytes+contextSnippetMax+1024 {
		t.Errorf("output not bounded: %d bytes", len(out))
	}
	if strings.Contains(out, strings.Repeat("x", 1000)) {
		t.Errorf("oversized snippet not truncated")
	}
}

func TestExtractTerms(t *testing.T) {
	src := "Build `ProfileRepo` in `storage/profile_repo.py`; call `get(user_id)`. Also runReconciliationChecks and plain words like the and user."
	terms := extractTerms(src, 12)
	has := func(w string) bool {
		for _, x := range terms {
			if x == w {
				return true
			}
		}
		return false
	}
	if !has("ProfileRepo") || !has("storage/profile_repo.py") || !has("runReconciliationChecks") {
		t.Errorf("missing expected terms in %v", terms)
	}
	if has("get(user_id)") {
		t.Errorf("regex-unsafe term not filtered: %v", terms)
	}
	if has("the") || has("user") {
		t.Errorf("prose word leaked into terms: %v", terms)
	}
}

func TestBuildContext_NilGrounderEmpty(t *testing.T) {
	if got := BuildContext(context.Background(), nil, "anything `Foo`"); got != "" {
		t.Errorf("nil grounder should give empty context, got %q", got)
	}
}

func TestBuildContext_GroundsAgainstRef(t *testing.T) {
	repo := repoWithRefs(t)
	g := NewGrounder(repo, "main", "", "scavenger", false, 0) // scavenger disabled → search-only
	out := BuildContext(context.Background(), g, "We will extend `Alpha` in foo.go.")
	if out == "" {
		t.Fatal("expected a context block")
	}
	if !strings.Contains(out, "foo.go") || !strings.Contains(out, "CODEBASE CONTEXT") {
		t.Errorf("context block missing hit/header:\n%s", out)
	}
	if !strings.Contains(out, "main") {
		t.Errorf("context block should name the grounding ref:\n%s", out)
	}
}

func TestBuildContext_NoHitsEmpty(t *testing.T) {
	repo := repoWithRefs(t)
	g := NewGrounder(repo, "main", "", "scavenger", false, 0)
	if got := BuildContext(context.Background(), g, "Nothing `Nonexistent` here."); got != "" {
		t.Errorf("no hits should give empty context, got %q", got)
	}
}
