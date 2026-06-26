package decompose

import (
	"context"
	"fmt"
	"strings"

	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/store"
)

// SpecContent pairs a spec's relative path with its body content so the
// caller can hand multiple specs to DecomposeForRoadmap.
type SpecContent struct {
	Path string
	Body string
}

// DecomposeForRoadmap proposes subtasks for one roadmap phase. The user
// prompt sent to the runner concatenates the phase body + each linked
// spec's content. Returns the same *Result shape as Decompose.
//
// Synthesizes a store.Task so the existing Decompose flow's prompt
// template, submit_subtasks tool schema, validation, and cost calc are
// all reused — this is a thin wrapper, not a parallel implementation.
func DecomposeForRoadmap(ctx context.Context, runner Runner, project store.Project, phase roadmap.Phase, specs []SpecContent, maxSubtasks int, stackHint string, existing []ExistingRef, codebaseContext string) (*Result, error) {
	if phase.Number == "" {
		return nil, fmt.Errorf("decompose roadmap: phase number is empty")
	}
	body := buildRoadmapPrompt(phase, specs)
	task := store.Task{
		Title:    fmt.Sprintf("Phase %s: %s", phase.Number, phase.Title),
		Body:     body,
		Pipeline: "build", // advisory — each subtask sets its own pipeline
	}
	return Decompose(ctx, runner, task, project, maxSubtasks, stackHint, existing, codebaseContext)
}

// buildRoadmapPrompt assembles the phase + spec content into a single
// body string that Decompose will embed in its user prompt under
// "Body:". Including the project slug indirectly happens because
// Decompose's template already names the project.
func buildRoadmapPrompt(phase roadmap.Phase, specs []SpecContent) string {
	var b strings.Builder
	b.WriteString("This phase comes from a roadmap document. The body and any linked specs follow.\n\n")
	b.WriteString("ROADMAP PHASE:\n")
	b.WriteString(phase.Body)
	b.WriteString("\n\n")
	for _, s := range specs {
		b.WriteString(fmt.Sprintf("SPEC (%s):\n", s.Path))
		b.WriteString(s.Body)
		b.WriteString("\n\n")
	}
	b.WriteString("Decompose this phase into 3-8 concrete Hive tasks via submit_subtasks.")
	return b.String()
}
