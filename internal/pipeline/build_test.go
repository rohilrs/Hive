package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
)

type fakeAdapter struct {
	scripts   map[string][]*adapter.StageOutput
	calls     []adapter.StageRequest
	reviewErr error
	// tokens and toolCalls are injected into every RunStage response when set.
	tokens    adapter.TokenUsage
	toolCalls []adapter.ToolCallRecord
}

func (f *fakeAdapter) Name() string { return "fake" }
func (f *fakeAdapter) Close() error { return nil }
func (f *fakeAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictChangesRequested}, nil
}

func (f *fakeAdapter) RunStage(_ context.Context, req adapter.StageRequest) (*adapter.StageOutput, error) {
	f.calls = append(f.calls, req)
	if req.StageName == "review" && f.reviewErr != nil {
		return nil, f.reviewErr
	}
	scripts := f.scripts[req.StageName]
	if len(scripts) == 0 {
		return nil, errors.New("no script for " + req.StageName)
	}
	var base *adapter.StageOutput
	if req.Iter < len(scripts) {
		base = scripts[req.Iter]
	} else {
		base = scripts[len(scripts)-1]
	}
	// Merge tokens + toolCalls from fakeAdapter fields into the output.
	out := *base
	out.Tokens = f.tokens
	out.ToolCalls = f.toolCalls
	return &out, nil
}

// fakeStageStore is an in-memory StageStore for testing.
type fakeStageStore struct {
	stages    []fakeStage
	toolCalls map[int64][]ToolCallRecord
	nextID    int64
}

type fakeStage struct {
	id                int64
	runID, name       string
	iter              int
	model             string
	verdict           string
	verdictConfidence *float64
	tokensIn          int
	tokensOut         int
	cacheHit          int
	costUSD           *float64
}

func newFakeStageStore() *fakeStageStore {
	return &fakeStageStore{toolCalls: map[int64][]ToolCallRecord{}}
}

func (f *fakeStageStore) BeginStage(_ context.Context, runID, name string, iter int, model string) (int64, error) {
	f.nextID++
	f.stages = append(f.stages, fakeStage{
		id: f.nextID, runID: runID, name: name, iter: iter, model: model,
	})
	return f.nextID, nil
}

func (f *fakeStageStore) EndStage(_ context.Context, stageID int64, verd string, vc *float64, tIn, tOut, cache int, cost *float64) error {
	for i := range f.stages {
		if f.stages[i].id == stageID {
			f.stages[i].verdict = verd
			f.stages[i].verdictConfidence = vc
			f.stages[i].tokensIn = tIn
			f.stages[i].tokensOut = tOut
			f.stages[i].cacheHit = cache
			f.stages[i].costUSD = cost
			return nil
		}
	}
	return fmt.Errorf("fake: stage %d not found", stageID)
}

func (f *fakeStageStore) PutToolCalls(_ context.Context, _ string, stageID int64, calls []ToolCallRecord) error {
	f.toolCalls[stageID] = append(f.toolCalls[stageID], calls...)
	return nil
}

func makeVerdict(kind adapter.VerdictKind) *adapter.Verdict {
	return &adapter.Verdict{Kind: kind, Confidence: 90, FromTool: true}
}

func newTestRun() *Run {
	repo := "/tmp/repo"
	return &Run{
		ID:           "test-run",
		Task:         &store.Task{ID: "t1", Title: "fix login bug"},
		Project:      &store.Project{ID: "p1", Slug: "auth", RepoPath: &repo},
		WorktreePath: "/tmp/wt",
		RuntimeDir:   "/tmp/hive-run-test-run",
		Pipeline:     "build",
	}
}

func TestBuildApprovesOnFirstIter(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}},
		"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
	}}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  5 * time.Second,
		},
	}
	r, err := b.Run(context.Background(), newTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "done" {
		t.Errorf("status=%s", r.Status)
	}
	if r.Iterations != 1 {
		t.Errorf("iters=%d", r.Iterations)
	}
	if len(adp.calls) != 2 {
		t.Errorf("calls=%d want 2", len(adp.calls))
	}

	for _, c := range adp.calls {
		if !strings.HasPrefix(c.StageDir, "/tmp/hive-run-test-run/stage-0-") {
			t.Errorf("StageDir=%s", c.StageDir)
		}
		if c.Cwd != "/tmp/wt" {
			t.Errorf("Cwd=%s", c.Cwd)
		}
		if c.OriginalRepoPath != "/tmp/repo" {
			t.Errorf("OriginalRepoPath=%s want /tmp/repo", c.OriginalRepoPath)
		}
		if c.RunDir != "/tmp/hive-run-test-run" {
			t.Errorf("RunDir=%s want /tmp/hive-run-test-run", c.RunDir)
		}
	}
}

