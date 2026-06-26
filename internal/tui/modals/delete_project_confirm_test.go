package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeString feeds each rune of s into the modal one keystroke at a time.
// Mirrors the bubbletea key model — the textinput consumes runes via
// tea.KeyRunes msgs the same way `tea.KeyMsg{Runes: []rune{r}}` would
// arrive from the terminal.
func typeString(t *testing.T, m *DeleteProjectConfirmModal, s string) {
	t.Helper()
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestDeleteProjectConfirmEnterRejectedWithoutMatchingSlug(t *testing.T) {
	m := NewDeleteProjectConfirmModal("hive", 3, 7)
	// Press enter without typing anything in the slug input.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.errMsg, "does not match") {
		t.Errorf("errMsg=%q want 'typed slug does not match...'", m.errMsg)
	}
	if m.submitting {
		t.Errorf("submitting should stay false when slug doesn't match")
	}
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(SubmitRequest); ok {
				t.Errorf("enter without matching slug must NOT emit SubmitRequest")
			}
		}
	}
}

func TestDeleteProjectConfirmEnterEmitsRequestWithMatchingSlug(t *testing.T) {
	m := NewDeleteProjectConfirmModal("hive", 3, 7)
	typeString(t, m, "hive")
	if m.typedSlug.Value() != "hive" {
		t.Fatalf("setup: typedSlug=%q want hive", m.typedSlug.Value())
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter after matching slug should emit cmd")
	}
	if !m.submitting {
		t.Errorf("submitting should flip to true on a valid submit")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "delete_project" {
		t.Errorf("Kind=%q want delete_project", req.Kind)
	}
	if req.Params["slug"] != "hive" {
		t.Errorf("slug=%v want hive", req.Params["slug"])
	}
}

// Regression for the bug found in 8.C.2 T6 smoke: a slug containing 'y'
// couldn't be typed because the modal previously intercepted 'y' as
// confirm. Confirm key is now enter/ctrl+s so slug chars pass through
// to the textinput cleanly.
func TestDeleteProjectConfirmSlugWithYCanBeTyped(t *testing.T) {
	m := NewDeleteProjectConfirmModal("my-app", 0, 0)
	typeString(t, m, "my-app")
	if m.typedSlug.Value() != "my-app" {
		t.Errorf("typedSlug=%q want 'my-app' (slug containing y should type cleanly)", m.typedSlug.Value())
	}
	// Confirm via enter — the 'y' inside the slug already passed through.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter after fully-typed matching slug should emit cmd")
	}
	if !m.submitting {
		t.Errorf("submitting should flip to true for slug with y")
	}
}

func TestDeleteProjectConfirmEscEmitsClose(t *testing.T) {
	m := NewDeleteProjectConfirmModal("hive", 0, 0)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}

func TestDeleteProjectConfirmRPCErrorShownInline(t *testing.T) {
	m := NewDeleteProjectConfirmModal("hive", 0, 0)
	// Simulate a submit (typed slug + enter).
	typeString(t, m, "hive")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.submitting {
		t.Fatal("setup: should be submitting after enter")
	}
	m2, _ := m.Update(RPCResultMsg{Kind: "delete_project", Err: errors.New("project has running children")})
	mm := m2.(*DeleteProjectConfirmModal)
	if !strings.Contains(mm.errMsg, "running children") {
		t.Errorf("errMsg=%q want to contain 'running children'", mm.errMsg)
	}
	if mm.submitting {
		t.Errorf("submitting should reset to false on error so user can retry")
	}
}

func TestDeleteProjectConfirmSuccessClosesModal(t *testing.T) {
	m := NewDeleteProjectConfirmModal("hive", 0, 0)
	typeString(t, m, "hive")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := m.Update(RPCResultMsg{Kind: "delete_project", Err: nil})
	if cmd == nil {
		t.Fatal("success result should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("success should emit CloseMsg; got %T", cmd())
	}
}

func TestDeleteProjectConfirmShowsCascadeCounts(t *testing.T) {
	m := NewDeleteProjectConfirmModal("hive", 12, 34)
	view := m.View(80, 24)
	if !strings.Contains(view, "12") {
		t.Errorf("view should contain task count 12; got:\n%s", view)
	}
	if !strings.Contains(view, "34") {
		t.Errorf("view should contain run count 34; got:\n%s", view)
	}
	if !strings.Contains(view, "Delete") {
		t.Errorf("view should contain the destructive verb 'Delete'; got:\n%s", view)
	}
	if !strings.Contains(view, "hive") {
		t.Errorf("view should contain the slug; got:\n%s", view)
	}
}
