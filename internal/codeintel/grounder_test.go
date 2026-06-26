package codeintel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

// fakeFetcher returns a canned capsule and records the cwd it was called with.
type fakeFetcher struct {
	lastCwd string
	cap     *capsule.Capsule
}

func (f *fakeFetcher) Fetch(ctx context.Context, req capsule.Req) (*capsule.Capsule, error) {
	f.lastCwd = req.Cwd
	return f.cap, nil
}

func TestGrounder_CapsuleIndexesOnceAndFetches(t *testing.T) {
	repo := repoWithRefs(t) // from search_test.go
	groundDir := filepath.Join(t.TempDir(), "ground")

	indexCalls := 0
	ff := &fakeFetcher{cap: &capsule.Capsule{Target: "Alpha", Callers: "x"}}
	g := NewGrounder(repo, "main", groundDir, "scavenger", true, 0)
	g.fetcher = ff
	g.indexFn = func(ctx context.Context, dir string) error { indexCalls++; return nil }

	c, err := g.Capsule(context.Background(), "foo.go", "Alpha")
	if err != nil {
		t.Fatalf("Capsule: %v", err)
	}
	if c.Target != "Alpha" {
		t.Errorf("capsule Target = %q", c.Target)
	}
	if _, err := os.Stat(filepath.Join(groundDir, ".git")); err != nil {
		t.Errorf("grounding worktree not created: %v", err)
	}
	if ff.lastCwd != groundDir {
		t.Errorf("fetcher cwd = %q, want %q", ff.lastCwd, groundDir)
	}
	if _, err := g.Capsule(context.Background(), "foo.go", "Alpha"); err != nil {
		t.Fatalf("second Capsule: %v", err)
	}
	if indexCalls != 1 {
		t.Errorf("indexFn called %d times, want 1 (cached on unchanged SHA)", indexCalls)
	}
}

func TestGrounder_CapsuleDisabledReturnsUnavailable(t *testing.T) {
	repo := repoWithRefs(t)
	g := NewGrounder(repo, "main", filepath.Join(t.TempDir(), "g"), "scavenger", false, 0)
	_, err := g.Capsule(context.Background(), "foo.go", "Alpha")
	if !errors.Is(err, ErrScavengerUnavailable) {
		t.Fatalf("want ErrScavengerUnavailable, got %v", err)
	}
}

func TestGrounder_SearchDelegates(t *testing.T) {
	repo := repoWithRefs(t)
	g := NewGrounder(repo, "main", "", "scavenger", true, 0)
	hits, err := g.Search(context.Background(), "func Alpha", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search via grounder: hits=%+v err=%v", hits, err)
	}
}
