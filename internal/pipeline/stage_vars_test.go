package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitWorktree builds a temp git repo with a base commit (tracked.go + apps/backend/base.go).
func gitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t.t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "apps", "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps", "backend", "base.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "base")
	return dir
}

func TestRenderStageCommand_FastPath(t *testing.T) {
	got := renderStageCommand(context.Background(), "pnpm -r test", "main", "/nonexistent")
	if got != "pnpm -r test" {
		t.Errorf("fast path changed cmd: %q", got)
	}
}

func TestRenderStageCommand_TargetBranch(t *testing.T) {
	got := renderStageCommand(context.Background(), "pnpm --filter '...[origin/{{target_branch}}]' test", "chat-test-harness", t.TempDir())
	if !strings.Contains(got, "origin/chat-test-harness") || strings.Contains(got, "{{") {
		t.Errorf("target_branch not substituted: %q", got)
	}
}

func TestRenderStageCommand_ChangedFilesAndDirs(t *testing.T) {
	wt := gitWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "tracked.go"), []byte("package p\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "apps", "backend", "new.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := renderStageCommand(context.Background(), "x {{changed_files}} :: {{changed_dirs}}", "main", wt)
	if !strings.Contains(got, "'apps/backend/new.go'") || !strings.Contains(got, "'tracked.go'") {
		t.Errorf("changed_files wrong: %q", got)
	}
	if !strings.Contains(got, "'apps/backend'") || !strings.Contains(got, "'.'") {
		t.Errorf("changed_dirs wrong: %q", got)
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unsubstituted token remains: %q", got)
	}
}

func TestRenderStageCommand_EmptyDiff(t *testing.T) {
	wt := gitWorktree(t)
	got := renderStageCommand(context.Background(), "echo [{{changed_files}}]", "main", wt)
	if got != "echo []" {
		t.Errorf("empty diff should give empty var: %q", got)
	}
}

func TestRenderStageCommand_QuotesPathsWithSpaces(t *testing.T) {
	wt := gitWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "a b.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := renderStageCommand(context.Background(), "x {{changed_files}}", "main", wt)
	if !strings.Contains(got, "'a b.go'") {
		t.Errorf("path with space not single-quoted: %q", got)
	}
}

func TestJoinQuoted_SingleQuoteInPath(t *testing.T) {
	got := joinQuoted([]string{"it's.go"})
	if got != `'it'\''s.go'` {
		t.Errorf("single-quote in path: got %q", got)
	}
}

func TestRunShellPipelineStage_SubstitutesChangedFiles(t *testing.T) {
	wt := gitWorktree(t)
	if err := os.WriteFile(filepath.Join(wt, "tracked.go"), []byte("package p\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stages/Feedback/Events nil → runShellPipelineStage skips their best-effort calls.
	p := &BuildPipeline{Cfg: BuildConfig{ShellOutputMaxBytes: 8192}}
	run := &Run{ID: "r1", WorktreePath: wt, TargetBranch: "main"}
	// Passes (exit 0) ONLY if {{changed_files}} was substituted to include tracked.go.
	ok, err := p.runShellPipelineStage(context.Background(), run, 1, "test",
		"echo {{changed_files}} | grep -q tracked.go", 0)
	if err != nil {
		t.Fatalf("stage error: %v", err)
	}
	if !ok {
		t.Error("stage failed → {{changed_files}} was not substituted (grep didn't find tracked.go)")
	}
}

func TestRenderStageCommand_NonASCIIPath(t *testing.T) {
	wt := gitWorktree(t) // from this test file
	// A UTF-8 filename: without -z, git would emit "caf\303\251.go" (escaped+quoted).
	if err := os.WriteFile(filepath.Join(wt, "café.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := renderStageCommand(context.Background(), "x {{changed_files}}", "main", wt)
	if !strings.Contains(got, "'café.go'") {
		t.Errorf("non-ASCII path not raw/quoted correctly: %q", got)
	}
	if strings.Contains(got, `\303`) || strings.Contains(got, `"caf`) {
		t.Errorf("path was octal-escaped/double-quoted (quotePath not disabled): %q", got)
	}
}