func TestBuildLoopsOnChangesRequested(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}, {}, {}},
		"review": {
			{Verdict: makeVerdict(adapter.VerdictChangesRequested)},
			{Verdict: makeVerdict(adapter.VerdictChangesRequested)},
			{Verdict: makeVerdict(adapter.VerdictApprove)},
		},
	}}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder: ModelLadder{
				Worker:   []string{"sonnet", "sonnet", "opus"},
				Reviewer: []string{"haiku", "sonnet", "opus"},
			},
			StageTimeout: 5 * time.Second,
		},
	}
	r, err := b.Run(context.Background(), newTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "done" {
		t.Errorf("status=%s", r.Status)
	}
	if r.Iterations != 3 {
		t.Errorf("iters=%d", r.Iterations)
	}

	var iter2Worker string
	for _, c := range adp.calls {
		if c.Iter == 2 && c.StageName == "implement" {
			iter2Worker = c.Model
		}
	}
	if iter2Worker != "opus" {
		t.Errorf("iter 2 worker model=%s want opus", iter2Worker)
	}
}

func TestBuildExhaustsIterationsToNeedsAttention(t *testing.T) {
	cr := makeVerdict(adapter.VerdictChangesRequested)
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}, {}, {}},
		"review":    {{Verdict: cr}, {Verdict: cr}, {Verdict: cr}},
	}}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder: ModelLadder{
				Worker:   []string{"sonnet", "sonnet", "opus"},
				Reviewer: []string{"haiku", "sonnet", "opus"},
			},
			StageTimeout: 5 * time.Second,
		},
	}
	r, err := b.Run(context.Background(), newTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "needs_attention" {
		t.Errorf("status=%s want needs_attention", r.Status)
	}
	if r.Iterations != 3 {
		t.Errorf("iters=%d", r.Iterations)
	}
}

type fakeFeedbackStore struct {
	puts map[string]Feedback
}

func newFakeFeedback() *fakeFeedbackStore {
	return &fakeFeedbackStore{puts: map[string]Feedback{}}
}

func (f *fakeFeedbackStore) Put(_ context.Context, runID string, iter int, fb Feedback) error {
	f.puts[runID+"/"+strconv.Itoa(iter)] = fb
	return nil
}

func (f *fakeFeedbackStore) Get(_ context.Context, runID string, iter int) (Feedback, error) {
	fb, ok := f.puts[runID+"/"+strconv.Itoa(iter)]
	if !ok {
		return Feedback{}, ErrFeedbackNotFound
	}
	return fb, nil
}

func TestBuildPersistsAndReadsFileRefsAcrossIterations(t *testing.T) {
	cr := &adapter.Verdict{
		Kind:       adapter.VerdictChangesRequested,
		Confidence: 80,
		FromTool:   true,
		FileRefs: []verdict.FileRef{
			{Path: "internal/foo.go", Line: 10, Comment: "missing nil check", Reasoning: "panics on empty"},
		},
	}
	ap := &adapter.Verdict{Kind: adapter.VerdictApprove, Confidence: 95, FromTool: true}

	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}, {}},
		"review":    {{Verdict: cr}, {Verdict: ap}},
	}}
	fb := newFakeFeedback()
	b := &BuildPipeline{
		Adapter: adp, Feedback: fb,
		Cfg: BuildConfig{
			MaxIterations: 2,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  5 * time.Second,
		},
	}
	if _, err := b.Run(context.Background(), newTestRun()); err != nil {
		t.Fatal(err)
	}

	persisted, ok := fb.puts["test-run/0"]
	if !ok || len(persisted.FileRefs) != 1 || persisted.FileRefs[0].Path != "internal/foo.go" {
		t.Errorf("iter 0 FileRefs not persisted correctly: %+v", persisted)
	}

	var iter1ImplPrompt string
	for _, c := range adp.calls {
		if c.Iter == 1 && c.StageName == "implement" {
			iter1ImplPrompt = c.SystemPrompt
		}
	}
	if !strings.Contains(iter1ImplPrompt, "internal/foo.go") {
		t.Errorf("iter 1 implement prompt missing FileRef path: %q", iter1ImplPrompt)
	}
	if !strings.Contains(iter1ImplPrompt, "missing nil check") {
		t.Errorf("iter 1 implement prompt missing FileRef comment: %q", iter1ImplPrompt)
	}
}

func TestReviewerPromptRequiresFileRefs(t *testing.T) {
	p := &BuildPipeline{
		Cfg: BuildConfig{Ladder: ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}}},
	}
	prompt := p.reviewPrompt(newTestRun(), 0)
	if !strings.Contains(prompt, "file_refs") {
		t.Errorf("reviewer prompt must mention file_refs; got %q", prompt)
	}
	if !strings.Contains(prompt, "CHANGES_REQUESTED") {
		t.Errorf("reviewer prompt must mention CHANGES_REQUESTED contract")
	}
}

func TestBuildFailsLoudlyOnMissingFileRefs(t *testing.T) {
	adp := &fakeAdapter{
		scripts:   map[string][]*adapter.StageOutput{"implement": {{}}},
		reviewErr: verdict.ErrFileRefsMissing,
	}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  time.Second,
		},
	}
	_, err := b.Run(context.Background(), newTestRun())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, verdict.ErrFileRefsMissing) {
		t.Errorf("expected ErrFileRefsMissing in chain, got %v", err)
	}
}

