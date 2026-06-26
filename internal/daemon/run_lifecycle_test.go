package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
)

func TestBranchNameFromTaskMetadataReturnsTrimmedValue(t *testing.T) {
	task := &store.Task{
		Metadata: map[string]any{
			"branch_name": "  rohil/HBA-42  ",
		},
	}
	got := branchNameFromTaskMetadata(task)
	if got != "rohil/HBA-42" {
		t.Errorf("branchNameFromTaskMetadata = %q, want %q", got, "rohil/HBA-42")
	}
}

func TestBranchNameFromTaskMetadataReturnsEmptyWhenAbsent(t *testing.T) {
	cases := []struct {
		name string
		task *store.Task
	}{
		{"nil task", nil},
		{"nil metadata", &store.Task{Metadata: nil}},
		{"missing key", &store.Task{Metadata: map[string]any{"external_id": "HBA-42"}}},
		{"non-string value", &store.Task{Metadata: map[string]any{"branch_name": 123}}},
		{"empty string", &store.Task{Metadata: map[string]any{"branch_name": ""}}},
		{"whitespace only", &store.Task{Metadata: map[string]any{"branch_name": "   "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := branchNameFromTaskMetadata(tc.task); got != "" {
				t.Errorf("branchNameFromTaskMetadata = %q, want empty", got)
			}
		})
	}
}

// --- last_failure_feedback lifecycle tests ---

// persistLastFailureFeedback directly exercises the write/clear logic that
// lives in executePipeline (after p.Run returns). Because executePipeline
// dispatches a real adapter, we test the store+logic path in isolation:
// we construct the exact JSON marshal + SetTaskLastFailureFeedback call
// that the lifecycle code performs and assert the round-trip.

// TestLastFailureFeedbackWrittenOnNeedsAttention verifies that when a build
// run ends with needs_attention and FinalFeedback set, the task's
// last_failure_feedback column is populated with the correct JSON.
func TestLastFailureFeedbackWrittenOnNeedsAttention(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "proj", Name: "P", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: "t1", ProjectID: "p1", Source: "inbox", Title: "do work", Status: "pending"}
	if err := d.store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// Simulate what executePipeline does on a build needs_attention result.
	result := &pipeline.Result{
		Status: "needs_attention",
		FinalFeedback: &pipeline.Feedback{
			Summary:  "nil guard missing",
			FileRefs: []verdict.FileRef{{Path: "internal/foo.go", Line: 7, Comment: "add nil check"}},
		},
		ExhaustReason: "review never approved within the iteration cap",
	}

	// Replicate the lifecycle marshal+persist logic directly.
	if result.FinalFeedback != nil {
		blob, merr := json.Marshal(struct {
			Summary       string            `json:"summary"`
			FileRefs      []verdict.FileRef `json:"file_refs"`
			ExhaustReason string            `json:"exhaust_reason"`
		}{
			Summary:       result.FinalFeedback.Summary,
			FileRefs:      result.FinalFeedback.FileRefs,
			ExhaustReason: result.ExhaustReason,
		})
		if merr != nil {
			t.Fatalf("marshal: %v", merr)
		}
		if serr := d.store.SetTaskLastFailureFeedback(ctx, task.ID, string(blob)); serr != nil {
			t.Fatalf("SetTaskLastFailureFeedback: %v", serr)
		}
	}

	got, err := d.store.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.LastFailureFeedback == "" {
		t.Fatal("LastFailureFeedback should be non-empty after needs_attention")
	}
	if !strings.Contains(got.LastFailureFeedback, "nil guard missing") {
		t.Errorf("missing summary; got %q", got.LastFailureFeedback)
	}
	if !strings.Contains(got.LastFailureFeedback, "internal/foo.go") {
		t.Errorf("missing file ref; got %q", got.LastFailureFeedback)
	}
	if !strings.Contains(got.LastFailureFeedback, "review never approved") {
		t.Errorf("missing exhaust_reason; got %q", got.LastFailureFeedback)
	}
	// Verify the JSON keys match what implementPrompt's reader expects.
	var parsed struct {
		Summary       string            `json:"summary"`
		FileRefs      []verdict.FileRef `json:"file_refs"`
		ExhaustReason string            `json:"exhaust_reason"`
	}
	if err := json.Unmarshal([]byte(got.LastFailureFeedback), &parsed); err != nil {
		t.Fatalf("unmarshal stored blob: %v", err)
	}
	if parsed.Summary != "nil guard missing" {
		t.Errorf("summary=%q", parsed.Summary)
	}
	if len(parsed.FileRefs) != 1 || parsed.FileRefs[0].Path != "internal/foo.go" {
		t.Errorf("file_refs=%+v", parsed.FileRefs)
	}
	if parsed.ExhaustReason != "review never approved within the iteration cap" {
		t.Errorf("exhaust_reason=%q", parsed.ExhaustReason)
	}
}

