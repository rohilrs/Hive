package modals

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// SessionPickerLoadMsg is the request emitted by the picker's Init() that
// the root must satisfy by calling Client.ChatHistoryList and pushing the
// result back via SessionPickerLoadResultMsg.
type SessionPickerLoadMsg struct{}

// SessionPickerLoadResultMsg is the root's reply to SessionPickerLoadMsg —
// either the loaded rows or a transport/RPC error.
type SessionPickerLoadResultMsg struct {
	Rows []SessionRow
	Err  error
}

// SessionPickerResumeMsg is what the picker emits when the user hits Enter.
// The root handles it by setting the chat tab's sessionID + switching the
// active tab AND closing the modal.
//
// Carries the row's persisted metadata (Name/Provider/TotalCostUSD) so
// the chat tab can render the metadata bar immediately on resume instead
// of waiting for the next turn_done frame.
type SessionPickerResumeMsg struct {
	SessionID    string
	Name         string
	Provider     string
	TotalCostUSD float64
}

// NewChatSessionMsg is emitted by the picker when the user selects the
// pinned "+ New session" entry OR presses Ctrl-N inside the picker. The
// root handles it by resetting the chat tab to a fresh empty session and
// closing the modal.
type NewChatSessionMsg struct{}

// newSessionSentinel is the synthetic sentinel row pinned at index 0.
// Its empty ID lets the picker distinguish "+ New session" from a real
// session row on Enter / `d` keybinds. Created here (not in SetRows) so
// the entry is selectable during the initial loading state too.
var newSessionSentinel = SessionRow{ID: "", Name: "+ New session"}

// ChatSessionDeleteRequestMsg is emitted by the picker after the user
// confirms a `d` keybind. The root handles it by calling Client.ChatDelete
// (which evicts daemon-side state too) and reloads the picker.
type ChatSessionDeleteRequestMsg struct {
	SessionID string
	Name      string // for the toast/error context; the daemon never sees it
}

// ChatSessionDeleteResultMsg is the root's reply after the delete RPC
// returns. Picker uses it to apply the row removal or render an error.
type ChatSessionDeleteResultMsg struct {
	SessionID string
	Err       error
}

// SessionRow is the modal's render-friendly mirror of a daemon
// chat_session row. The root converts Client.ChatSessionRow into this
// type so the modals package doesn't import client types.
type SessionRow struct {
	ID           string
	Surface      string
	StartedAt    int64
	EndedAt      int64
	TotalCostUSD float64
	Name         string
	Provider     string
}

// ChatSessionPicker is a modal that lets the user browse and resume a past
// chat session. The root opens it via Ctrl-K.
type ChatSessionPicker struct {
	rows    []SessionRow
	cursor  int
	loading bool
	err     error
	// confirming is set when the user presses `d` on a real session row;
	// the next `y` confirms the delete, `n` / esc cancels.
	confirming bool
}

// NewChatSessionPicker creates a picker in its initial loading state.
// The "+ New session" sentinel row is pinned at index 0 from the start
// so the user can create a fresh session even before the past-sessions
// load completes (and so the picker is never empty/unselectable).
func NewChatSessionPicker() *ChatSessionPicker {
	return &ChatSessionPicker{loading: true, rows: []SessionRow{newSessionSentinel}}
}

func (m *ChatSessionPicker) Title() string { return "Resume chat session" }

// Init emits SessionPickerLoadMsg so the root kicks off the async load.
func (m *ChatSessionPicker) Init() tea.Cmd {
	return func() tea.Msg { return SessionPickerLoadMsg{} }
}

// SetRows stores the loaded sessions and clears the loading state.
// The "+ New session" sentinel is re-prepended so it stays at index 0
// regardless of what the daemon returned.
func (m *ChatSessionPicker) SetRows(rows []SessionRow) {
	merged := make([]SessionRow, 0, len(rows)+1)
	merged = append(merged, newSessionSentinel)
	merged = append(merged, rows...)
	m.rows = merged
	m.loading = false
}

// SetError stores a load error and clears the loading state.
func (m *ChatSessionPicker) SetError(err error) { m.err = err; m.loading = false }