func TestImplementPromptIncludesPrefetchOnIter0(t *testing.T) {
	prediction := &predictor.Result{
		InlineCapsules: []capsule.Capsule{
			{Target: "func DispatchRun(ctx context.Context, taskID string) error", Raw: "[TARGET]\nfunc DispatchRun(...)\n[BODY]\nfunc DispatchRun(...){...}", TokenEstimate: 120},
		},
		Overflow: []anthropic.Candidate{
			{File: "internal/store/runs.go", Score: 0.55, Reason: "adjacent helper"},
		},
		FullBundlePath: "/tmp/test-run/prefetch.md",
		Metrics:        predictor.Metrics{CandidateCount: 5, InlineCount: 1, OverflowCount: 1},
	}

	run := newTestRun()
	run.Prediction = prediction

	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}},
		"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
	}}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 1,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  time.Second,
		},
	}
	if _, err := b.Run(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	var iter0Implement string
	for _, c := range adp.calls {
		if c.StageName == "implement" && c.Iter == 0 {
			iter0Implement = c.SystemPrompt
		}
	}
	for _, want := range []string{"Pre-fetched context", "func DispatchRun", "/tmp/test-run/prefetch.md", "internal/store/runs.go"} {
		if !strings.Contains(iter0Implement, want) {
			t.Errorf("iter0 implement SystemPrompt missing %q\nFULL:\n%s", want, iter0Implement)
		}
	}

	// Reviewer should NOT see the prefetch.
	var iter0Review string
	for _, c := range adp.calls {
		if c.StageName == "review" && c.Iter == 0 {
			iter0Review = c.SystemPrompt
		}
	}
	if strings.Contains(iter0Review, "Pre-fetched context") {
		t.Errorf("reviewer prompt should NOT include prefetch; got:\n%s", iter0Review)
	}
}

func TestBuildPersistsStagesAndToolCalls(t *testing.T) {
	now := time.Now()
	adp := &fakeAdapter{
		scripts: map[string][]*adapter.StageOutput{
			"implement": {{}},
			"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
		},
		tokens: adapter.TokenUsage{Input: 4321, Output: 789, CacheHit: 120},
		toolCalls: []adapter.ToolCallRecord{
			{Name: "Read", ArgsJSON: []byte(`{"file_path":"a.go"}`), StartedAt: now, EndedAt: now, Success: true},
			{Name: "hive_submit_review_verdict", ArgsJSON: []byte(`{"verdict":"APPROVE"}`), StartedAt: now, EndedAt: now, Success: true},
		},
	}
	stages := newFakeStageStore()
	p := &BuildPipeline{
		Adapter:  adp,
		Feedback: newFakeFeedback(),
		Stages:   stages,
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder: ModelLadder{
				Worker:   []string{"claude-sonnet-4-6"},
				Reviewer: []string{"claude-haiku-4-5"},
			},
			StageTimeout: time.Second,
		},
	}

	res, err := p.Run(context.Background(), &Run{
		ID: "r1", Task: &store.Task{ID: "t1"}, Project: &store.Project{ID: "p"},
		WorktreePath: t.TempDir(), RuntimeDir: t.TempDir(), Pipeline: "build",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done", res.Status)
	}

	// Two stages: implement (iter 0), review (iter 0).
	if len(stages.stages) != 2 {
		t.Fatalf("got %d stages want 2: %+v", len(stages.stages), stages.stages)
	}
	if stages.stages[0].name != "implement" || stages.stages[1].name != "review" {
		t.Errorf("stage order wrong: %+v", stages.stages)
	}
	// Both stages should have token data from the fake adapter.
	if stages.stages[0].tokensIn != 4321 || stages.stages[0].tokensOut != 789 {
		t.Errorf("implement tokens wrong: %+v", stages.stages[0])
	}
	// Review stage should have verdict=APPROVE.
	if stages.stages[1].verdict != "APPROVE" {
		t.Errorf("review verdict=%q want APPROVE", stages.stages[1].verdict)
	}
	// Cost should be non-nil for both (we have pricing for sonnet + haiku).
	if stages.stages[0].costUSD == nil {
		t.Error("implement cost should be non-nil (sonnet is in pricing registry)")
	}
	if stages.stages[1].costUSD == nil {
		t.Error("review cost should be non-nil (haiku is in pricing registry)")
	}

	// Tool calls should be persisted under each stage.
	if len(stages.toolCalls[stages.stages[0].id]) != 2 {
		t.Errorf("implement stage tool calls: got %d want 2", len(stages.toolCalls[stages.stages[0].id]))
	}
}

