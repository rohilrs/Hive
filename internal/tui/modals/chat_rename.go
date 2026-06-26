package modals

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// ChatRenameSubmitMsg fires when the user hits Enter with a non-empty name.
// Root forwards this to the daemon's chat.set_name RPC.
type ChatRenameSubmitMsg struct {
	SessionID string
	Name      string
}

// ChatRenameErrorMsg lets the root surface a failed rename in the modal
// without closing it, so the user can correct + retry.
type ChatRenameErrorMsg struct {
	Err error
}

// ChatRenameModal is a single-textinput modal for renaming a chat session.
type ChatRenameModal struct {
	sessionID string
	input     textinput.Model
	err       error
}

func NewChatRenameModal(sessionID, currentName string) *ChatRenameModal {
	ti := textinput.New()
	ti.Placeholder = "session name"
	ti.CharLimit = 200
	ti.SetValue(currentName)
	ti.Focus()
	return &ChatRenameModal{sessionID: sessionID, input: ti}
}

func (m *ChatRenameModal) Title() string { return "Rename chat session" }
func (m *ChatRenameModal) Init() tea.Cmd { return textinput.Blink }

func (m *ChatRenameModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "enter":
			name := m.input.Value()
			if name == "" {
				m.err = errEmptyName
				return m, nil
			}
			return m, func() tea.Msg {
				return ChatRenameSubmitMsg{SessionID: m.sessionID, Name: name}
			}
		}
	}
	if errMsg, ok := msg.(ChatRenameErrorMsg); ok {
		m.err = errMsg.Err
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *ChatRenameModal) View(width, height int) string {
	body := m.input.View()
	if m.err != nil {
		body += "\n" + style.ErrorStyle.Render(m.err.Error())
	}
	hint := style.DimText.Render("\nEnter to save · Esc to cancel")
	return body + hint
}

// errEmptyName is a sentinel for the in-modal "name required" hint.
var errEmptyName = renameError("name cannot be empty")

type renameError string

func (e renameError) Error() string { return string(e) }
