package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/branchhealth"
)

func loadedModal(t *testing.T, rep branchhealth.HealthReport) *HealthCheckModal {
	t.Helper()
	m := NewHealthCheckModal("slug", "/repo", rep.Feature, rep.Target).(*HealthCheckModal)
	upd, _ := m.Update(healthLoadedMsg{report: rep})
	return upd.(*HealthCheckModal)
}

func TestHealthModal_ClosesWhileLoading(t *testing.T) {
	// A not-yet-loaded modal (slow git health check) must never trap the
	// user — any key closes it.
	m := NewHealthCheckModal("slug", "/repo", "feature", "main").(*HealthCheckModal)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected a command while loading")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("any key while loading should close the modal")
	}
}

func TestHealthModal_EscClosesWhileSubmitting(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 1})
	um, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = um.(*HealthCheckModal)
	um, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = um.(*HealthCheckModal)
	if !m.submitting {
		t.Fatal("precondition: modal should be submitting after r,y")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc while submitting should emit a command")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc while submitting should close the modal")
	}
}

func TestHealthModal_ShowsActionsWhenBehindAndClean(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 2, Ahead: 1})
	v := m.View(100, 40)
	if !strings.Contains(v, "rebase") || !strings.Contains(v, "merge") {
		t.Errorf("expected rebase/merge action hints, got:\n%s", v)
	}
}

func TestHealthModal_HidesActionsOnPredictedConflict(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{
		Feature: "feature", Target: "main", Behind: 2, ConflictPaths: []string{"a.go"},
	})
	v := m.View(100, 40)
	if strings.Contains(v, "[r]") {
		t.Errorf("must not offer rebase when conflicts predicted, got:\n%s", v)
	}
	if !strings.Contains(v, "resolve manually") {
		t.Errorf("expected conflict notice, got:\n%s", v)
	}
}

func TestHealthModal_RebaseConfirmEmitsSubmit(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 1})
	// Press 'r' → confirm prompt, no submit yet.
	upd, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = upd.(*HealthCheckModal)
	if cmd != nil {
		t.Fatal("pressing r should not emit a command yet")
	}
	if !strings.Contains(m.View(100, 40), "rewrites history") {
		t.Errorf("expected confirm prompt after r")
	}
	// Press 'y' → SubmitRequest.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("pressing y should emit a submit command")
	}
	msg := cmd()
	req, ok := msg.(SubmitRequest)
	if !ok {
		t.Fatalf("want SubmitRequest, got %T", msg)
	}
	if req.Kind != "remediate_health" || req.Params["action"] != "rebase" || req.Params["project_slug"] != "slug" {
		t.Errorf("bad submit: %+v", req)
	}
}

func TestHealthModal_SuccessReRenders(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 1})
	um, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = um.(*HealthCheckModal)
	um, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = um.(*HealthCheckModal)
	upd, _ := m.Update(RPCResultMsg{Kind: "remediate_health", Data: map[string]any{
		"behind": float64(0), "ahead": float64(2),
		"report": "Feature branch \"feature\" vs target \"main\":\n  ✓ clean\n",
	}})
	m = upd.(*HealthCheckModal)
	v := m.View(100, 40)
	if !strings.Contains(v, "rebased") {
		t.Errorf("expected success status mentioning rebased, got:\n%s", v)
	}
	if strings.Contains(v, "[r]") {
		t.Errorf("actions should be gone after behind→0, got:\n%s", v)
	}
}

func TestHealthModal_ErrorStaysOpen(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 1})
	um, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = um.(*HealthCheckModal)
	um, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = um.(*HealthCheckModal)
	upd, cmd := m.Update(RPCResultMsg{Kind: "remediate_health", Err: errors.New("rebase: boom")})
	m = upd.(*HealthCheckModal)
	if cmd != nil {
		t.Error("error result must not close the modal")
	}
	if !strings.Contains(m.View(100, 40), "boom") {
		t.Errorf("expected inline error, got:\n%s", m.View(100, 40))
	}
}

func TestHealthModal_CleanReportClosesOnAnyKey(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Clean: true})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if cmd == nil {
		t.Fatal("expected a command")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("non-actionable modal should close on any key")
	}
}

func TestHealthModal_HidesActionsWhenDirty(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 2, Dirty: true})
	v := m.View(100, 40)
	if strings.Contains(v, "[r]") {
		t.Errorf("must not offer rebase when tree is dirty, got:\n%s", v)
	}
	if !strings.Contains(v, "commit or stash") {
		t.Errorf("expected dirty hint, got:\n%s", v)
	}
}

// longReportText returns a multi-line report body simulating many ConflictPaths
// or a long health summary (40+ lines) to exercise height bounding.
func longReportText(n int) string {
	var b strings.Builder
	b.WriteString("Feature branch \"feature\" vs target \"main\":\n")
	b.WriteString("  Behind: 5 commits\n")
	b.WriteString("  Ahead:  2 commits\n")
	b.WriteString("  Predicted conflict paths:\n")
	for i := 0; i < n; i++ {
		b.WriteString("    internal/some/package/file_" + strings.Repeat("x", 10) + ".go\n")
	}
	return b.String()
}