// TestBuildPersistsStagesUnknownModelNullCost exercises the
// computeCost-returns-nil path: a model not in the pricing registry
// produces a stages row with cost_usd NULL (distinguishable from $0
// in aggregation queries).
func TestBuildPersistsStagesUnknownModelNullCost(t *testing.T) {
	adp := &fakeAdapter{
		scripts: map[string][]*adapter.StageOutput{
			"implement": {{}},
			"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
		},
		tokens:    adapter.TokenUsage{Input: 100, Output: 50, CacheHit: 0},
		toolCalls: nil,
	}
	stages := newFakeStageStore()
	p := &BuildPipeline{
		Adapter:  adp,
		Feedback: newFakeFeedback(),
		Stages:   stages,
		Cfg: BuildConfig{
			MaxIterations: 1,
			Ladder: ModelLadder{
				Worker:   []string{"claude-future-99"},   // not in registry
				Reviewer: []string{"claude-future-99"},   // not in registry
			},
			StageTimeout: time.Second,
		},
	}
	_, err := p.Run(context.Background(), &Run{
		ID: "r1", Task: &store.Task{ID: "t1"}, Project: &store.Project{ID: "p"},
		WorktreePath: t.TempDir(), RuntimeDir: t.TempDir(), Pipeline: "build",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(stages.stages) != 2 {
		t.Fatalf("got %d stages want 2", len(stages.stages))
	}
	for i, s := range stages.stages {
		if s.costUSD != nil {
			t.Errorf("stage %d costUSD=%v want nil (unknown model)", i, s.costUSD)
		}
	}
}

// fakeStallRecorder captures RecordStall calls for assertions.
type fakeStallRecorder struct {
	records []fakeStallRecord
}

type fakeStallRecord struct {
	RunID      string
	StageID    int64
	Layer      int
	DetectedAt int64
	Action     string
	Details    string
}

func (f *fakeStallRecorder) RecordStall(_ context.Context, runID string, stageID int64, layer int, detectedAt int64, action, details string) error {
	f.records = append(f.records, fakeStallRecord{runID, stageID, layer, detectedAt, action, details})
	return nil
}

// alwaysChangesAdapter is a test Adapter that always returns
// CHANGES_REQUESTED with the same FileRefs. Forces the loop detector
// to fire on iter 1+ when paired with a high-similarity fake detector.
type alwaysChangesAdapter struct {
	models []string // record the worker models used per implement stage
}

func (a *alwaysChangesAdapter) Name() string { return "always-changes" }
func (a *alwaysChangesAdapter) Close() error { return nil }
func (a *alwaysChangesAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictChangesRequested}, nil
}
func (a *alwaysChangesAdapter) RunStage(_ context.Context, req adapter.StageRequest) (*adapter.StageOutput, error) {
	if req.StageName == "implement" {
		a.models = append(a.models, req.Model)
		return &adapter.StageOutput{}, nil
	}
	return &adapter.StageOutput{
		Verdict: &adapter.Verdict{
			Kind:     adapter.VerdictChangesRequested,
			FromTool: true,
			FileRefs: []verdict.FileRef{{Path: "stuck.go", Line: 10, Comment: "same comment every time"}},
		},
	}, nil
}

func TestBuildLoopDetectorEscalatesLadderOnHighSimilarity(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	a := &alwaysChangesAdapter{}
	stalls := &fakeStallRecorder{}
	bp := &BuildPipeline{
		Adapter:  a,
		Stalls:   stalls,
		Loop:     &fakeLoopDetector{sim: 0.95},
		Feedback: newFakeFeedback(),
		Cfg: BuildConfig{
			MaxIterations: 5,
			Ladder: ModelLadder{
				Worker:   []string{"m0", "m1", "m2"},
				Reviewer: []string{"r0", "r1", "r2"},
			},
			LoopCheckAfterIter:      1,
			LoopSimilarityThreshold: 0.85,
		},
	}

	res, err := bp.Run(context.Background(), &Run{
		ID: "r", Task: &store.Task{Body: "do thing"},
		WorktreePath: tmp,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	if !strings.Contains(res.Summary, "loop_detected") {
		t.Errorf("Summary=%q missing loop_detected", res.Summary)
	}
	if len(stalls.records) < 2 {
		t.Fatalf("len(records)=%d want >= 2 (escalations + final mark)", len(stalls.records))
	}
	var sawEscalate, sawMark bool
	for _, r := range stalls.records {
		if r.Layer != 3 {
			t.Errorf("record layer=%d want 3", r.Layer)
		}
		if r.Action == "escalated_model" {
			sawEscalate = true
		}
		if r.Action == "marked_needs_attention" {
			sawMark = true
		}
	}
	if !sawEscalate || !sawMark {
		t.Errorf("expected both escalated_model + marked_needs_attention; got %+v", stalls.records)
	}
	// Worker models per implement stage with iter-driven baseline +
	// escalation jump:
	// - iter 0: ladderIdx=max(0,0)=0 → m0. After review, no prevIter
	//   yet (LoopCheckAfterIter=1 requires iter>=1 anyway).
	// - iter 1: ladderIdx=max(0,1)=1 → m1. After review CR, loop
	//   check fires → sim=0.95 → escalate 1→2 → escalated_model row.
	// - iter 2: ladderIdx=max(2,2)=2 → m2. After review CR, loop
	//   check fires → sim=0.95 → escalate 2→2 (already at top) →
	//   marked_needs_attention, bail.
	// Total: 3 implement calls (m0, m1, m2).
	if len(a.models) != 3 {
		t.Errorf("implement called %d times; want 3 (m0, m1, m2). Got models=%v", len(a.models), a.models)
	}
	if len(a.models) >= 3 && (a.models[0] != "m0" || a.models[1] != "m1" || a.models[2] != "m2") {
		t.Errorf("models=%v want [m0 m1 m2]", a.models)
	}
}

func TestBuildLoopDetectorSkipsBelowThreshold(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	a := &alwaysChangesAdapter{}
	stalls := &fakeStallRecorder{}
	bp := &BuildPipeline{
		Adapter:  a,
		Stalls:   stalls,
		Loop:     &fakeLoopDetector{sim: 0.10},
		Feedback: newFakeFeedback(),
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder: ModelLadder{
				Worker:   []string{"m0", "m1", "m2"},
				Reviewer: []string{"r0", "r1", "r2"},
			},
			LoopCheckAfterIter:      1,
			LoopSimilarityThreshold: 0.85,
		},
	}
	_, _ = bp.Run(context.Background(), &Run{ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp})
	if len(stalls.records) != 0 {
		t.Errorf("expected no stall rows; got %+v", stalls.records)
	}
}

func TestBuildLoopDetectorNilDisablesEntirely(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	a := &alwaysChangesAdapter{}
	stalls := &fakeStallRecorder{}
	bp := &BuildPipeline{
		Adapter:  a,
		Stalls:   stalls,
		Loop:     nil,
		Feedback: newFakeFeedback(),
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder: ModelLadder{
				Worker:   []string{"m0", "m1", "m2"},
				Reviewer: []string{"r0", "r1", "r2"},
			},
			LoopCheckAfterIter:      1,
			LoopSimilarityThreshold: 0.85,
		},
	}
	res, _ := bp.Run(context.Background(), &Run{ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp})
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention (exhaustion)", res.Status)
	}
	if strings.Contains(res.Summary, "loop_detected") {
		t.Errorf("Summary should NOT mention loop_detected when Loop=nil; got %q", res.Summary)
	}
}

