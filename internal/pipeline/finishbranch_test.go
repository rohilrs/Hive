package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFinishBranchAllGatesPass(t *testing.T) {
	p := &FinishBranchPipeline{Stages: newFakeStageStore(), Cfg: FinishBranchConfig{
		TypecheckCommand: "true", LintCommand: "true", TestCommand: "true",
		CreatePRCommand: "echo PR-URL", CIMonitorCommand: "true",
	}}
	res, err := p.Run(context.Background(), &Run{ID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (summary=%q)", res.Status, res.Summary)
	}
}

func TestFinishBranchStopsOnGateFailure(t *testing.T) {
	p := &FinishBranchPipeline{Cfg: FinishBranchConfig{
		TypecheckCommand: "true", LintCommand: "false", TestCommand: "true",
		CreatePRCommand: "echo nope", CIMonitorCommand: "true",
	}}
	res, err := p.Run(context.Background(), &Run{ID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" || !strings.Contains(res.Summary, "lint") {
		t.Errorf("want needs_attention naming lint; got %q / %q", res.Status, res.Summary)
	}
}

func TestFinishBranchEmptyCommandSkips(t *testing.T) {
	// Empty gates are skipped; the pipeline still reaches done.
	p := &FinishBranchPipeline{Cfg: FinishBranchConfig{
		TypecheckCommand: "true", LintCommand: "", TestCommand: "",
		CreatePRCommand: "", CIMonitorCommand: "",
	}}
	res, err := p.Run(context.Background(), &Run{ID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (empty gates skip)", res.Status)
	}
}

func TestFinishBranchUsesPerRunCommands(t *testing.T) {
	// Mirror of TestBuildUsesPerRunTestCommand for the finish-branch side:
	// run.Commands (per-project) must drive the gates, NOT the boot Cfg.
	wt := t.TempDir()
	marker := filepath.Join(wt, "ran")
	p := &FinishBranchPipeline{Stages: newFakeStageStore(), Cfg: FinishBranchConfig{
		// Cfg defaults must NOT run when run.Commands is set.
		TypecheckCommand: "true", LintCommand: "true",
		TestCommand:     "echo GLOBAL > " + marker,
		CreatePRCommand: "echo nope", CIMonitorCommand: "true",
	}}
	run := &Run{ID: "r1", WorktreePath: wt, Commands: &RunCommands{
		Typecheck:  "true",
		Lint:       "", // skip
		FinishTest: "echo PROJECT > " + marker,
		CreatePR:   "echo PR-URL",
		CIMonitor:  "", // skip
	}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (summary=%q)", res.Status, res.Summary)
	}
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != "PROJECT" {
		t.Errorf("marker=%q, want PROJECT (per-run command, not Cfg global)", got)
	}
}

// fakeFixer satisfies SubRunner. touchOnFix=true makes it create the
// sentinel file in the parent worktree (so the next gate run passes) and
// return a "done" child result. Records how many times it was called.
type fakeFixer struct {
	calls      int
	touchOnFix bool
	sentinel   string // path created on fix (relative to worktree)
	childStat  string // status returned in the child Result ("done"/"needs_attention")
}

func (f *fakeFixer) RunChildFix(ctx context.Context, parent *Run, gate, failureOutput string) (*Result, error) {
	f.calls++
	if f.touchOnFix {
		_ = os.WriteFile(filepath.Join(parent.WorktreePath, f.sentinel), []byte("fixed"), 0o644)
	}
	st := f.childStat
	if st == "" {
		st = "done"
	}
	return &Result{Status: st}, nil
}

func TestFinishBranchAutoFixConverges(t *testing.T) {
	wt := t.TempDir()
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  &fakeFixer{touchOnFix: true, sentinel: ".fixed", childStat: "done"},
		Cfg: FinishBranchConfig{
			TypecheckCommand: "true", LintCommand: "true",
			TestCommand:    "test -f .fixed",
			MaxFixAttempts: 2,
		},
	}
	res, err := p.Run(context.Background(), &Run{ID: "r1", WorktreePath: wt})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (summary=%q)", res.Status, res.Summary)
	}
}

func TestFinishBranchAutoFixExhaustsAttempts(t *testing.T) {
	wt := t.TempDir()
	fx := &fakeFixer{touchOnFix: false, childStat: "done"} // never actually fixes
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  fx,
		Cfg: FinishBranchConfig{
			TypecheckCommand: "true", LintCommand: "true",
			TestCommand:    "test -f .fixed",
			MaxFixAttempts: 2,
		},
	}
	res, err := p.Run(context.Background(), &Run{ID: "r1", WorktreePath: wt})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	if fx.calls != 2 {
		t.Errorf("fixer calls=%d want 2 (MaxFixAttempts)", fx.calls)
	}
}

