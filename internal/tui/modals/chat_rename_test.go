package modals

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestRenameModalInitialValuePrefilled(t *testing.T) {
	m := NewChatRenameModal("s1", "old name")
	if m.input.Value() != "old name" {
		t.Errorf("input value=%q, want 'old name'", m.input.Value())
	}
}

func TestRenameModalEnterEmitsSubmit(t *testing.T) {
	m := NewChatRenameModal("s1", "old name")
	m.input.SetValue("new name")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("nil cmd on enter")
	}
	submit, ok := cmd().(ChatRenameSubmitMsg)
	if !ok {
		t.Fatalf("got %T, want ChatRenameSubmitMsg", cmd())
	}
	if submit.SessionID != "s1" || submit.Name != "new name" {
		t.Errorf("submit=%+v", submit)
	}
}

func TestRenameModalEnterEmptyNameSetsError(t *testing.T) {
	m := NewChatRenameModal("s1", "")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	mm := updated.(*ChatRenameModal)
	if cmd != nil {
		// cmd may be non-nil if textinput returns a blink; just assert it's NOT a submit
		if msg := cmd(); msg != nil {
			if _, ok := msg.(ChatRenameSubmitMsg); ok {
				t.Errorf("submitted with empty name")
			}
		}
	}
	if mm.err == nil {
		t.Errorf("err not set on empty submit")
	}
}

func TestRenameModalEscEmitsClose(t *testing.T) {
	m := NewChatRenameModal("s1", "")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("nil cmd on esc")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("got %T, want CloseMsg", cmd())
	}
}

func TestRenameModalErrorMsgUpdatesState(t *testing.T) {
	m := NewChatRenameModal("s1", "x")
	updated, _ := m.Update(ChatRenameErrorMsg{Err: errors.New("boom")})
	mm := updated.(*ChatRenameModal)
	if mm.err == nil || mm.err.Error() != "boom" {
		t.Errorf("err=%v, want 'boom'", mm.err)
	}
}
