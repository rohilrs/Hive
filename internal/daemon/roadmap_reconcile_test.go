package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

func TestReconcileRoadmapStatus(t *testing.T) {
	d := newTestDaemon(t)
	ctx := context.Background()
	repo := t.TempDir()
	repoPath := repo

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "rrs", Name: "RRS", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
		ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
	}); err != nil {
		t.Fatal(err)
	}

	// Three-phase roadmap on disk.
	// Phase 1: Status ⬜ Not started (will be updated — tasks complete)
	// Phase 2: Status already done with provenance (must be preserved)
	// Phase 3: Status ⬜ Not started (tasks incomplete — must be left alone)
	roadmapBody := "## Phase 1: Foundation\n\n**Status:** ⬜ Not started\n\n## Phase 2: Core\n\n**Status:** ✅ Done — merged via PR #190\n\n## Phase 3: Polish\n\n**Status:** ⬜ Not started\n"
	writeSeqRoadmap(t, repo, "rrs", roadmapBody)

	// Phase 1: one task with gate_state=satisfied → derives Complete.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "t1", ProjectID: "p1", Source: "inbox", Title: "phase1-task",
		Status: "done", GateState: sequence.GateSatisfied,
		Metadata: map[string]any{"roadmap_phase": "1"},
	}); err != nil {
		t.Fatal(err)
	}

	// Phase 2: already Complete via MarkPhaseComplete (explicit mark). This is
	// the canonical path — derivePlan reads CompletedPhases from the dispatcher
	// row, which MarkPhaseComplete writes to the completed_phases column.
	if err := d.store.MarkPhaseComplete(ctx, "p1", "2"); err != nil {
		t.Fatal(err)
	}

	// Phase 3: one task with gate_state=none → not resolved → phase NOT Complete.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "t3", ProjectID: "p1", Source: "inbox", Title: "phase3-task",
		Status: "pending", GateState: sequence.GateNone,
		Metadata: map[string]any{"roadmap_phase": "3"},
	}); err != nil {
		t.Fatal(err)
	}

	// --- Call the function under test ---
	d.reconcileRoadmapStatus(ctx)

	// --- Read back and assert ---
	roadmapPath := filepath.Join(repo, "docs", "superpowers", "roadmaps", "rrs.md")
	data, err := os.ReadFile(roadmapPath)
	if err != nil {
		t.Fatalf("read roadmap after reconcile: %v", err)
	}
	md := string(data)

	// Phase 1: Status should now be "✅ Done — marked complete via Hive"
	if !strings.Contains(md, "✅ Done — marked complete via Hive") {
		t.Errorf("phase 1: expected '✅ Done — marked complete via Hive' in roadmap, got:\n%s", md)
	}

	// Phase 2: provenance line must be UNCHANGED
	if !strings.Contains(md, "✅ Done — merged via PR #190") {
		t.Errorf("phase 2: provenance line '✅ Done — merged via PR #190' was clobbered, got:\n%s", md)
	}

	// Phase 3: must still be ⬜ Not started (untouched)
	// Split into sections to check Phase 3 specifically.
	phase3Start := strings.Index(md, "## Phase 3")
	if phase3Start < 0 {
		t.Fatalf("phase 3 heading not found in roadmap:\n%s", md)
	}
	phase3Section := md[phase3Start:]
	if !strings.Contains(phase3Section, "⬜ Not started") {
		t.Errorf("phase 3: expected '⬜ Not started' to be unchanged, got phase 3 section:\n%s", phase3Section)
	}
	// Also confirm Hive's Done marker was NOT written into Phase 3 section.
	if strings.Contains(phase3Section, "marked complete via Hive") {
		t.Errorf("phase 3: incorrectly marked complete, got phase 3 section:\n%s", phase3Section)
	}
}