func TestFinishBranchAutoFixSkippedForNonLocalGate(t *testing.T) {
	wt := t.TempDir()
	fx := &fakeFixer{}
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  fx,
		Cfg: FinishBranchConfig{
			TypecheckCommand: "true", LintCommand: "true", TestCommand: "true",
			CreatePRCommand: "false", // fails
			MaxFixAttempts:  2,
		},
	}
	res, _ := p.Run(context.Background(), &Run{ID: "r1", WorktreePath: wt})
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	if fx.calls != 0 {
		t.Errorf("fixer calls=%d want 0 (create-pr is not fixable)", fx.calls)
	}
}

// recordingStageStore wraps fakeStageStore so tests can ask for the iter
// sequence observed for a given stage name (in BeginStage call order).
type recordingStageStore struct {
	fakeStageStore
}

func (r *recordingStageStore) iterSequence(name string) []int {
	var out []int
	for _, s := range r.stages {
		if s.name == name {
			out = append(out, s.iter)
		}
	}
	return out
}

func TestFinishBranchGateRetryIncrementsIter(t *testing.T) {
	// 'test' gate fails first attempt; fakeFixer creates the sentinel
	// .fixed so the SAME gate passes on the second attempt. Stage rows
	// for 'test' should record iter=0 (failed) then iter=1 (passed).
	wt := t.TempDir()
	stages := &recordingStageStore{fakeStageStore: fakeStageStore{toolCalls: map[int64][]ToolCallRecord{}}}
	p := &FinishBranchPipeline{
		Stages: stages,
		Fixer:  &fakeFixer{touchOnFix: true, sentinel: ".fixed", childStat: "done"},
		Cfg: FinishBranchConfig{
			TypecheckCommand: "true", LintCommand: "true",
			TestCommand:    "test -f .fixed",
			MaxFixAttempts: 2,
		},
	}
	res, err := p.Run(context.Background(), &Run{ID: "r1", WorktreePath: wt})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%q want done (summary=%q)", res.Status, res.Summary)
	}
	iters := stages.iterSequence("test")
	if len(iters) != 2 {
		t.Fatalf("BeginStage 'test' calls=%d want 2 (iters=%v)", len(iters), iters)
	}
	if iters[0] != 0 || iters[1] != 1 {
		t.Errorf("iter sequence=%v want [0,1]", iters)
	}
}

func TestFinishBranchAutoFixChildNotDone(t *testing.T) {
	wt := t.TempDir()
	fx := &fakeFixer{touchOnFix: false, childStat: "needs_attention"}
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  fx,
		Cfg: FinishBranchConfig{
			TypecheckCommand: "true", LintCommand: "true",
			TestCommand:    "test -f .fixed", // fails (file never created)
			MaxFixAttempts: 2,
		},
	}
	res, err := p.Run(context.Background(), &Run{ID: "r1", WorktreePath: wt})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	// Child returned a non-"done" status, so we stop after the FIRST attempt
	// rather than exhausting the budget.
	if fx.calls != 1 {
		t.Errorf("fixer calls=%d want 1 (stop on first non-converging child)", fx.calls)
	}
}

func TestFinishBranchCIMonitorAutoFix(t *testing.T) {
	wt := t.TempDir()
	fx := &fakeFixer{touchOnFix: true, sentinel: ".fixed", childStat: "done"}
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  fx,
	}
	run := &Run{ID: "r1", WorktreePath: wt, Commands: &RunCommands{
		Typecheck: "true", CIMonitor: "test -f .fixed",
		AutoFixCI: true, CIFixPushCommand: "true", // "true" = no-op push (no real remote in tests)
	}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (summary=%q)", res.Status, res.Summary)
	}
	if fx.calls != 1 {
		t.Errorf("fixer calls=%d want 1", fx.calls)
	}
}

