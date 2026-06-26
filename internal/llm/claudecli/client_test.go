package claudecli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
)

var (
	fakeClaudeOnce sync.Once
	fakeClaudePath string
	fakeClaudeErr  error
)

// buildFakeClaude builds scripts/fake-claude once per test process and
// returns the cached path. Mirrors the sync.Once pattern used in
// internal/adapter/claudecode/subprocess_test.go.
func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fake-claude-*")
		if err != nil {
			fakeClaudeErr = err
			return
		}
		out := filepath.Join(dir, "fake-claude")
		cmd := exec.Command("go", "build", "-o", out, "../../../scripts/fake-claude")
		if b, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeErr = err
			t.Logf("build fake-claude output: %s", b)
			return
		}
		fakeClaudePath = out
	})
	if fakeClaudeErr != nil {
		t.Fatalf("buildFakeClaude: %v", fakeClaudeErr)
	}
	return fakeClaudePath
}

func TestRunAndExtractTextEmpty(t *testing.T) {
	bin := buildFakeClaude(t)
	fixture := filepath.Join("testdata", "empty_response.jsonl")
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", fixture}})
	got, err := c.runAndExtractText(context.Background(), "haiku", "system prompt", "user prompt", 5*time.Second)
	if err != nil {
		t.Fatalf("runAndExtractText: %v", err)
	}
	if got != "" {
		t.Errorf("got=%q want empty", got)
	}
}

// TestRunAndExtractTextPassesNoToolsFlag verifies the claude subprocess
// is invoked with --tools "" so Haiku can't invoke Edit/Write/Bash and
// silently mutate the daemon's cwd (the user's main repo). Regression
// for the smoke-test failure on 2026-05-22 where the predictor
// repeatedly mutated source files instead of returning JSON.
func TestRunAndExtractTextPassesNoToolsFlag(t *testing.T) {
	bin := buildFakeClaude(t)
	fixture := filepath.Join("testdata", "empty_response.jsonl")
	// Run in a tempdir so the .fake-claude-argv.json doesn't collide
	// with other tests.
	tmp := t.TempDir()
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	// fixture path was relative to original cwd; resolve absolutely.
	absFixture := filepath.Join(cwd, fixture)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", absFixture}})
	if _, err := c.runAndExtractText(context.Background(), "haiku", "sys", "user", 5*time.Second); err != nil {
		t.Fatalf("runAndExtractText: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, ".fake-claude-argv.json"))
	if err != nil {
		t.Fatalf("read argv json: %v", err)
	}
	if !strings.Contains(string(raw), `"--tools"`) {
		t.Errorf("subprocess argv missing --tools flag: %s", raw)
	}
}

func TestRunAndExtractTextConcatenatesAssistantBlocks(t *testing.T) {
	bin := buildFakeClaude(t)
	fixture := filepath.Join("testdata", "multi_block_response.jsonl")
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", fixture}})
	got, err := c.runAndExtractText(context.Background(), "haiku", "sys", "user", 5*time.Second)
	if err != nil {
		t.Fatalf("runAndExtractText: %v", err)
	}
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("got=%q want both 'first' and 'second' concatenated", got)
	}
}

func TestClassifyVerdictApprove(t *testing.T) {
	bin := buildFakeClaude(t)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", "testdata/classify_approve.jsonl"}})
	got, err := c.ClassifyVerdict(context.Background(), "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "APPROVE" {
		t.Errorf("got.Verdict=%q want APPROVE", got.Verdict)
	}
	if got.Confidence != 92 {
		t.Errorf("got.Confidence=%d want 92", got.Confidence)
	}
}

func TestClassifyVerdictUnclearFallsBack(t *testing.T) {
	bin := buildFakeClaude(t)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", "testdata/classify_unclear.jsonl"}})
	got, err := c.ClassifyVerdict(context.Background(), "ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "CHANGES_REQUESTED" {
		t.Errorf("UNCLEAR should fail-safe to CHANGES_REQUESTED; got %q", got.Verdict)
	}
}

func TestClassifyVerdictGarbageFallsBack(t *testing.T) {
	bin := buildFakeClaude(t)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", "testdata/classify_garbage.jsonl"}})
	got, err := c.ClassifyVerdict(context.Background(), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "CHANGES_REQUESTED" {
		t.Errorf("unparseable should fail-safe to CHANGES_REQUESTED; got %q", got.Verdict)
	}
	if got.Confidence != 0 {
		t.Errorf("Confidence=%d want 0", got.Confidence)
	}
}

func TestPredictFilesReturnsRankedCandidates(t *testing.T) {
	bin := buildFakeClaude(t)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", "testdata/predict_ok.jsonl"}})
	got, err := c.PredictFiles(context.Background(), anthropic.PredictionRequest{
		Task:          "fix dispatch race",
		RepoFiles:     []string{"a.go", "b.go", "c.go"},
		MaxCandidates: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d want 2", len(got))
	}
	if got[0].File != "a.go" || got[0].Symbol != "Run" {
		t.Errorf("got[0]=%+v want a.go:Run", got[0])
	}
	if got[0].Score < got[1].Score {
		t.Errorf("expected ranked descending; got[0]=%v got[1]=%v", got[0].Score, got[1].Score)
	}
}

func TestPredictFilesGarbageReturnsEmpty(t *testing.T) {
	bin := buildFakeClaude(t)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", "testdata/predict_garbage.jsonl"}})
	got, err := c.PredictFiles(context.Background(), anthropic.PredictionRequest{
		Task: "x", RepoFiles: []string{"a.go"}, MaxCandidates: 5,
	})
	if err != nil {
		t.Fatalf("garbage should NOT error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got)=%d want 0 (fail-safe)", len(got))
	}
}

func TestStripCodeFence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no fence", `{"x":1}`, `{"x":1}`},
		{"json fence", "```json\n{\"x\":1}\n```", `{"x":1}`},
		{"bare fence", "```\n{\"x\":1}\n```", `{"x":1}`},
		{"fence with surrounding whitespace", "\n\n```json\n{\"x\":1}\n```\n\n", `{"x":1}`},
		{"multiline content in fence", "```\nline1\nline2\n```", "line1\nline2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripCodeFence(tc.in)
			if got != tc.want {
				t.Errorf("stripCodeFence(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPredictFilesStripsCodeFence(t *testing.T) {
	bin := buildFakeClaude(t)
	c := NewClient(Config{Binary: bin, ExtraArgs: []string{"-fixture", "testdata/predict_fenced.jsonl"}})
	got, err := c.PredictFiles(context.Background(), anthropic.PredictionRequest{
		Task: "x", RepoFiles: []string{"a.go"}, MaxCandidates: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "a.go" {
		t.Errorf("expected 1 candidate (a.go) after fence-strip; got %+v", got)
	}
}

// silence unused import if needed
var _ = anthropic.VerdictResult{}
