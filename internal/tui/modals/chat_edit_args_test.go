package modals

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewChatEditArgsModalPrefillsPrettyJSON(t *testing.T) {
	m := NewChatEditArgsModal("hive_add_task", "tc1", json.RawMessage(`{"title":"hi","priority":"P3"}`), 80, 24)
	got := m.textarea.Value()
	if !strings.Contains(got, `"title": "hi"`) {
		t.Errorf("expected pretty-printed title field with space-colon-space; got %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("expected multi-line indentation; got %q", got)
	}
}

func TestChatEditArgsModalCtrlSValidJSONEmitsSubmit(t *testing.T) {
	m := NewChatEditArgsModal("hive_add_task", "tc1", json.RawMessage(`{"title":"hi"}`), 80, 24)
	m.textarea.SetValue(`{"title":"edited"}`)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("expected submit cmd on ctrl+s")
	}
	msgs := unfoldBatch(cmd)
	var submit *ChatEditArgsSubmitMsg
	var sawClose bool
	for _, msg := range msgs {
		switch v := msg.(type) {
		case ChatEditArgsSubmitMsg:
			submit = &v
		case CloseMsg:
			sawClose = true
		}
	}
	if submit == nil {
		t.Fatal("missing ChatEditArgsSubmitMsg")
	}
	if !sawClose {
		t.Errorf("missing CloseMsg — submit should also close the modal")
	}
	if submit.ToolCallID != "tc1" {
		t.Errorf("ToolCallID=%q, want tc1", submit.ToolCallID)
	}
	var got map[string]any
	if err := json.Unmarshal(submit.EditedArgs, &got); err != nil {
		t.Fatalf("submit.EditedArgs not valid JSON: %s err=%v", submit.EditedArgs, err)
	}
	if got["title"] != "edited" {
		t.Errorf("submit args title=%v, want 'edited'", got["title"])
	}
	_ = updated
}

func TestChatEditArgsModalCtrlSInvalidJSONShowsInlineError(t *testing.T) {
	m := NewChatEditArgsModal("hive_add_task", "tc1", json.RawMessage(`{"title":"hi"}`), 80, 24)
	m.textarea.SetValue(`{not json`)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	pk := updated.(*ChatEditArgsModal)
	if pk.inlineErr == "" {
		t.Errorf("inlineErr should be set on invalid JSON")
	}
	if cmd != nil {
		msgs := unfoldBatch(cmd)
		for _, msg := range msgs {
			if _, ok := msg.(ChatEditArgsSubmitMsg); ok {
				t.Errorf("invalid JSON should not emit submit")
			}
			if _, ok := msg.(CloseMsg); ok {
				t.Errorf("invalid JSON should not close")
			}
		}
	}
}

func TestChatEditArgsModalEscClosesWithoutSubmit(t *testing.T) {
	m := NewChatEditArgsModal("hive_add_task", "tc1", json.RawMessage(`{"title":"hi"}`), 80, 24)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected close cmd on esc")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}

// unfoldBatch executes a tea.Cmd and collects all emitted messages.
// tea.BatchMsg is []tea.Cmd in bubbletea v1.3.x — each element is run
// and its message collected. For a simple single-msg cmd, returns the
// one msg.
func unfoldBatch(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			if c != nil {
				out = append(out, c())
			}
		}
		return out
	}
	return []tea.Msg{msg}
}
