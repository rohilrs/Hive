package modals

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/decompose"
)

// manySubtasks builds n proposals with longish titles/bodies to exercise the
// height-bounded scroll in the confirm modal.
func manySubtasks(n int) []decompose.ProposedSubtask {
	out := make([]decompose.ProposedSubtask, n)
	for i := range out {
		out[i] = decompose.ProposedSubtask{
			Title:    fmt.Sprintf("proposal %d with a deliberately long title that should truncate to the modal width", i),
			Body:     "a multi-paragraph body that collapses to a one-line preview shown under the head",
			Priority: "P2", Pipeline: "build",
		}
	}
	return out
}

// TestDecomposeConfirmNeverExceedsHeight: the proposals list scrolls within a
// height-bounded modal, so a long list can't overflow + clip the modal.
// Regression for the dogfood "modal gets cut off at the top".
func TestDecomposeConfirmNeverExceedsHeight(t *testing.T) {
	m := NewDecomposeConfirmModal("hive", "1", "phase one",
		"/repo/docs/superpowers/roadmaps/hive.md", []string{"docs/superpowers/specs/x.md"},
		manySubtasks(25))
	for _, wd := range []int{80, 120} {
		for _, h := range []int{16, 24, 40} {
			for _, cur := range []int{0, 12, 24} {
				m.cursor = cur
				for _, fr := range []bool{false, true} {
					m.focusRight = fr
					for _, sc := range []int{0, 5, 100} {
						m.detailScroll = sc
						rows := strings.Count(m.View(wd, h), "\n") + 1
						if rows > h {
							t.Errorf("w=%d h=%d cur=%d focusRight=%v scroll=%d: View %d rows > height", wd, h, cur, fr, sc, rows)
						}
					}
				}
			}
		}
	}
}