// TestHealthModal_ViewNeverExceedsHeight: the rendered line count must be ≤
// height for every combination of height and modal state, and the footer (or
// action-key hints) must always be present so the user is never trapped.
func TestHealthModal_ViewNeverExceedsHeight(t *testing.T) {
	// Build a loaded modal with a long report body (40+ lines) and showActions=true.
	report := branchhealth.HealthReport{
		Feature: "feature",
		Target:  "main",
		Behind:  5,
		Ahead:   2,
	}
	m := loadedModal(t, report)
	// Inject a long reportText to exercise the height-bounding path.
	m.reportText = longReportText(40)
	// showActions=true: behind>0, no conflicts, not dirty, feature≠target.
	m.showActions = true

	// Minimum usable height = 4 (modal chrome) + 5 (fixed overhead) + 2 (body
	// minimum for windowLines to fit hint) = 11. Use 12 as the floor so there
	// is at least 1 report line + the scroll hint visible.
	for _, h := range []int{12, 15, 20, 30, 40} {
		v := m.View(100, h)
		lines := strings.Count(v, "\n") + 1
		if lines > h {
			t.Errorf("h=%d: View produced %d lines (must be ≤ height)\n%s", h, lines, v)
		}
		// The action footer ("any key to close") must always be visible — it is
		// the only way to dismiss the modal and must never clip off-screen.
		if !strings.Contains(v, "any key") {
			t.Errorf("h=%d: footer 'any key' not found in output (clipped off):\n%s", h, v)
		}
	}
}

// TestHealthModal_ViewActionKeysNeverExceedsHeight: same invariant but for the
// showActions=true branch which shows [r]/[m] action hints instead of the plain
// footer — those must also remain visible at every height.
func TestHealthModal_ViewActionKeysNeverExceedsHeight(t *testing.T) {
	report := branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 5}
	m := loadedModal(t, report)
	m.reportText = longReportText(40)
	m.showActions = true

	for _, h := range []int{12, 15, 20, 30, 40} {
		v := m.View(100, h)
		lines := strings.Count(v, "\n") + 1
		if lines > h {
			t.Errorf("h=%d: View produced %d lines (must be ≤ height)\n%s", h, lines, v)
		}
		// Both the action keys AND the footer hint must be present.
		if !strings.Contains(v, "rebase") || !strings.Contains(v, "merge") {
			t.Errorf("h=%d: action keys [r]/[m] not found (clipped):\n%s", h, v)
		}
		if !strings.Contains(v, "any key") {
			t.Errorf("h=%d: footer 'any key' not found (clipped):\n%s", h, v)
		}
	}
}

// TestHealthModal_ViewConflictHintNeverExceedsHeight: conflict-path warning
// branch — hint must stay visible.
func TestHealthModal_ViewConflictHintNeverExceedsHeight(t *testing.T) {
	report := branchhealth.HealthReport{
		Feature:       "feature",
		Target:        "main",
		Behind:        3,
		ConflictPaths: []string{"a.go", "b.go"},
	}
	m := loadedModal(t, report)
	m.reportText = longReportText(40)

	for _, h := range []int{12, 15, 20, 30} {
		v := m.View(100, h)
		lines := strings.Count(v, "\n") + 1
		if lines > h {
			t.Errorf("h=%d: View produced %d lines\n%s", h, lines, v)
		}
		if !strings.Contains(v, "resolve manually") {
			t.Errorf("h=%d: conflict hint missing\n%s", h, v)
		}
		if !strings.Contains(v, "any key") {
			t.Errorf("h=%d: footer missing\n%s", h, v)
		}
	}
}

// TestHealthModal_ViewScrollHintAppearsWhenTruncated: when the report is longer
// than the body budget, the ↓ N more scroll hint must appear so the user knows
// content was truncated.
func TestHealthModal_ViewScrollHintAppearsWhenTruncated(t *testing.T) {
	report := branchhealth.HealthReport{Feature: "feature", Target: "main"}
	m := loadedModal(t, report)
	m.reportText = longReportText(40) // 40+ lines, will be truncated at small heights
	m.showActions = false

	// At h=15 the body budget is at most 15-9=6 lines; 40-line report must be truncated.
	v := m.View(100, 15)
	if !strings.Contains(v, "more") {
		t.Errorf("h=15: expected a scroll hint (↓ N more) when report is truncated:\n%s", v)
	}
}

func TestHealthModal_CancelConfirmWithN(t *testing.T) {
	m := loadedModal(t, branchhealth.HealthReport{Feature: "feature", Target: "main", Behind: 1})
	um, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = um.(*HealthCheckModal)
	um, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = um.(*HealthCheckModal)
	if cmd != nil {
		t.Error("n in confirm should emit no cmd")
	}
	if m.confirming != "" {
		t.Errorf("confirming should be cleared, got %q", m.confirming)
	}
	if strings.Contains(m.View(100, 40), "rewrites history") {
		t.Error("confirm prompt should be gone after cancel")
	}
}
