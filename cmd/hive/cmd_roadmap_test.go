package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/daemon"
	"github.com/rohilrs/Hive/internal/decompose"
)

// TestRoadmapSyncLinearCmdWiring verifies sync-linear is registered under the
// roadmap parent and requires exactly one arg (no live daemon needed).
func TestRoadmapSyncLinearCmdWiring(t *testing.T) {
	root := newRoadmapCmd()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "sync-linear" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("sync-linear subcommand not registered under roadmap")
	}
	if found.Args == nil {
		t.Error("sync-linear should require exactly 1 arg")
	}
}

// TestRoadmapDecomposeRequiresProjectArg verifies cobra rejects the
// subcommand when no project arg is supplied (Args = ExactArgs(1)).
func TestRoadmapDecomposeRequiresProjectArg(t *testing.T) {
	cmd := newRoadmapCmd()
	cmd.SetArgs([]string{"decompose", "--phase", "1"})
	cmd.SetOut(discardWriter{})
	cmd.SetErr(discardWriter{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no project given")
	}
}

// TestRoadmapDecomposeRequiresPhaseFlag verifies cobra rejects the
// subcommand when --phase is missing (MarkFlagRequired).
func TestRoadmapDecomposeRequiresPhaseFlag(t *testing.T) {
	cmd := newRoadmapCmd()
	cmd.SetArgs([]string{"decompose", "demo"})
	cmd.SetOut(discardWriter{})
	cmd.SetErr(discardWriter{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --phase missing")
	}
	if !strings.Contains(err.Error(), "phase") {
		t.Errorf("error should mention phase, got %q", err.Error())
	}
}

// TestRoadmapDecomposeApproveAllInsertsWithMetadata pins the happy path:
// fake daemon returns 3 proposed subtasks; --yes skips the prompt; the
// full proposal set is sent to applyDecompose in one call with the right
// project/phase linkage and all three subtasks.
func TestRoadmapDecomposeApproveAllInsertsWithMetadata(t *testing.T) {
	proposals := &daemon.RoadmapDecomposeResult{
		PhaseNumber: "1",
		PhaseTitle:  "bootstrap",
		RoadmapPath: "/repo/docs/superpowers/roadmaps/demo.md",
		SpecPaths:   []string{"docs/superpowers/specs/2026-05-31-bootstrap.md"},
		Subtasks: []decompose.ProposedSubtask{
			{Title: "init repo", Body: "set up go.mod", Priority: "P1", Pipeline: "build"},
			{Title: "add lint", Body: "configure golangci-lint", Priority: "P2", Pipeline: "build"},
			{Title: "add ci", Body: "github actions push gate", Priority: "P2", Pipeline: "build"},
		},
	}

	var applied map[string]any
	deps := &roadmapDeps{
		fetchProposals: func(slug, phase string, maxN int) (*daemon.RoadmapDecomposeResult, error) {
			if slug != "demo" {
				t.Errorf("fetchProposals got slug %q, want demo", slug)
			}
			if phase != "1" {
				t.Errorf("fetchProposals got phase %q, want 1", phase)
			}
			return proposals, nil
		},
		listExisting: func(slug, phase, roadmapPath string) (int, error) {
			return 0, nil
		},
		applyDecompose: func(params map[string]any) (*daemon.RoadmapDecomposeApplyResult, error) {
			applied = params
			return &daemon.RoadmapDecomposeApplyResult{Inserted: len(proposals.Subtasks)}, nil
		},
	}

	if err := runRoadmapDecompose("demo", "1", true, 0, deps); err != nil {
		t.Fatalf("runRoadmapDecompose: %v", err)
	}
	if applied["phase"] != "1" {
		t.Errorf("apply phase=%v want 1", applied["phase"])
	}
	if applied["project_slug"] != "demo" {
		t.Errorf("apply project_slug=%v want demo", applied["project_slug"])
	}
	if applied["spec_path"] != "docs/superpowers/specs/2026-05-31-bootstrap.md" {
		t.Errorf("apply spec_path=%v want the first spec path", applied["spec_path"])
	}
	subs, ok := applied["subtasks"].([]map[string]any)
	if !ok || len(subs) != 3 {
		t.Fatalf("want 3 subtasks applied, got %v", applied["subtasks"])
	}
	if subs[0]["title"] != "init repo" {
		t.Errorf("subtask[0].title=%v want init repo", subs[0]["title"])
	}
}

// TestRoadmapDecomposeRefusesIfPhaseAlreadyDecomposed pins the guard
// against double-decomposing a phase: if listExisting reports any
// matching task already linked to (roadmap_phase, roadmap_path), refuse
// with a hint to clean up first. applyDecompose must NOT be called.
func TestRoadmapDecomposeRefusesIfPhaseAlreadyDecomposed(t *testing.T) {
	deps := &roadmapDeps{
		fetchProposals: func(slug, phase string, maxN int) (*daemon.RoadmapDecomposeResult, error) {
			return &daemon.RoadmapDecomposeResult{
				PhaseNumber: "1",
				PhaseTitle:  "bootstrap",
				RoadmapPath: "/repo/roadmap.md",
				Subtasks:    []decompose.ProposedSubtask{{Title: "x"}},
			}, nil
		},
		listExisting: func(slug, phase, roadmapPath string) (int, error) {
			if slug != "demo" || phase != "1" || roadmapPath != "/repo/roadmap.md" {
				t.Errorf("listExisting args slug=%q phase=%q path=%q",
					slug, phase, roadmapPath)
			}
			return 2, nil
		},
		applyDecompose: func(params map[string]any) (*daemon.RoadmapDecomposeApplyResult, error) {
			t.Fatal("applyDecompose must NOT be called when phase already decomposed")
			return nil, nil
		},
	}

	err := runRoadmapDecompose("demo", "1", true, 0, deps)
	if err == nil {
		t.Fatal("expected error when phase already decomposed")
	}
	if !strings.Contains(err.Error(), "already decomposed") {
		t.Errorf("error should mention 'already decomposed', got %q", err.Error())
	}
}

// TestRoadmapDecomposeNoProposalsErrors pins the edge case where the
// daemon returns zero subtasks (model bailed). The CLI shouldn't prompt
// for an empty apply; surface a clear error so the operator knows to
// fix the roadmap phase body or spec.
func TestRoadmapDecomposeNoProposalsErrors(t *testing.T) {
	deps := &roadmapDeps{
		fetchProposals: func(slug, phase string, maxN int) (*daemon.RoadmapDecomposeResult, error) {
			return &daemon.RoadmapDecomposeResult{PhaseNumber: "1", Subtasks: nil}, nil
		},
		listExisting: func(slug, phase, roadmapPath string) (int, error) { return 0, nil },
		applyDecompose: func(params map[string]any) (*daemon.RoadmapDecomposeApplyResult, error) {
			t.Fatal("applyDecompose must NOT be called for empty proposals")
			return nil, nil
		},
	}
	if err := runRoadmapDecompose("demo", "1", true, 0, deps); err == nil {
		t.Fatal("expected error when no subtasks proposed")
	}
}

// TestRoadmapDecomposePartialErrorsSurfacedOnStderr pins the contract that
// partial per-subtask failures returned by the daemon (in Errors) are
// surfaced on stderr but do NOT cause runRoadmapDecompose to return an
// error. The daemon owns best-effort batching; the CLI just surfaces the
// diagnostic text.
func TestRoadmapDecomposePartialErrorsSurfacedOnStderr(t *testing.T) {
	proposals := &daemon.RoadmapDecomposeResult{
		PhaseNumber: "1",
		RoadmapPath: "/r.md",
		Subtasks: []decompose.ProposedSubtask{
			{Title: "a"},
			{Title: "b"},
			{Title: "c"},
		},
	}
	var stderrBuf strings.Builder
	deps := &roadmapDeps{
		fetchProposals: func(slug, phase string, maxN int) (*daemon.RoadmapDecomposeResult, error) {
			return proposals, nil
		},
		listExisting: func(slug, phase, roadmapPath string) (int, error) { return 0, nil },
		applyDecompose: func(params map[string]any) (*daemon.RoadmapDecomposeApplyResult, error) {
			return &daemon.RoadmapDecomposeApplyResult{
				Inserted: 2,
				Errors:   []string{`insert "b": boom`},
			}, nil
		},
		stderr: &stderrBuf,
	}
	if err := runRoadmapDecompose("demo", "1", true, 0, deps); err != nil {
		t.Fatalf("runRoadmapDecompose returned err on partial daemon failure: %v", err)
	}
	if !strings.Contains(stderrBuf.String(), "boom") {
		t.Errorf("expected stderr to contain error text, got %q", stderrBuf.String())
	}
}
