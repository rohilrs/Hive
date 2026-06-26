package decompose

import (
	"context"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/store"
)

func TestDecomposeForRoadmapHappyPath(t *testing.T) {
	runner := &stubRunner{out: makeToolUse(`[
        {"title":"init repo","body":"set up go.mod","priority":"P1","pipeline":"build"},
        {"title":"add ci","body":"github actions","priority":"P2","pipeline":"build"}
    ]`)}
	proj := store.Project{ID: "p1", Slug: "demo", Name: "Demo"}
	phase := roadmap.Phase{Number: "1", Title: "bootstrap", Body: "Get started."}
	specs := []SpecContent{{Path: "docs/superpowers/specs/2026-05-31-bootstrap.md", Body: "# Bootstrap spec\n\nThe details."}}

	result, err := DecomposeForRoadmap(context.Background(), runner, proj, phase, specs, 0, "", nil, "")
	if err != nil {
		t.Fatalf("DecomposeForRoadmap: %v", err)
	}
	if len(result.Subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(result.Subtasks))
	}
	// The user prompt seen by the runner must include the phase + spec content
	// so the model has the design as context.
	sent := runner.lastInput
	if !strings.Contains(sent, "bootstrap") || !strings.Contains(sent, "Get started") {
		t.Errorf("user prompt missing phase: %s", sent)
	}
	if !strings.Contains(sent, "Bootstrap spec") {
		t.Errorf("user prompt missing spec content: %s", sent)
	}
	if !strings.Contains(sent, "demo") {
		t.Errorf("user prompt missing project slug: %s", sent)
	}
}

func TestDecomposeForRoadmapRequiresPhase(t *testing.T) {
	runner := &stubRunner{}
	proj := store.Project{ID: "p", Slug: "s", Name: "n"}
	_, err := DecomposeForRoadmap(context.Background(), runner, proj, roadmap.Phase{}, nil, 0, "", nil, "")
	if err == nil {
		t.Fatal("expected error on empty phase number")
	}
}
