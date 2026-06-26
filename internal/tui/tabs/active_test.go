package tabs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeActiveSnap struct {
	runs    []ActiveRunSummary
	pending map[string]bool
}

func (f *fakeActiveSnap) ActiveRuns() []ActiveRunSummary { return f.runs }

func (f *fakeActiveSnap) PendingApprovalRunIDs() map[string]bool { return f.pending }

func TestActiveRendersRunRows(t *testing.T) {
	snap := &fakeActiveSnap{
		runs: []ActiveRunSummary{
			{ID: "run-1", TaskTitle: "thing", Pipeline: "build", Status: "running", Stage: "implement", Iter: 0},
		},
	}
	a := NewActive(snap)
	view := a.View()
	if !strings.Contains(view, "run-1") {
		t.Errorf("view missing run-1: %q", view)
	}
	if !strings.Contains(view, "implement") {
		t.Errorf("view missing stage: %q", view)
	}
}

func TestActiveEmptyMessage(t *testing.T) {
	a := NewActive(&fakeActiveSnap{})
	if !strings.Contains(a.View(), "No active runs") {
		t.Error("empty view missing helper text")
	}
}

func TestActiveEnterEmitsDrillIn(t *testing.T) {
	snap := &fakeActiveSnap{
		runs: []ActiveRunSummary{
			{ID: "run-x", Pipeline: "build", Status: "running"},
			{ID: "run-y", Pipeline: "build", Status: "running"},
		},
	}
	a := NewActive(snap)
	a.Update(tea.KeyMsg{Type: tea.KeyDown}) // selected=1
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter must return cmd")
	}
	req, ok := cmd().(DrillInRequest)
	if !ok {
		t.Fatalf("unexpected msg type")
	}
	if req.RunID != "run-y" {
		t.Errorf("RunID=%q want run-y", req.RunID)
	}
}

func TestActiveRendersProjectColumn(t *testing.T) {
	snap := &fakeActiveSnap{
		runs: []ActiveRunSummary{
			{ID: "run-1", Project: "hive", TaskTitle: "do x", Pipeline: "build", Status: "running"},
		},
	}
	a := NewActive(snap)
	view := a.View()
	if !strings.Contains(view, "PROJECT") {
		t.Errorf("header missing PROJECT: %q", view)
	}
	if !strings.Contains(view, "hive") {
		t.Errorf("row missing project slug: %q", view)
	}
}

func TestGroupRunsTreeOrderRootsThenChildren(t *testing.T) {
	// Build: root A (run-1), child of A (run-2), root B (run-3).
	// Expect: A, C, B in that order.
	runs := []ActiveRunSummary{
		{ID: "run-1"},
		{ID: "run-3"},
		{ID: "run-2", ParentRunID: "run-1"},
	}
	out := groupRunsTreeOrder(runs)
	if len(out) != 3 {
		t.Fatalf("len=%d want 3", len(out))
	}
	gotIDs := []string{out[0].ID, out[1].ID, out[2].ID}
	wantIDs := []string{"run-1", "run-2", "run-3"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("position %d: got %s want %s (full=%v)", i, gotIDs[i], wantIDs[i], gotIDs)
		}
	}
}

func TestGroupRunsTreeOrderMultipleChildrenSortedByID(t *testing.T) {
	// run-1 has two children: run-3 and run-2. ID-sort means run-2 first.
	runs := []ActiveRunSummary{
		{ID: "run-1"},
		{ID: "run-3", ParentRunID: "run-1"},
		{ID: "run-2", ParentRunID: "run-1"},
	}
	out := groupRunsTreeOrder(runs)
	gotIDs := []string{out[0].ID, out[1].ID, out[2].ID}
	wantIDs := []string{"run-1", "run-2", "run-3"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("position %d: got %s want %s (full=%v)", i, gotIDs[i], wantIDs[i], gotIDs)
		}
	}
}

func TestGroupRunsTreeOrderOrphanedChildBecomesRoot(t *testing.T) {
	// Child whose parent isn't in the slice should still render
	// (treated as a root rather than dropped — keeps observability
	// for the partial-snapshot case).
	runs := []ActiveRunSummary{
		{ID: "run-2", ParentRunID: "run-missing"},
		{ID: "run-1"},
	}
	out := groupRunsTreeOrder(runs)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2 (orphan child still rendered)", len(out))
	}
}

func TestActiveRendersChildrenIndentedUnderParent(t *testing.T) {
	// Root run-1 (parent), child run-2 (parent=run-1), root run-3.
	// Render order in View() must be run-1 → run-2 → run-3, and the
	// child row must carry the tree marker.
	snap := &fakeActiveSnap{
		runs: []ActiveRunSummary{
			{ID: "run-1", TaskTitle: "parent-task", Pipeline: "finish-branch", Status: "running"},
			{ID: "run-3", TaskTitle: "other-task", Pipeline: "build", Status: "running"},
			{ID: "run-2", TaskTitle: "parent-task", Pipeline: "build", Status: "running", ParentRunID: "run-1"},
		},
	}
	a := NewActive(snap)
	view := a.View()

	pos1 := strings.Index(view, "run-1")
	pos2 := strings.Index(view, "run-2")
	pos3 := strings.Index(view, "run-3")
	if pos1 < 0 || pos2 < 0 || pos3 < 0 {
		t.Fatalf("missing rows: pos1=%d pos2=%d pos3=%d view=%q", pos1, pos2, pos3, view)
	}
	if !(pos1 < pos2 && pos2 < pos3) {
		t.Errorf("order wrong: pos1=%d pos2=%d pos3=%d", pos1, pos2, pos3)
	}

	// Child row's line must carry the tree marker (├ or └).
	childLineStart := strings.LastIndex(view[:pos2], "\n") + 1
	childLine := view[childLineStart:pos2]
	if !strings.Contains(childLine, "├") && !strings.Contains(childLine, "└") {
		t.Errorf("child row missing tree marker: %q", childLine)
	}
	// Root rows must NOT carry the marker.
	rootLineStart := strings.LastIndex(view[:pos1], "\n") + 1
	rootLine := view[rootLineStart:pos1]
	if strings.Contains(rootLine, "├") || strings.Contains(rootLine, "└") {
		t.Errorf("root row unexpectedly carries tree marker: %q", rootLine)
	}
}

func TestActiveDKeyEmitsConfirmAbandon(t *testing.T) {
	snap := &fakeActiveSnap{
		runs: []ActiveRunSummary{{ID: "run-z", Pipeline: "build", Status: "running"}},
	}
	a := NewActive(snap)
	_, cmd := a.Update(tea.KeyMsg{Runes: []rune{'d'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("d should emit cmd")
	}
	req, ok := cmd().(TabOpenModalRequest)
	if !ok {
		t.Fatalf("got %T", cmd())
	}
	if req.Kind != "confirm_abandon" || req.InitialState["run_id"] != "run-z" {
		t.Errorf("got req=%+v", req)
	}
}
