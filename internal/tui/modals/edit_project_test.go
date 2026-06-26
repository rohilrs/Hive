package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEditProjectModalPreFillsFields(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	if got := m.form.name.Value(); got != "Hive" {
		t.Errorf("name field=%q want Hive", got)
	}
	if got := m.form.repo.Value(); got != "/tmp/hive" {
		t.Errorf("repo_path field=%q want /tmp/hive", got)
	}
	view := m.View(80, 60)
	// Target branch is always shown (incl. manual); only the policy is sequenced-only.
	for _, want := range []string{"Hive", "/tmp/hive", "hive", "[manual]", "Target branch"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q; got:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Advancement policy") {
		t.Errorf("manual mode should not show the advancement-policy field; got:\n%s", view)
	}
}

func TestEditProjectModalSubmitManual(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	m.form.name.SetValue("Hive 2")
	m.form.repo.SetValue("/tmp/hive2")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("ctrl+s should emit cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg type %T want SubmitRequest", cmd())
	}
	if req.Kind != "edit_project" {
		t.Errorf("Kind=%q want edit_project", req.Kind)
	}
	if req.Params["slug"] != "hive" || req.Params["name"] != "Hive 2" || req.Params["repo_path"] != "/tmp/hive2" {
		t.Errorf("params wrong: %#v", req.Params)
	}
	if req.Params["dispatch_mode"] != "manual" {
		t.Errorf("dispatch_mode=%v want manual", req.Params["dispatch_mode"])
	}
	// target_branch is always sent now (the [scheduler] base, not sequenced-only).
	if _, ok := req.Params["target_branch"]; !ok {
		t.Errorf("manual submit must carry target_branch (always sent): %#v", req.Params)
	}
}

func TestEditProjectModalSeedsSequenced(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "human_merge", "", "merge", false, false, true)
	if !m.form.sequenced() {
		t.Fatalf("dispatchMode=%d want sequenced", m.form.dispatchMode)
	}
	if policyChoice(m.form.policy) != "human_merge" {
		t.Errorf("policy=%q want human_merge", policyChoice(m.form.policy))
	}
	if m.form.target.Value() != "staging" {
		t.Errorf("target=%q want staging", m.form.target.Value())
	}
	view := m.View(80, 60)
	for _, want := range []string{"[sequenced]", "Target branch", "staging", "Advancement policy", "[human_merge]"} {
		if !strings.Contains(view, want) {
			t.Errorf("sequenced view missing %q; got:\n%s", want, view)
		}
	}
}

// TestEditProjectModalKeepsSequencedWhenGateFails confirms that an
// already-sequenced project whose roadmap vanished (canSequence=false) keeps
// "sequenced" selected — the form must NOT silently auto-correct it to manual.
func TestEditProjectModalKeepsSequencedWhenGateFails(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "human_merge", "", "merge", false, false, false)
	if !m.form.sequenced() {
		t.Fatalf("sequenced must stay selected even when canSequence=false; dispatchMode=%d", m.form.dispatchMode)
	}
	view := m.View(80, 60)
	if !strings.Contains(view, "sequenced") {
		t.Errorf("view should still show sequenced (greyed); got:\n%s", view)
	}
}

func TestEditProjectModalModeCycleRevealsFields(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	// Tab past name(0) + repo(1) + status(2) to the dispatch-mode slot (3).
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.form.focusedIsSelector() {
		t.Fatalf("expected dispatch-mode selector focused after 3 tabs")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if dispatchModeChoice(m.form.dispatchMode) != "auto_all" {
		t.Errorf("after 1 right: %q want auto_all", dispatchModeChoice(m.form.dispatchMode))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if dispatchModeChoice(m.form.dispatchMode) != "sequenced" {
		t.Errorf("after 2 right: %q want sequenced", dispatchModeChoice(m.form.dispatchMode))
	}
	if !strings.Contains(m.View(80, 60), "Target branch") {
		t.Error("cycling to sequenced should reveal target field")
	}
}

func TestEditProjectModalSubmitSequenced(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "auto_merge_on_green", "", "merge", false, false, true)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("want SubmitRequest, got %T", cmd())
	}
	if req.Params["dispatch_mode"] != "sequenced" {
		t.Errorf("dispatch_mode=%v want sequenced", req.Params["dispatch_mode"])
	}
	if req.Params["target_branch"] != "staging" {
		t.Errorf("target_branch=%v want staging", req.Params["target_branch"])
	}
	if req.Params["policy"] != "auto_merge_on_green" {
		t.Errorf("policy=%v want auto_merge_on_green", req.Params["policy"])
	}
}

