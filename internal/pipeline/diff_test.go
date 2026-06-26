package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureWorktreeDiffNoChanges(t *testing.T) {
	dir := setupGitRepo(t, "fileA", "initial\n")
	diff, err := captureWorktreeDiff(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("captureWorktreeDiff: %v", err)
	}
	if strings.TrimSpace(diff) != "" {
		t.Errorf("expected empty diff, got %q", diff)
	}
}

func TestCaptureWorktreeDiffWithEdits(t *testing.T) {
	dir := setupGitRepo(t, "fileA", "initial\n")
	if err := os.WriteFile(filepath.Join(dir, "fileA"), []byte("initial\nadded line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	diff, err := captureWorktreeDiff(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("captureWorktreeDiff: %v", err)
	}
	if !strings.Contains(diff, "added line") {
		t.Errorf("diff missing 'added line': %s", diff)
	}
	if !strings.Contains(diff, "fileA") {
		t.Errorf("diff missing file name: %s", diff)
	}
}

func TestCaptureWorktreeDiffMissingRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := captureWorktreeDiff(context.Background(), dir, "main")
	if err == nil {
		t.Error("expected error for non-git dir")
	}
}

// setupGitRepo creates a temp git repo with one committed file on the
// `main` branch. Returns the repo path.
func setupGitRepo(t *testing.T, filename, content string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
}