func TestFinishBranchCIMonitorNotFixableByDefault(t *testing.T) {
	wt := t.TempDir()
	fx := &fakeFixer{touchOnFix: true, sentinel: ".fixed", childStat: "done"}
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  fx,
	}
	run := &Run{ID: "r1", WorktreePath: wt, Commands: &RunCommands{
		Typecheck: "true", CIMonitor: "false",
		AutoFixCI: false,
	}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	if fx.calls != 0 {
		t.Errorf("fixer calls=%d want 0 (ci-monitor not fixable by default)", fx.calls)
	}
}

func TestFinishBranchCIMonitorAutoFixCapAtOne(t *testing.T) {
	wt := t.TempDir()
	fx := &fakeFixer{touchOnFix: false, childStat: "done"} // never actually fixes
	p := &FinishBranchPipeline{
		Stages: newFakeStageStore(),
		Fixer:  fx,
		// MaxFixAttempts deliberately high to prove ci-monitor caps at 1.
		Cfg: FinishBranchConfig{MaxFixAttempts: 5},
	}
	run := &Run{ID: "r1", WorktreePath: wt, Commands: &RunCommands{
		Typecheck: "true", CIMonitor: "test -f .fixed",
		AutoFixCI: true, CIFixPushCommand: "true",
	}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	if fx.calls != 1 {
		t.Errorf("fixer calls=%d want 1 (ci-monitor capped at one, not MaxFixAttempts)", fx.calls)
	}
}

func TestRunShellStageExported(t *testing.T) {
	out, ok, err := RunShellStage(context.Background(), "echo hello", t.TempDir(), 10*time.Second, 8192)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output=%q", out)
	}
}

func TestResolveFinishCommandsIncludesFormat(t *testing.T) {
	cfg := FinishBranchConfig{FormatCommand: "fmt-cmd", TypecheckCommand: "tc", LintCommand: "ln", TestCommand: "ts"}
	got := resolveFinishCommands(cfg, &Run{})
	if got.format != "fmt-cmd" {
		t.Errorf("Format = %q, want fmt-cmd", got.format)
	}
}

func TestSubstituteTargetBranch(t *testing.T) {
	got := substituteTargetBranch("gh pr create --fill --base {{target_branch}}", "staging")
	want := "gh pr create --fill --base staging"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Empty target resolves to main.
	got = substituteTargetBranch("--base {{target_branch}}", "")
	if got != "--base main" {
		t.Errorf("empty: got %q, want --base main", got)
	}
	// No token: command untouched.
	got = substituteTargetBranch("gh pr create --fill", "staging")
	if got != "gh pr create --fill" {
		t.Errorf("no token: got %q", got)
	}
}

func TestParsePRURL(t *testing.T) {
	out := "branch pushed\nhttps://github.com/owner/repo/pull/137\n"
	url, num := parsePRURL(out)
	if url != "https://github.com/owner/repo/pull/137" {
		t.Errorf("url = %q", url)
	}
	if num != 137 {
		t.Errorf("num = %d, want 137", num)
	}
	// No URL in output.
	url, num = parsePRURL("nothing here")
	if url != "" || num != 0 {
		t.Errorf("no-match: got (%q, %d), want (\"\", 0)", url, num)
	}

	// Query-string suffix must NOT be captured.
	url, num = parsePRURL("https://github.com/owner/repo/pull/137?expand=1\n")
	if url != "https://github.com/owner/repo/pull/137" {
		t.Errorf("query-string: url = %q, want no query suffix", url)
	}
	if num != 137 {
		t.Errorf("query-string: num = %d, want 137", num)
	}

	// URL embedded mid-line with trailing text.
	url, num = parsePRURL("Created PR https://github.com/owner/repo/pull/88 (draft)\n")
	if url != "https://github.com/owner/repo/pull/88" {
		t.Errorf("mid-line: url = %q, want https://github.com/owner/repo/pull/88", url)
	}
	if num != 88 {
		t.Errorf("mid-line: num = %d, want 88", num)
	}

	// Multiple PR URLs on separate lines: return the FIRST one.
	url, num = parsePRURL("https://github.com/owner/repo/pull/10\nhttps://github.com/owner/repo/pull/20\n")
	if url != "https://github.com/owner/repo/pull/10" {
		t.Errorf("multi-url: url = %q, want first PR URL", url)
	}
	if num != 10 {
		t.Errorf("multi-url: num = %d, want 10", num)
	}
}
