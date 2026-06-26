package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func findItem(items []SourceItem, sourceID string) *SourceItem {
	for i := range items {
		if items[i].SourceID == sourceID {
			return &items[i]
		}
	}
	return nil
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestInboxFetch(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "proj1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeFile(t, filepath.Join(projDir, "a.md"), "# Add retry\n\nbody text")
	writeFile(t, filepath.Join(projDir, "b.md"), "---\ntitle: Custom Title\npipeline: plan\npriority: P1\nlabels: backend, infra\n---\nBody of b.")

	s := &InboxSource{Root: root}
	items, err := s.Fetch(context.Background(), "proj1", nil)
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d: %+v", len(items), items)
	}

	a := findItem(items, "a.md")
	if a == nil {
		t.Fatalf("item a.md not found in %+v", items)
	}
	if a.Title != "Add retry" {
		t.Errorf("a.md Title = %q, want %q (from # heading)", a.Title, "Add retry")
	}
	if a.State != "open" {
		t.Errorf("a.md State = %q, want %q", a.State, "open")
	}
	if !strings.Contains(a.Body, "body text") {
		t.Errorf("a.md Body = %q, want it to contain %q", a.Body, "body text")
	}

	b := findItem(items, "b.md")
	if b == nil {
		t.Fatalf("item b.md not found in %+v", items)
	}
	if b.Title != "Custom Title" {
		t.Errorf("b.md Title = %q, want %q", b.Title, "Custom Title")
	}
	if b.Priority != "P1" {
		t.Errorf("b.md Priority = %q, want %q", b.Priority, "P1")
	}
	if b.State != "open" {
		t.Errorf("b.md State = %q, want %q", b.State, "open")
	}
	if !containsStr(b.Labels, "hive:plan") {
		t.Errorf("b.md Labels = %v, want it to contain %q", b.Labels, "hive:plan")
	}
	if !containsStr(b.Labels, "backend") {
		t.Errorf("b.md Labels = %v, want it to contain %q", b.Labels, "backend")
	}
	if !containsStr(b.Labels, "infra") {
		t.Errorf("b.md Labels = %v, want it to contain %q", b.Labels, "infra")
	}
	if b.Body != "Body of b." {
		t.Errorf("b.md Body = %q, want %q (frontmatter stripped)", b.Body, "Body of b.")
	}
}

func TestInboxFetchMissingDir(t *testing.T) {
	s := &InboxSource{Root: t.TempDir()}
	items, err := s.Fetch(context.Background(), "nonexistent", nil)
	if err != nil {
		t.Fatalf("Fetch returned error for missing dir: %v", err)
	}
	if items != nil {
		t.Errorf("expected nil items for missing dir, got %+v", items)
	}
}