func (m *ChatSessionPicker) Update(msg tea.Msg) (Modal, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	// Confirming-delete state: y commits, n/esc cancels. No other keys
	// are honored while confirming so accidental navigation can't slip
	// past the prompt.
	if m.confirming {
		switch key.String() {
		case "y", "Y":
			row := m.rows[m.cursor]
			m.confirming = false
			return m, func() tea.Msg {
				return ChatSessionDeleteRequestMsg{SessionID: row.ID, Name: row.Name}
			}
		case "n", "N", "esc":
			m.confirming = false
		}
		return m, nil
	}

	switch key.String() {
	case "esc":
		return m, func() tea.Msg { return CloseMsg{} }
	case "ctrl+n":
		// Ctrl-N anywhere in the picker creates a fresh session — the
		// pinned "+ New session" entry covers discoverability; this
		// covers muscle-memory.
		return m, func() tea.Msg { return NewChatSessionMsg{} }
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "d":
		// Delete keybind: only valid on real session rows. The sentinel
		// "+ New session" entry (ID=="") can't be deleted.
		if m.cursor < 0 || m.cursor >= len(m.rows) {
			return m, nil
		}
		if m.rows[m.cursor].ID == "" {
			return m, nil
		}
		m.confirming = true
		return m, nil
	case "enter":
		if len(m.rows) == 0 {
			return m, nil
		}
		row := m.rows[m.cursor]
		if row.ID == "" {
			// Sentinel "+ New session" row — start a fresh session.
			return m, func() tea.Msg { return NewChatSessionMsg{} }
		}
		return m, func() tea.Msg {
			return SessionPickerResumeMsg{
				SessionID:    row.ID,
				Name:         row.Name,
				Provider:     row.Provider,
				TotalCostUSD: row.TotalCostUSD,
			}
		}
	}
	return m, nil
}

// ApplyDeletedRow removes a row by SessionID after a successful delete
// and clamps the cursor. Called by the root after Client.ChatDelete
// returns OK so the picker can update without a full reload round-trip.
// Returns true if the row was found and removed.
func (m *ChatSessionPicker) ApplyDeletedRow(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	for i, r := range m.rows {
		if r.ID == sessionID {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			if m.cursor >= len(m.rows) {
				m.cursor = len(m.rows) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			return true
		}
	}
	return false
}

func (m *ChatSessionPicker) View(width, height int) string {
	if m.loading {
		return style.DimText.Render("loading sessions…")
	}
	if m.err != nil {
		return style.ErrorStyle.Render("error: " + m.err.Error())
	}
	if len(m.rows) == 0 {
		return style.DimText.Render("(no chat sessions)")
	}
	var b []byte
	for i, r := range m.rows {
		var line string
		if r.ID == "" {
			// Sentinel "+ New session" row — render compactly, no metadata.
			line = fmt.Sprintf("%-40s", r.Name)
		} else {
			started := time.Unix(r.StartedAt, 0).Format("2006-01-02 15:04")
			status := "open"
			if r.EndedAt > 0 {
				status = "ended " + time.Unix(r.EndedAt, 0).Format("15:04")
			}
			name := r.Name
			if name == "" {
				name = "(unnamed)"
			}
			if rs := []rune(name); len(rs) > 40 {
				name = string(rs[:37]) + "..."
			}
			prov := r.Provider
			if prov == "claude-code" {
				prov = "cc"
			} else if prov == "" {
				prov = "?"
			}
			line = fmt.Sprintf("%-40s  %-3s  %s  %s  $%.4f", name, prov, started, status, r.TotalCostUSD)
		}
		line = style.CursorMarker(i == m.cursor) + line
		b = append(b, line...)
		b = append(b, '\n')
	}
	if m.confirming && m.cursor < len(m.rows) {
		row := m.rows[m.cursor]
		name := row.Name
		if name == "" {
			name = "(unnamed)"
		}
		prompt := style.NeedsAttention.Render(
			fmt.Sprintf("delete '%s'? [y/n]", name))
		b = append(b, '\n')
		b = append(b, prompt...)
	} else {
		// Key-help footer. Different hint when cursor is on the
		// sentinel "+ New session" row (delete is meaningless there).
		help := "↑↓/jk nav · enter resume · ctrl+n new · d delete · esc close"
		if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].ID == "" {
			help = "↑↓/jk nav · enter new · ctrl+n new · esc close"
		}
		b = append(b, '\n')
		b = append(b, style.DimText.Render(help)...)
	}
	return string(b)
}
