package decompose

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/store"
)

// stubRunner returns a fixed TurnOutput; tests construct it per-case.
// lastInput captures the concatenated text of the first user message
// sent on the most recent RunTurn call so callers can assert on the
// prompt that was actually delivered. Zero-value default — existing
// tests don't need to set or read it.
type stubRunner struct {
	out       *anthropic.TurnOutput
	err       error
	lastInput string
}

func (s *stubRunner) RunTurn(ctx context.Context, in anthropic.TurnInput) (*anthropic.TurnOutput, error) {
	// Capture the first user message's concatenated text for assertion
	// in tests. Walk the SDK's MessageParam → ContentBlockParamUnion →
	// OfText.Text chain.
	for _, m := range in.Messages {
		if m.Role != anth.MessageParamRoleUser {
			continue
		}
		var buf []byte
		for _, block := range m.Content {
			if block.OfText != nil {
				buf = append(buf, block.OfText.Text...)
			}
		}
		s.lastInput = string(buf)
		break
	}
	return s.out, s.err
}

func makeToolUse(subtasksJSON string) *anthropic.TurnOutput {
	return &anthropic.TurnOutput{
		StopReason: "tool_use",
		ToolCalls: []anthropic.ToolCall{
			{
				ID:    "tu1",
				Name:  "submit_subtasks",
				Input: json.RawMessage(`{"subtasks":` + subtasksJSON + `}`),
			},
		},
		TokensIn:  1000,
		TokensOut: 500,
	}
}

func sampleTask() store.Task {
	return store.Task{ID: "task-1", ProjectID: "p1", Title: "ship phase 7", Body: "do all the things", Priority: "P1", Pipeline: "build"}
}
func sampleProject() store.Project {
	return store.Project{ID: "p1", Slug: "hive", Name: "Hive"}
}

func TestDecomposeHappyPath(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[
		{"title":"step 1","body":"do thing","priority":"P0","pipeline":"build"},
		{"title":"step 2","body":"do other","priority":"P1","pipeline":"debug"}
	]`)}
	res, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(res.Subtasks) != 2 {
		t.Fatalf("got %d subtasks, want 2", len(res.Subtasks))
	}
	if res.Subtasks[0].Title != "step 1" || res.Subtasks[0].Priority != "P0" || res.Subtasks[0].Pipeline != "build" {
		t.Errorf("subtask[0]=%+v", res.Subtasks[0])
	}
	if res.InputTokens != 1000 || res.OutputTokens != 500 {
		t.Errorf("tokens in=%d out=%d, want 1000/500", res.InputTokens, res.OutputTokens)
	}
	if res.CostUSD <= 0 {
		t.Errorf("CostUSD=%f, want > 0", res.CostUSD)
	}
}

func TestDecomposeRejectsEmpty(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[]`)}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err=%v, want 'empty'", err)
	}
}

func TestDecomposeRejectsOversize(t *testing.T) {
	// 21 subtasks (1 over the hard cap of 20)
	var items []string
	for i := 0; i < 21; i++ {
		items = append(items, `{"title":"x","body":"y","priority":"P1","pipeline":"build"}`)
	}
	runner := &stubRunner{out: makeToolUse(`[` + strings.Join(items, ",") + `]`)}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 20, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "oversize") {
		t.Errorf("err=%v, want 'oversize'", err)
	}
}

func TestDecomposeRejectsInvalidPriority(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"x","body":"y","priority":"urgent","pipeline":"build"}]`)}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "priority") {
		t.Errorf("err=%v, want 'priority'", err)
	}
}

func TestDecomposeRejectsInvalidPipeline(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"x","body":"y","priority":"P1","pipeline":"lint"}]`)}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "pipeline") {
		t.Errorf("err=%v, want 'pipeline'", err)
	}
}

func TestDecomposeRejectsEmptyTitle(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"","body":"y","priority":"P1","pipeline":"build"}]`)}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("err=%v, want 'title'", err)
	}
}

func TestDecomposeDedupesDuplicateTitles(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[
		{"title":"same","body":"a","priority":"P1","pipeline":"build"},
		{"title":"same","body":"b","priority":"P1","pipeline":"build"},
		{"title":"different","body":"c","priority":"P1","pipeline":"build"}
	]`)}
	res, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(res.Subtasks) != 2 {
		t.Errorf("got %d subtasks after dedup, want 2", len(res.Subtasks))
	}
	if res.Subtasks[0].Body != "a" {
		t.Errorf("first 'same' should be kept, body=%q want 'a'", res.Subtasks[0].Body)
	}
}