// TestDecomposeConfirmTabFocusAndNav: Tab switches which pane ↑/↓ act on — left
// moves the proposal selection, right scrolls the detail; selection reset clears
// the detail scroll.
func TestDecomposeConfirmTabFocusAndNav(t *testing.T) {
	m := NewDecomposeConfirmModal("hive", "1", "p", "/r.md", nil, manySubtasks(20))
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 1 || m.focusRight {
		t.Fatalf("left focus down should move selection; cursor=%d focusRight=%v", m.cursor, m.focusRight)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.focusRight {
		t.Fatal("tab should focus the detail pane")
	}
	cur := m.cursor
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.detailScroll != 1 || m.cursor != cur {
		t.Errorf("right focus down should scroll detail; detailScroll=%d cursor=%d", m.detailScroll, m.cursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.detailScroll != 0 {
		t.Errorf("changing selection should reset detailScroll; got %d", m.detailScroll)
	}
}

// sampleSubtasks builds a 3-proposal fixture matching the wire shape that
// roadmap.decompose returns. Used across the decompose_confirm tests so
// the assertions can reference stable titles.
func sampleSubtasks() []decompose.ProposedSubtask {
	return []decompose.ProposedSubtask{
		{Title: "alpha task", Body: "do alpha", Priority: "P1", Pipeline: "build"},
		{Title: "beta task", Body: "do beta", Priority: "P2", Pipeline: "debug"},
		{Title: "gamma task", Body: "do gamma"}, // priority/pipeline default
	}
}

func TestDecomposeConfirmRendersProposalsList(t *testing.T) {
	m := NewDecomposeConfirmModal(
		"hive", "2", "ship the TUI",
		"/repo/docs/superpowers/roadmaps/hive.md",
		[]string{"docs/superpowers/specs/x.md"},
		sampleSubtasks(),
	)
	view := m.View(120, 40)
	for _, title := range []string{"alpha task", "beta task", "gamma task"} {
		if !strings.Contains(view, title) {
			t.Errorf("view missing proposal title %q; got:\n%s", title, view)
		}
	}
	// Title includes the phase number + phase title.
	if !strings.Contains(view, "Phase 2") || !strings.Contains(view, "ship the TUI") {
		t.Errorf("view missing phase heading; got:\n%s", view)
	}
	// Roadmap + spec paths surface (DimText-rendered).
	if !strings.Contains(view, "roadmaps/hive.md") {
		t.Errorf("view missing roadmap path; got:\n%s", view)
	}
	// Footer hint with y/n keybinds.
	if !strings.Contains(view, "insert all") || !strings.Contains(view, "cancel") {
		t.Errorf("view missing footer hint; got:\n%s", view)
	}
}

func TestDecomposeConfirmYEmitsInsertTasks(t *testing.T) {
	m := NewDecomposeConfirmModal(
		"hive", "2", "title", "/r/hive.md",
		[]string{"docs/superpowers/specs/x.md", "docs/superpowers/specs/y.md"},
		sampleSubtasks(),
	)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("y should emit a cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "roadmap_insert_tasks" {
		t.Errorf("Kind=%q want roadmap_insert_tasks", req.Kind)
	}
	if req.Params["project_slug"] != "hive" {
		t.Errorf("project_slug=%v want hive", req.Params["project_slug"])
	}
	// Subtasks passed through as typed slice (not []any) so the root
	// handler can iterate without re-decoding.
	subs, ok := req.Params["subtasks"].([]decompose.ProposedSubtask)
	if !ok {
		t.Fatalf("subtasks param type=%T want []decompose.ProposedSubtask", req.Params["subtasks"])
	}
	if len(subs) != 3 {
		t.Errorf("subtasks len=%d want 3", len(subs))
	}
	// Phase linkage fields are top-level params (no longer nested in metadata)
	// so the root can forward them directly to roadmap.decompose_apply.
	if req.Params["phase"] != "2" {
		t.Errorf("phase=%v want 2", req.Params["phase"])
	}
	if req.Params["phase_title"] != "title" {
		t.Errorf("phase_title=%v want title", req.Params["phase_title"])
	}
	if req.Params["roadmap_path"] != "/r/hive.md" {
		t.Errorf("roadmap_path=%v want /r/hive.md", req.Params["roadmap_path"])
	}
	// First spec is the primary; second is dropped per CLI behavior.
	if req.Params["spec_path"] != "docs/superpowers/specs/x.md" {
		t.Errorf("spec_path=%v want first spec only", req.Params["spec_path"])
	}
	// Modal flips into submitting state so a rapid second y is ignored.
	if !m.submitting {
		t.Errorf("submitting should flip to true on y")
	}
}

func TestDecomposeConfirmEscEmitsClose(t *testing.T) {
	m := NewDecomposeConfirmModal("hive", "1", "t", "/r.md", nil, sampleSubtasks())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc emitted %T want CloseMsg", cmd())
	}
}

func TestDecomposeConfirmNEmitsClose(t *testing.T) {
	// n is an alias for esc — the "no" half of [y/N].
	m := NewDecomposeConfirmModal("hive", "1", "t", "/r.md", nil, sampleSubtasks())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatal("n should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("n emitted %T want CloseMsg", cmd())
	}
}

func TestDecomposeConfirmSuccessClosesModal(t *testing.T) {
	m := NewDecomposeConfirmModal("hive", "1", "t", "/r.md", nil, sampleSubtasks())
	// Simulate submit by flipping into submitting; the RPC result then
	// arrives via RPCResultMsg from the root.
	m.submitting = true
	_, cmd := m.Update(RPCResultMsg{
		Kind: "roadmap_insert_tasks",
		Data: map[string]any{"inserted": 3, "total": 3},
	})
	if cmd == nil {
		t.Fatal("success result should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("success emitted %T want CloseMsg", cmd())
	}
}

func TestDecomposeConfirmRpcErrorShownInline(t *testing.T) {
	m := NewDecomposeConfirmModal("hive", "1", "t", "/r.md", nil, sampleSubtasks())
	m.submitting = true
	_, cmd := m.Update(RPCResultMsg{
		Kind: "roadmap_insert_tasks",
		Err:  errors.New("daemon: project not found"),
	})
	// Error should NOT close — the operator should be able to read it.
	if cmd != nil {
		// If a cmd is returned it must NOT be CloseMsg.
		if _, ok := cmd().(CloseMsg); ok {
			t.Errorf("error result must not auto-close the modal")
		}
	}
	if !strings.Contains(m.errMsg, "project not found") {
		t.Errorf("errMsg=%q want 'project not found'", m.errMsg)
	}
	// submitting cleared so the operator can retry with y.
	if m.submitting {
		t.Errorf("submitting should clear on error")
	}
	view := m.View(120, 40)
	if !strings.Contains(view, "project not found") {
		t.Errorf("view missing inline error; got:\n%s", view)
	}
}

func TestDecomposeConfirmMergeAnnotationInLeftRow(t *testing.T) {
	// When a subtask has MergeFrom set the left-pane row must include the
	// "← merges <ref>" annotation so the operator sees the reconciliation
	// intent without switching to the detail pane.
	subs := []decompose.ProposedSubtask{
		{Title: "alpha task", Body: "do alpha", Priority: "P1", Pipeline: "build", MergeFrom: "linear:ABC-42"},
		{Title: "beta task", Body: "do beta", Priority: "P2", Pipeline: "build"},
	}
	m := NewDecomposeConfirmModal("hive", "1", "phase", "/r.md", nil, subs)
	view := m.View(160, 40)
	if !strings.Contains(view, "← merges linear:ABC-42") {
		t.Errorf("view missing merge annotation for linear:ABC-42; got:\n%s", view)
	}
	// Non-merge subtask must not have the annotation.
	if strings.Contains(view, "← merges") && strings.Count(view, "← merges") != 1 {
		t.Errorf("unexpected extra merge annotations in view; got:\n%s", view)
	}
}

func TestDecomposeConfirmDoubleYIgnoredWhileSubmitting(t *testing.T) {
	// Defensive — a second y press while submitting must not re-fire
	// the SubmitRequest (would double-insert the entire batch).
	m := NewDecomposeConfirmModal("hive", "1", "t", "/r.md", nil, sampleSubtasks())
	_, firstCmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if firstCmd == nil {
		t.Fatal("first y should emit cmd")
	}
	_, secondCmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if secondCmd != nil {
		t.Errorf("second y while submitting must NOT re-emit; got cmd")
	}
}