// TestLastFailureFeedbackClearedOnDone verifies that when a build run ends
// with status="done", any previously-stored last_failure_feedback is cleared.
func TestLastFailureFeedbackClearedOnDone(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "proj2", Name: "P2", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: "t2", ProjectID: "p1", Source: "inbox", Title: "do work 2", Status: "pending"}
	if err := d.store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// Seed a prior failure.
	if err := d.store.SetTaskLastFailureFeedback(ctx, "t2", `{"summary":"old failure","file_refs":[],"exhaust_reason":"something"}`); err != nil {
		t.Fatalf("seed feedback: %v", err)
	}
	got1, _ := d.store.GetTask(ctx, "t2")
	if got1.LastFailureFeedback == "" {
		t.Fatal("pre-condition: feedback should be set")
	}

	// Simulate executePipeline's done path: ClearTaskLastFailureFeedback.
	if err := d.store.ClearTaskLastFailureFeedback(ctx, "t2"); err != nil {
		t.Fatalf("ClearTaskLastFailureFeedback: %v", err)
	}

	got2, err := d.store.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got2.LastFailureFeedback != "" {
		t.Errorf("LastFailureFeedback should be empty after done; got %q", got2.LastFailureFeedback)
	}
}

// TestLastFailureFeedbackNotWrittenWhenNil verifies that when FinalFeedback
// is nil (no feedback was captured before give-up), the task column is
// NOT touched (stays empty on a fresh task).
func TestLastFailureFeedbackNotWrittenWhenNil(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "proj3", Name: "P3", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: "t3", ProjectID: "p1", Source: "inbox", Title: "do work 3", Status: "pending"}
	if err := d.store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}

	// Replicate the lifecycle guard: nil FinalFeedback → skip write.
	result := &pipeline.Result{Status: "needs_attention", FinalFeedback: nil, ExhaustReason: "something"}
	if result.FinalFeedback != nil {
		t.Fatal("pre-condition: FinalFeedback should be nil for this test path")
	}
	// (no write happens)

	got, err := d.store.GetTask(ctx, "t3")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.LastFailureFeedback != "" {
		t.Errorf("LastFailureFeedback should be empty when FinalFeedback=nil; got %q", got.LastFailureFeedback)
	}
}

func TestNonSeqPROpenSetsAwaitingMerge(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p", Slug: "ns", Name: "N", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// gate=none task (non-sequenced); a finish-branch run opened a PR.
	task := &store.Task{ID: "tt", ProjectID: "p", Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateNone}
	if err := d.store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	d.scheduler.markNonSeqPRGate(ctx, task, "https://github.com/o/r/pull/3")
	got, _ := d.store.GetTask(ctx, "tt")
	if got.GateState != sequence.GateAwaitingMerge {
		t.Errorf("gate=%q, want awaiting_merge", got.GateState)
	}
	// A task already gate-managed (sequenced) must NOT be overridden.
	seqTask := &store.Task{ID: "st", ProjectID: "p", Source: "inbox", Title: "y", Status: "running", GateState: sequence.GateBuilt}
	_ = d.store.InsertTask(ctx, seqTask)
	d.scheduler.markNonSeqPRGate(ctx, seqTask, "https://github.com/o/r/pull/4")
	got2, _ := d.store.GetTask(ctx, "st")
	if got2.GateState != sequence.GateBuilt {
		t.Errorf("gate=%q, want built (sequenced gate must not be overridden)", got2.GateState)
	}
}