func TestDecomposeDefaultsMissingPipeline(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"x","body":"y","priority":"P1"}]`)}
	res, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if res.Subtasks[0].Pipeline != "build" {
		t.Errorf("default pipeline=%q, want 'build'", res.Subtasks[0].Pipeline)
	}
}

func TestDecomposePropagatesRunnerError(t *testing.T) {
	runner := &stubRunner{err: errors.New("boom")}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err=%v, want runner err propagated", err)
	}
}

func TestDecomposeNoToolUseStopReasonIsError(t *testing.T) {
	// Assistant responded with text instead of tool_use.
	runner := &stubRunner{out: &anthropic.TurnOutput{StopReason: "end_turn", Text: "I refuse"}}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err == nil {
		t.Error("expected error for end_turn without tool_use")
	}
}

// TestDecomposeIncludesStackHint: a non-empty stack hint is appended to the
// user prompt so the model writes acceptance criteria using the project's real
// commands (e.g. pnpm) rather than defaulting to Go's `go test`.
func TestDecomposeIncludesStackHint(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1","pipeline":"build"}]`)}
	hint := "PROJECT PIPELINE COMMANDS:\n- tests: pnpm -r test\n- build: pnpm -r build"
	if _, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, hint, nil, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.lastInput, "pnpm -r test") {
		t.Errorf("user prompt missing the stack hint; got:\n%s", runner.lastInput)
	}
}

// TestDecomposeOmitsEmptyStackHint: an empty hint adds nothing extra.
func TestDecomposeOmitsEmptyStackHint(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1","pipeline":"build"}]`)}
	if _, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runner.lastInput, "PROJECT PIPELINE COMMANDS") {
		t.Errorf("empty hint should not add the commands block; got:\n%s", runner.lastInput)
	}
}

func TestDecomposeIncludesExistingWorkBlock(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1","pipeline":"build"}]`)}
	existing := []ExistingRef{{Ref: "linear:u1", Block: "- [linear:u1] (HBA-1) Stand up harness — …"}}
	if _, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", existing, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.lastInput, "EXISTING WORK") || !strings.Contains(runner.lastInput, "linear:u1") {
		t.Errorf("prompt missing EXISTING WORK block:\n%s", runner.lastInput)
	}
}

func TestDecomposeRejectsFabricatedMergeFrom(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1","pipeline":"build","merge_from":"linear:GHOST"}]`)}
	existing := []ExistingRef{{Ref: "linear:u1", Block: "- [linear:u1] x"}}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", existing, "")
	if err == nil || !strings.Contains(err.Error(), "merge_from") {
		t.Errorf("want merge_from rejection, got %v", err)
	}
}

func TestDecomposeAcceptsValidMergeFrom(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1","pipeline":"build","merge_from":"linear:u1"}]`)}
	existing := []ExistingRef{{Ref: "linear:u1", Block: "- [linear:u1] x"}}
	res, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", existing, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Subtasks[0].MergeFrom != "linear:u1" {
		t.Errorf("merge_from not preserved: %q", res.Subtasks[0].MergeFrom)
	}
}

func TestDecomposeStripsMergeFromWhenNoExistingWork(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1","pipeline":"build","merge_from":"hive:ghost"}]`)}
	res, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err != nil {
		t.Fatalf("nil existing-work must not error on stray merge_from: %v", err)
	}
	if res.Subtasks[0].MergeFrom != "" {
		t.Errorf("merge_from should be stripped when no existing work; got %q", res.Subtasks[0].MergeFrom)
	}
}

