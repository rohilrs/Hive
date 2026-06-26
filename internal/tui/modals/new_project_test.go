package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewProjectRequiresSlugAndName(t *testing.T) {
	m := NewNewProject()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.form.errMsg, "required") {
		t.Errorf("errMsg=%q want 'required'", m.form.errMsg)
	}
}

func TestNewProjectSubmitsManual(t *testing.T) {
	m := NewNewProject()
	m.form.slug.SetValue("hive")
	m.form.name.SetValue("Hive")
	m.form.repo.SetValue("/tmp/hive")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("submit should emit cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg type %T want SubmitRequest", cmd())
	}
	if req.Params["slug"] != "hive" || req.Params["name"] != "Hive" {
		t.Errorf("params wrong: %#v", req.Params)
	}
	if req.Params["dispatch_mode"] != "manual" {
		t.Errorf("dispatch_mode=%v want manual (default)", req.Params["dispatch_mode"])
	}
	// target_branch is always sent now (the [scheduler] base, not sequenced-only).
	if _, ok := req.Params["target_branch"]; !ok {
		t.Errorf("manual submit must carry target_branch (always sent): %#v", req.Params)
	}
	// Integration params always serialize.
	if _, ok := req.Params["feature_branch"]; !ok {
		t.Errorf("integration params missing from submit: %#v", req.Params)
	}
}

// TestNewProjectSequencedDisabled confirms the new-project modal cannot cycle
// the dispatch-mode selector onto the disabled "sequenced" option (a brand-new
// project has no roadmap, so canSequence is false).
func TestNewProjectSequencedDisabled(t *testing.T) {
	m := NewNewProject()
	m.form.slug.SetValue("hive")
	m.form.name.SetValue("Hive")
	// Tab past slug(0)/name(1)/repo(2) to the dispatch-mode slot (3).
	for i := 0; i < 3; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
	if !m.form.focusedIsSelector() {
		t.Fatalf("expected dispatch-mode selector focused after 3 tabs")
	}
	// Cycle all the way around — sequenced must never become selected.
	for i := 0; i < len(dispatchModes)+1; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRight})
		if m.form.sequenced() {
			t.Fatalf("cycling reached disabled sequenced option")
		}
	}
}

// TestNewProjectFitsHeight verifies the new-project modal never overflows a
// short height and keeps its footer + bottom border.
func TestNewProjectFitsHeight(t *testing.T) {
	m := NewNewProject()
	m.form.slug.SetValue("hive")
	m.form.name.SetValue("Hive")
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

func TestNewProjectClosesOnSuccessfulResult(t *testing.T) {
	m := NewNewProject()
	_, cmd := m.Update(RPCResultMsg{Kind: "new_project", Err: nil})
	if cmd == nil {
		t.Fatal("expected close cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("expected CloseMsg, got %T", cmd())
	}
}

func TestNewProjectShowsErrorOnFailedResult(t *testing.T) {
	m := NewNewProject()
	m2, _ := m.Update(RPCResultMsg{Kind: "new_project", Err: errors.New("slug exists")})
	if !strings.Contains(m2.(*NewProject).form.errMsg, "slug exists") {
		t.Errorf("error not shown")
	}
}

// TestNewProjectFitsHeightWithLongError verifies that a long error message
// (>54 chars, wraps at the modal's inner width) does not cause the rendered
// output to exceed the given height or clip the bottom border ╰.
func TestNewProjectFitsHeightWithLongError(t *testing.T) {
	m := NewNewProject()
	m.form.slug.SetValue("hive")
	m.form.name.SetValue("Hive")
	// Inject a long error (95 chars) — wraps to 2 visual lines at innerW=54.
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
