package pipeline

import (
	"context"
	"testing"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/store"
)

func debugTestRun() *Run {
	return &Run{ID: "run-1", Task: &store.Task{ID: "t1", Title: "fix bug", Body: "It crashes."}, Project: &store.Project{ID: "p1"}}
}

func TestDebugVerifyPassesFirstIter(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"reproduce": {{}}, "isolate": {{}}, "fix": {{}},
	}}
	// VerifyCommand empty => verify auto-passes.
	p := &DebugPipeline{Adapter: adp, Cfg: DebugConfig{MaxIterations: 3, Ladder: ModelLadder{Worker: []string{"m"}}}}
	res, err := p.Run(context.Background(), debugTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" {
		t.Errorf("Status=%q want done", res.Status)
	}
	if res.Iterations != 1 {
		t.Errorf("Iterations=%d want 1", res.Iterations)
	}
}

func TestDebugExhaustsWhenVerifyAlwaysFails(t *testing.T) {
	adp := &fakeAdapter{scripts: map[string][]*adapter.StageOutput{
		"reproduce": {{}}, "isolate": {{}, {}}, "fix": {{}, {}},
	}}
	// VerifyCommand "false" always exits non-zero.
	p := &DebugPipeline{Adapter: adp, Feedback: newFakeFeedback(), Cfg: DebugConfig{MaxIterations: 2, VerifyCommand: "false", Ladder: ModelLadder{Worker: []string{"m"}}}}
	res, err := p.Run(context.Background(), debugTestRun())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_attention" {
		t.Errorf("Status=%q want needs_attention", res.Status)
	}
}