// approveOnceAdapter is a test Adapter that approves on every review.
// Used to drive the test/validate stages without going through L3 loop
// detection.
type approveOnceAdapter struct{}

func (a *approveOnceAdapter) Name() string { return "approve-once" }
func (a *approveOnceAdapter) Close() error { return nil }
func (a *approveOnceAdapter) ClassifyVerdict(_ context.Context, _ string) (*adapter.Verdict, error) {
	return &adapter.Verdict{Kind: adapter.VerdictApprove}, nil
}
func (a *approveOnceAdapter) RunStage(_ context.Context, req adapter.StageRequest) (*adapter.StageOutput, error) {
	if req.StageName == "review" {
		return &adapter.StageOutput{
			Verdict: &adapter.Verdict{Kind: adapter.VerdictApprove, FromTool: true},
		}, nil
	}
	return &adapter.StageOutput{}, nil
}

func TestBuildTestValidateBothPassDone(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	bp := &BuildPipeline{
		Adapter:  &approveOnceAdapter{},
		Feedback: newFakeFeedback(),
		Cfg: BuildConfig{
			MaxIterations:        2,
			Ladder:               ModelLadder{Worker: []string{"m"}, Reviewer: []string{"r"}},
			TestCommand:          "true",
			ValidateCommand:      "true",
			TestStageTimeout:     3 * time.Second,
			ValidateStageTimeout: 3 * time.Second,
		},
	}
	res, err := bp.Run(context.Background(), &Run{
		ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done", res.Status)
	}
	if !strings.Contains(res.Summary, "approved + tests + validate") {
		t.Errorf("Summary=%q missing 'approved + tests + validate'", res.Summary)
	}
}

func TestBuildTestFailsContinuesNextIter(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	fb := newFakeFeedback()
	bp := &BuildPipeline{
		Adapter:  &approveOnceAdapter{},
		Feedback: fb,
		Cfg: BuildConfig{
			MaxIterations:        2,
			Ladder:               ModelLadder{Worker: []string{"m"}, Reviewer: []string{"r"}},
			TestCommand:          "echo broken; exit 1",
			ValidateCommand:      "true",
			TestStageTimeout:     3 * time.Second,
			ValidateStageTimeout: 3 * time.Second,
		},
	}
	res, _ := bp.Run(context.Background(), &Run{
		ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp,
	})
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	gotFb, err := fb.Get(context.Background(), "r", 0)
	if err != nil {
		t.Fatalf("feedback Get iter 0: %v", err)
	}
	refs := gotFb.FileRefs
	if len(refs) != 1 {
		t.Fatalf("iter 0 refs len=%d want 1", len(refs))
	}
	if refs[0].Path != "(test failures)" {
		t.Errorf("Path=%q want '(test failures)'", refs[0].Path)
	}
	if !strings.Contains(refs[0].Comment, "broken") {
		t.Errorf("Comment=%q missing 'broken'", refs[0].Comment)
	}
}

func TestBuildValidateFailsAfterTestPasses(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	fb := newFakeFeedback()
	bp := &BuildPipeline{
		Adapter:  &approveOnceAdapter{},
		Feedback: fb,
		Cfg: BuildConfig{
			MaxIterations:        1,
			Ladder:               ModelLadder{Worker: []string{"m"}, Reviewer: []string{"r"}},
			TestCommand:          "true",
			ValidateCommand:      "echo vet-fail; exit 2",
			TestStageTimeout:     3 * time.Second,
			ValidateStageTimeout: 3 * time.Second,
		},
	}
	res, _ := bp.Run(context.Background(), &Run{
		ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp,
	})
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
	gotFb2, _ := fb.Get(context.Background(), "r", 0)
	if len(gotFb2.FileRefs) != 1 || gotFb2.FileRefs[0].Path != "(validate failures)" {
		t.Errorf("refs=%+v want one (validate failures)", gotFb2.FileRefs)
	}
}

func TestBuildSkipsEmptyTestCommand(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	bp := &BuildPipeline{
		Adapter:  &approveOnceAdapter{},
		Feedback: newFakeFeedback(),
		Cfg: BuildConfig{
			MaxIterations:        1,
			Ladder:               ModelLadder{Worker: []string{"m"}, Reviewer: []string{"r"}},
			TestCommand:          "",
			ValidateCommand:      "true",
			ValidateStageTimeout: 3 * time.Second,
		},
	}
	res, _ := bp.Run(context.Background(), &Run{
		ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp,
	})
	if res.Status != "done" {
		t.Errorf("Status=%q want done (test skipped, validate passes)", res.Status)
	}
	if !strings.Contains(res.Summary, "approved + validate") {
		t.Errorf("Summary=%q missing 'approved + validate'", res.Summary)
	}
}

func TestBuildSummaryWhenBothShellStagesSkipped(t *testing.T) {
	tmp := setupGitRepo(t, "x", "init\n")
	bp := &BuildPipeline{
		Adapter:  &approveOnceAdapter{},
		Feedback: newFakeFeedback(),
		Cfg: BuildConfig{
			MaxIterations: 1,
			Ladder:        ModelLadder{Worker: []string{"m"}, Reviewer: []string{"r"}},
			// Both empty — fall back to pre-3.4 summary.
		},
	}
	res, _ := bp.Run(context.Background(), &Run{
		ID: "r", Task: &store.Task{Body: "do"}, WorktreePath: tmp,
	})
	if res.Status != "done" {
		t.Errorf("Status=%q want done", res.Status)
	}
	if res.Summary != "approved on iter 0" {
		t.Errorf("Summary=%q want 'approved on iter 0'", res.Summary)
	}
}

func TestBuildRunsDocumenterOnDone(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}},
		"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
		"document":  {{}},
	}}
	p := &BuildPipeline{Adapter: adp, Cfg: BuildConfig{
		MaxIterations:     2,
		Ladder:            ModelLadder{Worker: []string{"m"}, Reviewer: []string{"m"}},
		DocumenterEnabled: true,
		DocumenterModel:   "m",
	}}
	res, err := p.Run(context.Background(), &Run{ID: "r1", Task: &store.Task{ID: "t1", Body: "x"}, Project: &store.Project{ID: "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Fatalf("Status=%q want done", res.Status)
	}
	if res.DocumentationSkipped {
		t.Errorf("DocumentationSkipped=true; want false (documenter succeeded)")
	}
	// The document stage must request the optional submit-documentation tool.
	var docReq *adapter.StageRequest
	for i := range adp.calls {
		if adp.calls[i].StageName == "document" {
			docReq = &adp.calls[i]
			break
		}
	}
	if docReq == nil {
		t.Fatal("no document stage request recorded")
	}
	if docReq.DocToolName != "hive_submit_documentation" {
		t.Errorf("DocToolName=%q want hive_submit_documentation", docReq.DocToolName)
	}
}

func TestBuildDocumenterFailureIsNonBlocking(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}},
		"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
		// no "document" script => fakeAdapter returns an error for that stage.
	}}
	p := &BuildPipeline{Adapter: adp, Cfg: BuildConfig{
		MaxIterations:     2,
		Ladder:            ModelLadder{Worker: []string{"m"}, Reviewer: []string{"m"}},
		DocumenterEnabled: true,
		DocumenterModel:   "m",
	}}
	res, err := p.Run(context.Background(), &Run{ID: "r1", Task: &store.Task{ID: "t1", Body: "x"}, Project: &store.Project{ID: "p1"}})
	if err != nil {
		t.Fatalf("documenter failure must not fail the run: %v", err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (documenter non-blocking)", res.Status)
	}
	if !res.DocumentationSkipped {
		t.Errorf("DocumentationSkipped=false; want true (documenter stage errored)")
	}
}