func TestEditProjectModalEnterOnSelectorCyclesNotSubmits(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	// Edit ring (with status): name(0) repo(1) status(2) mode(3) … — 3 tabs to mode.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab}) // focus mode slot (3)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if dispatchModeChoice(m.form.dispatchMode) != "auto_all" {
		t.Errorf("Enter on mode slot should cycle; mode=%q", dispatchModeChoice(m.form.dispatchMode))
	}
	if cmd != nil {
		if _, ok := cmd().(SubmitRequest); ok {
			t.Error("Enter on selector slot must not submit")
		}
	}
}

func TestEditProjectModalStatusSelectorRoundTrips(t *testing.T) {
	// Prefill paused → the status selector seeds to "paused" and the view shows it.
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "paused", "manual", "", "", "", "merge", false, false, true)
	if statusOptions[m.form.status] != "paused" {
		t.Fatalf("status seeded=%q want paused", statusOptions[m.form.status])
	}
	if !strings.Contains(m.View(80, 60), "Status:") {
		t.Errorf("edit modal view should show the Status field")
	}
	// Cycle status to "archived" (3 tabs reach the status slot: name→repo→status).
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyTab}) // status slot (index 2)
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if statusOptions[m.form.status] != "archived" {
		t.Fatalf("after right from paused: status=%q want archived", statusOptions[m.form.status])
	}
	// Submit carries the chosen status.
	m.form.name.SetValue("Hive")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	req := cmd().(SubmitRequest)
	if req.Params["status"] != "archived" {
		t.Errorf("submit status=%v want archived", req.Params["status"])
	}
}

// TestNewProjectModalHasNoStatusField pins that the status selector is edit-only
// (new projects are always active — no need to expose the selector).
func TestNewProjectModalHasNoStatusField(t *testing.T) {
	m := NewNewProject()
	if strings.Contains(m.View(80, 60), "Status:") {
		t.Errorf("new-project modal must NOT show the Status field")
	}
}

func TestEditProjectModalRejectsEmptyName(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	m.form.name.SetValue("")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !strings.Contains(m.form.errMsg, "name is required") {
		t.Errorf("errMsg=%q want 'name is required'", m.form.errMsg)
	}
	if cmd != nil {
		if _, ok := cmd().(SubmitRequest); ok {
			t.Error("empty name should not emit SubmitRequest")
		}
	}
	if m.form.submitting {
		t.Error("empty name should not flip submitting")
	}
}

func TestEditProjectModalEscEmitsClose(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}

func TestEditProjectModalClosesOnSuccess(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	_, cmd := m.Update(RPCResultMsg{Kind: "edit_project", Err: nil})
	if cmd == nil {
		t.Fatal("expected close cmd on success")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("expected CloseMsg, got %T", cmd())
	}
}

func TestEditProjectModalSeedsAndRoundTripsIntegration(t *testing.T) {
	// Seeded [integration] settings: feature_branch=spec/x, merge_method=squash,
	// task_auto_integrate=on, auto_fix_ci=off.
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "spec/x", "squash", true, false, true)

	// Pre-fill: state reflects the seeded values.
	if m.form.feature.Value() != "spec/x" {
		t.Errorf("feature=%q want spec/x", m.form.feature.Value())
	}
	if mergeMethods[m.form.mergeMethod] != "squash" {
		t.Errorf("mergeMethod=%q want squash", mergeMethods[m.form.mergeMethod])
	}
	if m.form.autoIntegrate != 1 {
		t.Errorf("autoIntegrate=%d want 1 (on)", m.form.autoIntegrate)
	}
	if m.form.autoFixCI != 0 {
		t.Errorf("autoFixCI=%d want 0 (off)", m.form.autoFixCI)
	}

	// View renders the integration block (always present, even non-sequenced).
	view := m.View(80, 60)
	for _, want := range []string{"Feature branch", "spec/x", "Merge method", "squash", "Auto-integrate tasks", "Auto-fix CI"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q; got:\n%s", want, view)
		}
	}

	// Submit carries the integration params with the seeded values.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("ctrl+s should emit cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg type %T want SubmitRequest", cmd())
	}
	if req.Params["feature_branch"] != "spec/x" {
		t.Errorf("feature_branch=%v want spec/x", req.Params["feature_branch"])
	}
	if req.Params["merge_method"] != "squash" {
		t.Errorf("merge_method=%v want squash", req.Params["merge_method"])
	}
	if req.Params["task_auto_integrate"] != true {
		t.Errorf("task_auto_integrate=%v want true", req.Params["task_auto_integrate"])
	}
	if req.Params["auto_fix_ci"] != false {
		t.Errorf("auto_fix_ci=%v want false", req.Params["auto_fix_ci"])
	}
}

