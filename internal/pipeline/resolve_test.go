package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/store"
)

// resolveAdapter is a test Adapter whose RunStage runs an arbitrary side
// effect against the worktree (e.g. writing a marker-free file). The
// fakeAdapter in build_test.go is script-driven and cannot mutate the
// worktree, which the resolve loop's no-markers guard requires.
type resolveAdapter struct {
	onRun func(req adapter.StageRequest)
	err   error
	calls int
}

func (a *resolveAdapter) Name() string { return "resolve-fake" }
func (a *resolveAdapter) Close() error { return nil }
func (a *resolveAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictApprove}, nil
}
func (a *resolveAdapter) RunStage(_ context.Context, req adapter.StageRequest) (*adapter.StageOutput, error) {
	a.calls++
	if a.onRun != nil {
		a.onRun(req)
	}
	if a.err != nil {
		return nil, a.err
	}
	return &adapter.StageOutput{}, nil
}

func TestResolvePipelineResolvesAndValidates(t *testing.T) {
	dir := seedConflictRepo(t)
	adp := &resolveAdapter{onRun: func(req adapter.StageRequest) {
		// Overwrite the conflicted file with a marker-free merge of both sides.
		writeFile(t, req.Cwd, "f.txt", "feature-change\ntarget-change\ncommon\n")
	}}
	p := &ResolvePipeline{
		Adapter:  adp,
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations:   3,
			ValidateCommand: "true",
			TestCommand:     "true",
			PushFn:          func(*Run) error { return nil },
		},
	}
	run := &Run{ID: "run-x", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "merge both"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status=%q want done (summary=%q)", res.Status, res.Summary)
	}
	b := osReadFile(dir, "f.txt")
	if hasConflictMarkers(b) {
		t.Error("markers should be gone after resolve")
	}
}