// TestTaskImplementPrompt verifies the implement-stage user prompt always
// carries the task description (title + body) and is never empty — claude
// --print rejects an empty prompt, and the title is otherwise never sent to
// the worker (the system prompt only gives generic instructions).
func TestTaskImplementPrompt(t *testing.T) {
	cases := []struct {
		name, title, body, want string
	}{
		{"title and body", "Add login", "Use OAuth.", "Add login\n\nUse OAuth."},
		{"empty body", "Add login", "", "Add login"},
		{"whitespace body", "Add login", "  \n ", "Add login"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := taskImplementPrompt(&store.Task{Title: c.title, Body: c.body})
			if got != c.want {
				t.Errorf("taskImplementPrompt(%q,%q) = %q, want %q", c.title, c.body, got, c.want)
			}
			if strings.TrimSpace(got) == "" {
				t.Error("prompt is empty — claude --print would reject it")
			}
		})
	}
}

// TestBuildExhaustSummaryReflectsTestFailure verifies that when review APPROVES
// every iteration but the post-approve test command fails, the terminal summary
// names the test-command failure instead of the misleading "without APPROVE"
// (the review DID approve). Regression guard for the diagnostics fix.
func TestBuildExhaustSummaryReflectsTestFailure(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}, {}, {}},
		"review": {
			{Verdict: makeVerdict(adapter.VerdictApprove)},
			{Verdict: makeVerdict(adapter.VerdictApprove)},
			{Verdict: makeVerdict(adapter.VerdictApprove)},
		},
	}}
	wt := t.TempDir() // real cwd so the shell stage can exec
	run := newTestRun()
	run.WorktreePath = wt
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 3,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  5 * time.Second,
			TestCommand:   "false", // always fails -> never reaches "done"
		},
	}
	r, err := b.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "needs_attention" {
		t.Errorf("status=%s want needs_attention", r.Status)
	}
	if strings.Contains(r.Summary, "without APPROVE") {
		t.Errorf("summary still says 'without APPROVE' despite approvals: %q", r.Summary)
	}
	if !strings.Contains(r.Summary, "test command failed") {
		t.Errorf("summary should name the test-command failure, got: %q", r.Summary)
	}
}