// TestEditProjectModalIntegrationSelectorCycles drives the focus ring to the
// merge_method selector and cycles it, confirming the integration selectors are
// reachable + cycle correctly in the non-sequenced ring.
func TestEditProjectModalIntegrationSelectorCycles(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "manual", "", "", "", "merge", false, false, true)
	// Non-sequenced edit ring (with status): name(0) repo(1) status(2) mode(3)
	// feature(4) target(5) merge_method(6) auto_integrate(7) auto_fix_ci(8).
	// Tab to merge_method.
	for i := 0; i < 6; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if !m.form.focusedIsSelector() {
		t.Fatalf("expected merge_method selector focused after 6 tabs")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if mergeMethods[m.form.mergeMethod] != "squash" {
		t.Errorf("after right: merge=%q want squash", mergeMethods[m.form.mergeMethod])
	}
	// Auto-integrate toggle next.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.form.autoIntegrate != 1 {
		t.Errorf("auto-integrate=%d want 1 (on)", m.form.autoIntegrate)
	}
}

// TestEditProjectModalFitsHeight verifies the modal in sequenced mode (all ~9
// fields) never overflows a short height and KEEPS its footer + bottom border.
func TestEditProjectModalFitsHeight(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "human_merge", "spec/x", "squash", true, false, true)
	view := m.View(80, 16)
	lines := strings.Split(view, "\n")
	if len(lines) > 16 {
		t.Errorf("rendered %d lines, want <= 16; got:\n%s", len(lines), view)
	}
	if !strings.Contains(view, "submit") {
		t.Errorf("footer (submit) clipped; got:\n%s", view)
	}
	if !strings.Contains(view, "╰") {
		t.Errorf("bottom border ╰ missing (modal clipped); got:\n%s", view)
	}
}

// TestEditProjectModalFocusedFieldVisible advances focus to the LAST field and
// confirms its label is auto-scrolled into the windowed view.
func TestEditProjectModalFocusedFieldVisible(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "human_merge", "spec/x", "squash", true, false, true)
	// Drive focus to the last slot (Auto-fix CI) via tab. The form clamps +
	// wraps the focus ring, so tab len(fields) times lands back on the first;
	// instead loop until the view shows the last field focused.
	n := len(m.form.fields())
	for i := 0; i < n-1; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	view := m.View(80, 16)
	if !strings.Contains(view, "Auto-fix CI") {
		t.Errorf("focused last field (Auto-fix CI) not visible after auto-scroll; got:\n%s", view)
	}
}

func TestEditProjectModalStaysOpenOnEnableGateError(t *testing.T) {
	// The enable-gate failure (mode=sequenced, no roadmap) comes back as the
	// edit_project RPC error — the modal must show it inline and STAY OPEN.
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "human_merge", "", "merge", false, false, true)
	m2, cmd := m.Update(RPCResultMsg{Kind: "edit_project", Err: errors.New("cannot enable sequenced dispatch: roadmap not found")})
	if !strings.Contains(m2.(*EditProjectModal).form.errMsg, "roadmap not found") {
		t.Errorf("gate error not shown; errMsg=%q", m2.(*EditProjectModal).form.errMsg)
	}
	if cmd != nil {
		if _, ok := cmd().(CloseMsg); ok {
			t.Error("modal must NOT close on enable-gate error")
		}
	}
	// Bottom border must still be present (modal must not be clipped).
	view := m2.(*EditProjectModal).View(80, 20)
	if !strings.Contains(view, "╰") {
		t.Errorf("bottom border ╰ missing after enable-gate error; got:\n%s", view)
	}
}

// TestEditProjectModalFitsHeightWithLongError verifies that a long enable-gate
// error message (>54 chars, wraps at inner width) does not inflate fieldBudget
// and clip the bottom border.
func TestEditProjectModalFitsHeightWithLongError(t *testing.T) {
	m := NewEditProjectModal("hive", "Hive", "/tmp/hive", "active", "sequenced", "staging", "human_merge", "", "merge", false, false, true)
	m.form.errMsg = "enable gate failed: roadmap and current-phase spec required before sequenced dispatch can be turned on"
	view := m.View(80, 20)
	lines := strings.Split(view, "\n")
	if len(lines) > 20 {
		t.Errorf("rendered %d lines, want <= 20; got:\n%s", len(lines), view)
	}
	if !strings.Contains(view, "╰") {
		t.Errorf("bottom border ╰ missing (modal clipped by long error); got:\n%s", view)
	}
}