func TestValidateSanitizesDependsOn(t *testing.T) {
	items := []ProposedSubtask{
		{Title: "a", Body: "x", Priority: "P1"},
		{Title: "b", Body: "x", Priority: "P1"},
		// index 2: deps {0, 2, -1, 5, 0} → only 0 is a valid backward ref;
		// 2 is self, 5 is forward/out-of-range, -1 is negative, dup 0 deduped.
		{Title: "c", Body: "x", Priority: "P1", DependsOn: []int{0, 2, -1, 5, 0}},
	}
	out, err := Validate(items, 10)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d items, want 3", len(out))
	}
	got := out[2].DependsOn
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("depends_on sanitized = %v, want [0]", got)
	}
	// Items without depends_on stay nil.
	if out[0].DependsOn != nil || out[1].DependsOn != nil {
		t.Errorf("items without deps should keep nil depends_on; got %v / %v", out[0].DependsOn, out[1].DependsOn)
	}
}

func TestValidateDropsAllInvalidDependsOnToNil(t *testing.T) {
	items := []ProposedSubtask{
		{Title: "a", Body: "x", Priority: "P1"},
		// All forward/self/out-of-range → cleaned to nil, not empty slice.
		{Title: "b", Body: "x", Priority: "P1", DependsOn: []int{1, 5, -2}},
	}
	out, err := Validate(items, 10)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if out[1].DependsOn != nil {
		t.Errorf("all-invalid deps should clean to nil; got %v", out[1].DependsOn)
	}
}

func TestDecomposeRoundTripsRelevantFiles(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[
		{"title":"t1","body":"b1","priority":"P1","relevant_files":["a.go","b.ts","a.go"," "]}
	]`)}
	res, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil, "")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(res.Subtasks) != 1 {
		t.Fatalf("got %d subtasks", len(res.Subtasks))
	}
	got := res.Subtasks[0].RelevantFiles
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.ts" {
		t.Errorf("relevant_files = %v, want [a.go b.ts]", got)
	}
}

func TestValidateRemapsDependsOnAcrossDedup(t *testing.T) {
	// Input A(0), A-dup(1), B(2), C(3 depends_on input idx 2 = B).
	// After dedup out=[A,B,C]; C's dep must remap from input-2 to output-1 (B).
	in := []ProposedSubtask{
		{Title: "A", Body: "b", Priority: "P1"},
		{Title: "A", Body: "b", Priority: "P1"},
		{Title: "B", Body: "b", Priority: "P1"},
		{Title: "C", Body: "b", Priority: "P1", DependsOn: []int{2}},
	}
	clean, err := Validate(in, 10)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(clean) != 3 {
		t.Fatalf("got %d after dedup, want 3", len(clean))
	}
	c := clean[2]
	if c.Title != "C" || len(c.DependsOn) != 1 || c.DependsOn[0] != 1 {
		t.Errorf("C.DependsOn = %v, want [1] (B's output index)", c.DependsOn)
	}
}

func TestValidateDropsDependsOnTargetingDroppedDup(t *testing.T) {
	// C depends on input idx 1 (the dropped A-dup) → dropped, not mis-wired.
	in := []ProposedSubtask{
		{Title: "A", Body: "b", Priority: "P1"},
		{Title: "A", Body: "b", Priority: "P1"},
		{Title: "C", Body: "b", Priority: "P1", DependsOn: []int{1}},
	}
	clean, err := Validate(in, 10)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	c := clean[len(clean)-1]
	if len(c.DependsOn) != 0 {
		t.Errorf("C.DependsOn = %v, want nil (dep targeted a dropped dup)", c.DependsOn)
	}
}

func TestDecomposeInjectsCodebaseContext(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[{"title":"t","body":"b","priority":"P1"}]`)}
	_, err := Decompose(context.Background(), runner, sampleTask(), sampleProject(), 10, "", nil,
		"CODEBASE CONTEXT — existing: foo.go:1: func Alpha()")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if !strings.Contains(runner.lastInput, "CODEBASE CONTEXT") || !strings.Contains(runner.lastInput, "foo.go:1") {
		t.Errorf("codebase context not injected into prompt:\n%s", runner.lastInput)
	}
}

func TestValidateCapsRelevantFiles(t *testing.T) {
	many := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, "f"+strconv.Itoa(i)+".go")
	}
	in := []ProposedSubtask{{Title: "t", Body: "b", Priority: "P1", RelevantFiles: many}}
	clean, err := Validate(in, 10)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(clean[0].RelevantFiles) != maxRelevantFiles {
		t.Errorf("relevant_files len = %d, want cap %d", len(clean[0].RelevantFiles), maxRelevantFiles)
	}
}