func TestBuildUsesPerRunTestCommand(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}},
		"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
	}}
	wt := t.TempDir()
	run := newTestRun()
	run.WorktreePath = wt
	// Per-run override writes a sentinel; the Cfg default would write a DIFFERENT one.
	marker := filepath.Join(wt, "ran")
	run.Commands = &RunCommands{
		Test:     "echo PROJECT > " + marker,
		Validate: "", // skip
	}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 2,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  5 * time.Second,
			TestCommand:   "echo GLOBAL > " + marker, // must NOT run
		},
	}
	r, err := b.Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "done" {
		t.Errorf("status=%s want done (per-run test passed, validate skipped)", r.Status)
	}
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != "PROJECT" {
		t.Errorf("marker=%q, want PROJECT (per-run command, not Cfg global)", got)
	}
}

func TestImplementPromptRendersSummary(t *testing.T) {
	p := &BuildPipeline{}
	fb := Feedback{
		Summary:  "Overall: the guard regresses the unconfirmed path.",
		FileRefs: []verdict.FileRef{{Path: "a.ts", Line: 5, Comment: "fix X"}},
	}
	out := p.implementPrompt(&Run{Task: &store.Task{}}, 1, fb, nil)
	if !strings.Contains(out, "Previous review summary") || !strings.Contains(out, "regresses the unconfirmed path") {
		t.Errorf("expected the summary block:\n%s", out)
	}
	if !strings.Contains(out, "a.ts") {
		t.Errorf("file refs must still render:\n%s", out)
	}
	// Summary block must appear before the file-ref list.
	if strings.Index(out, "Previous review summary") >= strings.Index(out, "a.ts") {
		t.Errorf("summary block should appear before file-ref list:\n%s", out)
	}
}

// TestImplementPromptSummaryOnly verifies that when feedback carries only a
// summary (no FileRefs), the prompt renders the summary block but does NOT
// emit the "reviewer flagged" header (which would be a dangling header with
// no bullet list beneath it).
func TestImplementPromptSummaryOnly(t *testing.T) {
	p := &BuildPipeline{}
	fb := Feedback{Summary: "just a summary", FileRefs: nil}
	out := p.implementPrompt(&Run{Task: &store.Task{}}, 1, fb, nil)
	if !strings.Contains(out, "Previous review summary") {
		t.Errorf("expected 'Previous review summary' heading:\n%s", out)
	}
	if !strings.Contains(out, "just a summary") {
		t.Errorf("expected summary text:\n%s", out)
	}
	if strings.Contains(out, "reviewer flagged") {
		t.Errorf("must NOT emit 'reviewer flagged' header when FileRefs is empty:\n%s", out)
	}
}

// TestImplementPromptIter0WithLastFailureFeedback verifies that when a task
// carries a non-empty LastFailureFeedback JSON blob, the iter-0 implement
// prompt is prepended with the "previous attempt" block containing the
// summary, file refs, and exhaust reason.
func TestImplementPromptIter0WithLastFailureFeedback(t *testing.T) {
	blob := `{"summary":"nil pointer in auth","file_refs":[{"path":"internal/auth/login.go","line":42,"comment":"missing nil guard","reasoning":"panics on empty token"}],"exhaust_reason":"review never approved within the iteration cap"}`
	task := &store.Task{ID: "t1", Title: "fix login", LastFailureFeedback: blob}
	p := &BuildPipeline{}
	out := p.implementPrompt(&Run{Task: task}, 0, Feedback{}, nil)
	for _, want := range []string{
		"A previous attempt at this task failed",
		"Outcome: review never approved within the iteration cap",
		"nil pointer in auth",
		"internal/auth/login.go:42",
		"missing nil guard",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("iter-0 prompt missing %q\nFULL:\n%s", want, out)
		}
	}
	// The "previous attempt" block must appear BEFORE the base instruction.
	prevIdx := strings.Index(out, "A previous attempt")
	baseIdx := strings.Index(out, "You are implementing")
	if prevIdx < 0 || baseIdx < 0 || prevIdx >= baseIdx {
		t.Errorf("'previous attempt' block must precede base instruction; prevIdx=%d baseIdx=%d\n%s", prevIdx, baseIdx, out)
	}
}

