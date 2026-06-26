package modals

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestConfirmAbandonYEmitsSubmit(t *testing.T) {
	m := NewConfirmAbandon("run-x").(*ConfirmAbandon)
	_, cmd := m.Update(tea.KeyMsg{Runes: []rune{'y'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("y should submit")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got %T", cmd())
	}
	if req.Params["run_id"] != "run-x" {
		t.Errorf("missing run_id")
	}
}

func TestConfirmAbandonNCloses(t *testing.T) {
	m := NewConfirmAbandon("run-x").(*ConfirmAbandon)
	_, cmd := m.Update(tea.KeyMsg{Runes: []rune{'n'}, Type: tea.KeyRunes})
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("n should close")
	}
}
