package pipeline

import (
	"context"
	"testing"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/verdict"
)

func planTestRun() *Run {
	return &Run{
		ID:      "run-1",
		Task:    &store.Task{ID: "t1", Title: "Add a widget", Body: "We need a widget."},
		Project: &store.Project{ID: "p1"},
	}
}

func TestPlanApprovesAndWritesPlan(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"brainstorm":  {{}},
		"spec-write":  {{}},
		"spec-review": {{Verdict: makeVerdict(adapter.VerdictApprove)}},
		"plan-write":  {{}},
	}}
	p := &PlanPipeline{Adapter: adp, Cfg: PlanConfig{MaxIterations: 3, Ladder: ModelLadder{Worker: []string{"m"}, Reviewer: []string{"m"}}}}
	res, err := p.Run(context.Background(), planTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done (summary=%q)", res.Status, res.Summary)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations=%d want 1", res.Iterations)
	}
}

func TestPlanLoopsThenApproves(t *testing.T) {
	cr := makeVerdict(adapter.VerdictChangesRequested)
	cr.FileRefs = []verdict.FileRef{{Path: "spec.md", Comment: "add non-goals", Reasoning: "scope"}}
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"brainstorm":  {{}},
		"spec-write":  {{}, {}},
		"spec-review": {{Verdict: cr}, {Verdict: makeVerdict(adapter.VerdictApprove)}},
		"plan-write":  {{}},
	}}
	p := &PlanPipeline{Adapter: adp, Feedback: newFakeFeedback(), Cfg: PlanConfig{MaxIterations: 3, Ladder: ModelLadder{Worker: []string{"m"}, Reviewer: []string{"m"}}}}
	res, err := p.Run(context.Background(), planTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done", res.Status)
	}
	if res.Iterations != 2 {
		t.Errorf("Iterations=%d want 2", res.Iterations)
	}
}

func TestPlanExhaustsWithoutApprove(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"brainstorm":  {{}},
		"spec-write":  {{}, {}},
		"spec-review": {{Verdict: makeVerdict(adapter.VerdictChangesRequested)}, {Verdict: makeVerdict(adapter.VerdictChangesRequested)}},
	}}
	p := &PlanPipeline{Adapter: adp, Cfg: PlanConfig{MaxIterations: 2, Ladder: ModelLadder{Worker: []string{"m"}, Reviewer: []string{"m"}}}}
	res, err := p.Run(context.Background(), planTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
}