// TestImplementPromptIter0EmptyLastFailureFeedback verifies that when
// LastFailureFeedback is empty (or absent), the iter-0 prompt is unchanged —
// no "previous attempt" block is injected.
func TestImplementPromptIter0EmptyLastFailureFeedback(t *testing.T) {
	task := &store.Task{ID: "t1", Title: "fix login", LastFailureFeedback: ""}
	p := &BuildPipeline{}
	out := p.implementPrompt(&Run{Task: task}, 0, Feedback{}, nil)
	if strings.Contains(out, "A previous attempt") {
		t.Errorf("iter-0 prompt must NOT have 'previous attempt' block when LastFailureFeedback is empty\nFULL:\n%s", out)
	}
	if !strings.Contains(out, "You are implementing") {
		t.Errorf("iter-0 prompt must still contain base instruction\nFULL:\n%s", out)
	}
}

// TestImplementPromptIter0MalformedLastFailureFeedback verifies that
// malformed JSON in LastFailureFeedback silently falls through — the base
// prompt is returned unchanged (no panic, no "previous attempt" block).
func TestImplementPromptIter0MalformedLastFailureFeedback(t *testing.T) {
	task := &store.Task{ID: "t1", Title: "fix login", LastFailureFeedback: "{not valid json"}
	p := &BuildPipeline{}
	out := p.implementPrompt(&Run{Task: task}, 0, Feedback{}, nil)
	if strings.Contains(out, "A previous attempt") {
		t.Errorf("iter-0 prompt must NOT have 'previous attempt' block on malformed JSON\nFULL:\n%s", out)
	}
}

// TestBuildExhaustsPopulatesFinalFeedback verifies that when a build run
// exhausts its iteration cap, Result.FinalFeedback is non-nil and
// Result.ExhaustReason is set.
func TestBuildExhaustsPopulatesFinalFeedback(t *testing.T) {
	fileRef := verdict.FileRef{Path: "internal/foo.go", Line: 10, Comment: "missing guard"}
	cr := &adapter.Verdict{
		Kind:     adapter.VerdictChangesRequested,
		FromTool: true,
		FileRefs: []verdict.FileRef{fileRef},
		Summary:  "needs nil guard everywhere",
	}
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}, {}},
		"review":    {{Verdict: cr}, {Verdict: cr}},
	}}
	fb := newFakeFeedback()
	b := &BuildPipeline{
		Adapter:  adp,
		Feedback: fb,
		Cfg: BuildConfig{
			MaxIterations: 2,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  5 * time.Second,
		},
	}
	r, err := b.Run(context.Background(), newTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "needs_attention" {
		t.Errorf("status=%s want needs_attention", r.Status)
	}
	if r.FinalFeedback == nil {
		t.Fatal("FinalFeedback should be non-nil after exhaustion with feedback")
	}
	if r.FinalFeedback.Summary != "needs nil guard everywhere" {
		t.Errorf("FinalFeedback.Summary=%q", r.FinalFeedback.Summary)
	}
	if len(r.FinalFeedback.FileRefs) != 1 || r.FinalFeedback.FileRefs[0].Path != "internal/foo.go" {
		t.Errorf("FinalFeedback.FileRefs=%+v", r.FinalFeedback.FileRefs)
	}
	if r.ExhaustReason == "" {
		t.Error("ExhaustReason should be non-empty")
	}
}

// TestBuildDoneResultHasNilFinalFeedback verifies that a successful build run
// (status="done") leaves FinalFeedback nil (nothing to carry forward).
func TestBuildDoneResultHasNilFinalFeedback(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"implement": {{}},
		"review":    {{Verdict: makeVerdict(adapter.VerdictApprove)}},
	}}
	b := &BuildPipeline{
		Adapter: adp,
		Cfg: BuildConfig{
			MaxIterations: 2,
			Ladder:        ModelLadder{Worker: []string{"sonnet"}, Reviewer: []string{"haiku"}},
			StageTimeout:  5 * time.Second,
		},
	}
	r, err := b.Run(context.Background(), newTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "done" {
		t.Errorf("status=%s want done", r.Status)
	}
	if r.FinalFeedback != nil {
		t.Errorf("FinalFeedback should be nil on done; got %+v", r.FinalFeedback)
	}
	if r.ExhaustReason != "" {
		t.Errorf("ExhaustReason should be empty on done; got %q", r.ExhaustReason)
	}
}
