package modals

import (
	"encoding/json"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// ChatEditArgsSubmitMsg is emitted by ChatEditArgsModal on a successful
// Ctrl-S. The root handles it by sending a chat.confirm RPC with
// Approve=true and EditedInput set.
type ChatEditArgsSubmitMsg struct {
	ToolCallID string
	EditedArgs json.RawMessage
}

// ChatEditArgsModal lets the user edit the JSON args for a pending
// tool_proposed before approving it. Pre-filled with the model's
// proposed args (pretty-printed). Ctrl-S validates + submits; esc
// closes; invalid JSON renders an inline error and keeps the modal
// open so the user doesn't lose their edits.
type ChatEditArgsModal struct {
	toolName   string
	toolCallID string
	textarea   textarea.Model
	inlineErr  string
}

// NewChatEditArgsModal constructs the modal pre-filled with pretty-
// printed args. Width/height are passed in so the textarea can size
// itself within the modal chrome.
func NewChatEditArgsModal(toolName, toolCallID string, args json.RawMessage, width, height int) *ChatEditArgsModal {
	ta := textarea.New()
	// Pretty-print: Unmarshal then MarshalIndent. On parse error,
	// fall back to the raw text so the user can at least see what
	// was proposed.
	var anyVal any
	pretty := string(args)
	if err := json.Unmarshal(args, &anyVal); err == nil {
		if b, err := json.MarshalIndent(anyVal, "", "  "); err == nil {
			pretty = string(b)
		}
	}
	ta.SetValue(pretty)
	// Size: width minus modal chrome ~4 columns; height min(10, terminal-6).
	taWidth := width - 4
	if taWidth < 20 {
		taWidth = 20
	}
	taHeight := 10
	if height > 0 && height-6 < 10 {
		taHeight = height - 6
		if taHeight < 3 {
			taHeight = 3
		}
	}
	ta.SetWidth(taWidth)
	ta.SetHeight(taHeight)
	ta.Focus()
	return &ChatEditArgsModal{
		toolName:   toolName,
		toolCallID: toolCallID,
		textarea:   ta,
	}
}

func (m *ChatEditArgsModal) Title() string { return "Edit args for " + m.toolName }

func (m *ChatEditArgsModal) Init() tea.Cmd {
	return textarea.Blink
}

func (m *ChatEditArgsModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}
	switch key.Type {
	case tea.KeyEsc:
		return m, func() tea.Msg { return CloseMsg{} }
	case tea.KeyCtrlS:
		// Validate as a JSON object — match the daemon's defensive
		// check so the user catches the error client-side.
		var probe map[string]any
		raw := []byte(m.textarea.Value())
		if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
			m.inlineErr = "invalid JSON object: " + errString(err)
			return m, nil
		}
		m.inlineErr = ""
		submit := ChatEditArgsSubmitMsg{ToolCallID: m.toolCallID, EditedArgs: raw}
		return m, tea.Batch(
			func() tea.Msg { return submit },
			func() tea.Msg { return CloseMsg{} },
		)
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m *ChatEditArgsModal) View(width, height int) string {
	body := m.textarea.View()
	footer := style.DimText.Render("ctrl+s submit · esc cancel")
	if m.inlineErr != "" {
		return body + "\n" + style.ErrorStyle.Render(m.inlineErr) + "\n" + footer
	}
	return body + "\n" + footer
}

// errString returns "non-object value" when err is nil (probe==nil case)
// to make the inline error coherent when the JSON parsed but produced
// nil (json "null").
func errString(err error) string {
	if err == nil {
		return "got null or non-object value"
	}
	return err.Error()
}