// TestResolvePipelineRecordsSubprocessError is the regression for the Phase-3a
// dogfood blind spot: when the resolve subprocess errors (claude fails to spawn
// or exits before producing output), the loop set lastFail but neither logged
// nor recorded feedback — so a per-iteration subprocess failure "exhausted" with
// ZERO diagnosable trace (#295 died ~0.2s/iter and the cause was lost). The
// error must now be persisted as feedback so the next failure is diagnosable.
func TestResolvePipelineRecordsSubprocessError(t *testing.T) {
	dir := seedConflictRepo(t)
	adp := &resolveAdapter{err: errors.New("claude exited 1 before producing output")}
	fb := newFakeFeedback()
	p := &ResolvePipeline{
		Adapter:  adp,
		Stages:   newFakeStageStore(),
		Feedback: fb,
		Cfg: ResolveConfig{
			MaxIterations:   2,
			ValidateCommand: "true",
			TestCommand:     "true",
			PushFn:          func(*Run) error { return nil },
		},
	}
	run := &Run{ID: "run-suberr", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "x"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "needs_attention" {
		t.Fatalf("status=%q want needs_attention", res.Status)
	}
	if len(fb.puts) == 0 {
		t.Fatal("subprocess error was not recorded as feedback (still swallowed)")
	}
	found := false
	for _, v := range fb.puts {
		if strings.Contains(v.Summary, "resolve subprocess:") && strings.Contains(v.Summary, "claude exited 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("feedback missing the subprocess error detail; got %+v", fb.puts)
	}
}

// TestResolvePipelineSetsStageDir is a regression test for a live-smoke bug:
// the resolve StageRequest must carry a per-stage scratch dir under the run's
// RuntimeDir. The real claudecode adapter writes events.jsonl + materializes
// its MCP/HOME scope there; an empty StageDir made the subprocess error
// immediately (the fake adapter ignores it, so this guards the wiring).
func TestResolvePipelineSetsStageDir(t *testing.T) {
	dir := seedConflictRepo(t)
	rtDir := t.TempDir()
	var gotStageDir, gotRunDir string
	adp := &resolveAdapter{onRun: func(req adapter.StageRequest) {
		gotStageDir, gotRunDir = req.StageDir, req.RunDir
		writeFile(t, req.Cwd, "f.txt", "merged\n")
	}}
	p := &ResolvePipeline{
		Adapter: adp, Stages: newFakeStageStore(), Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{MaxIterations: 1, ValidateCommand: "true", TestCommand: "true", PushFn: func(*Run) error { return nil }},
	}
	run := &Run{ID: "run-sd", WorktreePath: dir, RuntimeDir: rtDir, TargetBranch: "target", Task: &store.Task{Body: "x"}}
	if _, err := p.Run(context.Background(), run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotStageDir == "" {
		t.Error("StageDir must be non-empty on the resolve StageRequest (adapter needs it)")
	}
	if gotRunDir != rtDir {
		t.Errorf("RunDir=%q want %q", gotRunDir, rtDir)
	}
	if !strings.HasPrefix(gotStageDir, rtDir) {
		t.Errorf("StageDir %q should be under RuntimeDir %q", gotStageDir, rtDir)
	}
}

func TestResolvePipelineExhaustsToNeedsAttention(t *testing.T) {
	dir := seedConflictRepo(t)
	// Adapter that never resolves (leaves markers) -> guard fails every iter.
	adp := &resolveAdapter{}
	pushed := false
	p := &ResolvePipeline{
		Adapter:  adp,
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations:   2,
			ValidateCommand: "true",
			TestCommand:     "true",
			PushFn:          func(*Run) error { pushed = true; return nil },
		},
	}
	run := &Run{ID: "run-y", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "x"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "needs_attention" {
		t.Fatalf("status=%q want needs_attention", res.Status)
	}
	if adp.calls != 2 {
		t.Errorf("adapter RunStage calls=%d want 2 (one per iteration)", adp.calls)
	}
	if pushed {
		t.Error("PushFn must not run when the conflict was never resolved")
	}
}

func TestResolvePipelineCleanMergePushes(t *testing.T) {
	dir := seedCleanMergeRepo(t)
	pushed := false
	p := &ResolvePipeline{
		Adapter:  &resolveAdapter{},
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations: 3,
			PushFn:        func(*Run) error { pushed = true; return nil },
		},
	}
	run := &Run{ID: "run-clean", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "x"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status=%q want done", res.Status)
	}
	if !pushed {
		t.Error("clean merge should push")
	}
}

// TestResolveCleanPushSummaryIsNeutral is the regression for the dishonest
// success summary: on the clean-merge path the pipeline pushed locally and
// returned "merge was clean; pushed" — but the daemon then re-checks the PR
// server-side and may park it CONFLICTING. The pipeline summary must NOT claim
// the merge is done; it only locally resolved + pushed and is awaiting the
// daemon's merge confirmation.
func TestResolveCleanPushSummaryIsNeutral(t *testing.T) {
	dir := seedCleanMergeRepo(t)
	p := &ResolvePipeline{
		Adapter:  &resolveAdapter{},
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations: 3,
			PushFn:        func(*Run) error { return nil },
		},
	}
	run := &Run{ID: "run-clean-neutral", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "x"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Fatalf("status=%q want done (summary=%q)", res.Status, res.Summary)
	}
	if strings.Contains(res.Summary, "merge was clean") {
		t.Errorf("summary %q must not claim the merge was clean/done — the daemon confirms the merge", res.Summary)
	}
	if !strings.Contains(res.Summary, "awaiting merge") {
		t.Errorf("summary %q should signal the merge is still awaiting confirmation", res.Summary)
	}
}

func TestResolvePipelineRejectsOutOfScopeEdit(t *testing.T) {
	// Use the auto-merge seed so other.txt is a tracked file in the repo.
	// The agent resolves f.txt (markers gone) but also edits other.txt as an
	// unstaged working-tree change — exactly the realistic agent case.
	// git diff --name-only lists unstaged changes to tracked files, so the guard
	// must still catch this stray edit and reject every iteration.
	dir := seedConflictRepoWithAutoMerge(t)
	adp := &resolveAdapter{onRun: func(req adapter.StageRequest) {
		writeFile(t, req.Cwd, "f.txt", "feature\ntarget\ncommon\n")
		// Stray unstaged edit to a tracked file — not in the conflicted set.
		writeFile(t, req.Cwd, "other.txt", "stray edit to fake the gate\n")
	}}
	pushed := false
	p := &ResolvePipeline{
		Adapter:  adp,
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations:   2,
			ValidateCommand: "true",
			TestCommand:     "true",
			PushFn:          func(*Run) error { pushed = true; return nil },
		},
	}
	run := &Run{ID: "run-scope", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "merge both"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "needs_attention" {
		t.Fatalf("status=%q want needs_attention (stray edit should be rejected every iter)", res.Status)
	}
	if pushed {
		t.Error("PushFn must not run when the agent edited out-of-scope files")
	}
}

// TestResolvePipelineAllowsMergeAutoMergedFiles is a regression test for the
// live bug where outOfScopeEdits used `git status --porcelain` and wrongly
// flagged files that git auto-merged (staged) during BuildConflictContext as
// agent edits — causing every iteration to be rejected, exhausting to
// needs_attention on even a trivial single-file conflict.
//
// The fix: use `git diff --name-only` (unstaged working-tree diff) instead.
// Auto-merged files are STAGED (worktree == index → not listed by git diff),
// while the agent's edits and the resolved conflicted files are UNSTAGED
// (worktree != index → listed by git diff, then excluded by the conflict set).
func TestResolvePipelineAllowsMergeAutoMergedFiles(t *testing.T) {
	// seedConflictRepoWithAutoMerge: merging target→feature conflicts on f.txt
	// AND auto-merges other.txt cleanly (other.txt is staged after the merge).
	dir := seedConflictRepoWithAutoMerge(t)
	// Agent resolves ONLY f.txt; it does NOT touch other.txt.
	adp := &resolveAdapter{onRun: func(req adapter.StageRequest) {
		writeFile(t, req.Cwd, "f.txt", "feature\ntarget\ncommon\n")
	}}
	pushed := false
	p := &ResolvePipeline{
		Adapter:  adp,
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations:   2,
			TestCommand:     "true",
			ValidateCommand: "true",
			PushFn:          func(*Run) error { pushed = true; return nil },
		},
	}
	run := &Run{ID: "run-automerge", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "resolve f.txt"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Before the fix: status --porcelain listed staged other.txt → wrongly
	// flagged as out-of-scope → exhausted to needs_attention.
	// After the fix: git diff --name-only skips staged files → no stray edits
	// detected → pipeline succeeds.
	if res.Status != "done" {
		t.Fatalf("status=%q want done; summary=%q\n(auto-merged other.txt must not be flagged as out-of-scope)", res.Status, res.Summary)
	}
	if !pushed {
		t.Error("PushFn must be called on the green path")
	}
}

func TestResolvePipelineCleanMergeButRedTestsNeedsAttention(t *testing.T) {
	dir := seedCleanMergeRepo(t)
	pushed := false
	p := &ResolvePipeline{
		Adapter:  &resolveAdapter{},
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg: ResolveConfig{
			MaxIterations: 3,
			TestCommand:   "false", // always red
			PushFn:        func(*Run) error { pushed = true; return nil },
		},
	}
	run := &Run{ID: "run-clean-red", WorktreePath: dir, TargetBranch: "target", Task: &store.Task{Body: "x"}}
	res, err := p.Run(context.Background(), run)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "needs_attention" {
		t.Fatalf("status=%q want needs_attention (clean merge with red tests must not push)", res.Status)
	}
	if pushed {
		t.Error("PushFn must not run when a clean merge breaks the tests")
	}
}

func TestResolvePipelineNilGuards(t *testing.T) {
	p := &ResolvePipeline{
		Adapter:  &resolveAdapter{},
		Stages:   newFakeStageStore(),
		Feedback: newFakeFeedback(),
		Cfg:      ResolveConfig{PushFn: func(*Run) error { return nil }},
	}
	// nil Task must not panic.
	res, err := p.Run(context.Background(), &Run{ID: "run-nil", WorktreePath: t.TempDir(), Task: nil})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "needs_attention" {
		t.Fatalf("status=%q want needs_attention", res.Status)
	}
}

func TestUnresolvablePathsFlagsLockfilesAndBinary(t *testing.T) {
	cc := &ConflictContext{Files: []ConflictFile{
		{Path: "src/app.ts", Merged: "<<<<<<<\na\n=======\nb\n>>>>>>>\n"},
		{Path: "pnpm-lock.yaml", Merged: "<<<<<<<\n...\n"},
		{Path: "img.png", Merged: "PNG\x00\x01binary"},
	}}
	bad := unresolvablePaths(cc)
	if len(bad) != 2 {
		t.Fatalf("unresolvable=%v want [pnpm-lock.yaml img.png]", bad)
	}
	wantBad := map[string]bool{"pnpm-lock.yaml": true, "img.png": true}
	for _, pth := range bad {
		if !wantBad[pth] {
			t.Errorf("unexpected unresolvable path %q", pth)
		}
	}
}

func TestResolvePromptRendersConflictAndConstraint(t *testing.T) {
	cc := &ConflictContext{
		TargetBranch: "target",
		Files: []ConflictFile{{
			Path: "f.txt", Merged: "<<<<<<< HEAD\nfeature\n=======\ntarget\n>>>>>>> target\n",
			OursDiff: "+feature", TheirsDiff: "+target",
		}},
	}
	out := resolvePrompt(cc, "Make f say both.", Feedback{})
	// The lean prompt lists the conflicted PATH, references the markers, names
	// the target branch, carries the task, and constrains edits — but it does
	// NOT embed file content/diffs (the agent Reads the files itself).
	for _, want := range []string{"f.txt", "<<<<<<<", "Read", "target", "Make f say both.", "ONLY"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
}

// TestResolvePromptStaysLeanForLargeConflict is the regression for the E2BIG
// instant-fail (#295 / HBA-87): resolvePrompt previously embedded each file's
// full content + both diffs, so a multi-file conflict produced a >128 KiB
// system prompt → claude's `--append-system-prompt` argv exceeded Linux's
// MAX_ARG_STRLEN → `fork/exec: argument list too long` → every iteration died
// before claude started. The prompt must list paths only and stay tiny.
func TestResolvePromptStaysLeanForLargeConflict(t *testing.T) {
	huge := strings.Repeat("x", 200*1024) // 200 KiB of "content" per field
	cc := &ConflictContext{
		TargetBranch: "target",
		Files: []ConflictFile{
			{Path: "a.go", Merged: huge, OursDiff: huge, TheirsDiff: huge},
			{Path: "b.go", Merged: huge, OursDiff: huge, TheirsDiff: huge},
		},
	}
	out := resolvePrompt(cc, "do the thing", Feedback{})
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("prompt must list every conflicted path; got:\n%s", out)
	}
	if strings.Contains(out, huge) {
		t.Error("prompt must NOT embed file content/diffs (E2BIG on large conflicts)")
	}
	if len(out) > 8*1024 {
		t.Errorf("prompt is %d bytes — must stay lean (well under the 128 KiB argv limit)", len(out))
	}
}

func TestResolvePromptIncludesValidationFeedback(t *testing.T) {
	cc := &ConflictContext{TargetBranch: "t", Files: []ConflictFile{{Path: "f.txt"}}}
	out := resolvePrompt(cc, "task", Feedback{Summary: "tests still red", FileRefs: nil})
	if !strings.Contains(out, "tests still red") {
		t.Errorf("prompt should surface prior validation failure:\n%s", out)
	}
}
