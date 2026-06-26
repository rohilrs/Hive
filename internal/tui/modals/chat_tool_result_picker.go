package modals

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// pickerState is the modal's two-state machine: a list (statePicking)
// and a scrollable inspector (stateViewing) of the selected row.
type pickerState int

const (
	statePicking pickerState = iota
	stateViewing
)

// Viewport padding around the inspector. Header + blank + footer + blank
// rows consume 6 vertical lines; horizontal chrome (border + padding)
// consumes 4 columns. Centralized so the resize handler and the Enter
// transition stay in lockstep.
const (
	pickerVpHPad = 6
	pickerVpWPad = 4
)

// ChatToolResultRow is one entry in the picker — mirrors a tool_result
// frame's salient fields. The chat tab collects these from its
// chatFrameView.Result slots; the root constructs the modal with the
// rows (most-recent-first).
type ChatToolResultRow struct {
	Tool    string
	Result  string
	IsError bool
}

// ChatToolResultPicker is a two-state modal: a list (picking) and a
// scrollable inspector (viewing) of the selected row's body. Mirrors
// task_detail.go's multi-state pattern.
//
// Keys (picking): up/k, down/j, enter (open inspector), esc (close).
// Keys (viewing): esc (back to list), q (close), up/down/PgUp/PgDn
// (scroll viewport).
type ChatToolResultPicker struct {
	rows     []ChatToolResultRow
	cursor   int
	state    pickerState
	viewport viewport.Model
	width    int
	height   int
}

// NewChatToolResultPicker constructs the picker with the given rows
// (most-recent-first). Width/height come from the root's window dims.
func NewChatToolResultPicker(rows []ChatToolResultRow, width, height int) *ChatToolResultPicker {
	return &ChatToolResultPicker{
		rows:   rows,
		state:  statePicking,
		width:  width,
		height: height,
	}
}

func (m *ChatToolResultPicker) Title() string { return "Tool results" }

func (m *ChatToolResultPicker) Init() tea.Cmd { return nil }

// Width / Height expose the modal's current dimensions for tests verifying
// the root forwards tea.WindowSizeMsg correctly.
func (m *ChatToolResultPicker) Width() int  { return m.width }
func (m *ChatToolResultPicker) Height() int { return m.height }

// viewportDims returns the (width, height) the inspector viewport should
// be sized to given the current modal dimensions. Padding factored into
// pickerVpWPad / pickerVpHPad constants; clamps to sane minimums so the
// viewport stays usable on small terminals.
func (m *ChatToolResultPicker) viewportDims() (int, int) {
	vpW := m.width - pickerVpWPad
	if vpW < 20 {
		vpW = 20
	}
	vpH := m.height - pickerVpHPad
	if vpH < 3 {
		vpH = 3
	}
	return vpW, vpH
}

func (m *ChatToolResultPicker) Update(msg tea.Msg) (Modal, tea.Cmd) {
	// Handle terminal resizes so the inspector viewport doesn't wedge at
	// stale dims after Enter (the viewport was previously sized once at
	// the Enter transition and never updated).
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		m.height = ws.Height
		if m.state == stateViewing {
			vpW, vpH := m.viewportDims()
			m.viewport.Width = vpW
			m.viewport.Height = vpH
		}
		return m, nil
	}

	key, ok := msg.(tea.KeyMsg)
	if !ok {
		if m.state == stateViewing {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	if m.state == stateViewing {
		switch key.String() {
		case "esc":
			m.state = statePicking
			// Drop the body so it's GC-able while back in the list; the
			// next Enter rebuilds the viewport with fresh content.
			m.viewport.SetContent("")
			return m, nil
		case "q":
			return m, func() tea.Msg { return CloseMsg{} }
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	// picking state
	switch key.String() {
	case "esc":
		return m, func() tea.Msg { return CloseMsg{} }
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "enter":
		if len(m.rows) == 0 {
			return m, nil
		}
		// Initialize viewport with the selected row's body.
		vpW, vpH := m.viewportDims()
		m.viewport = viewport.New(vpW, vpH)
		m.viewport.SetContent(m.rows[m.cursor].Result)
		m.state = stateViewing
	}
	return m, nil
}

func (m *ChatToolResultPicker) View(width, height int) string {
	if m.state == stateViewing && m.cursor >= 0 && m.cursor < len(m.rows) {
		header := style.ChatHive.Render(fmt.Sprintf("%s (result %d/%d)",
			m.rows[m.cursor].Tool, m.cursor+1, len(m.rows)))
		footer := style.DimText.Render("esc back · q close · ↑↓/PgUp/PgDn scroll")
		return header + "\n\n" + m.viewport.View() + "\n\n" + footer
	}

	// picking state
	header := style.ChatHive.Render("Tool results — pick one to inspect")
	footer := style.DimText.Render("↑↓/jk nav · enter inspect · esc close")
	if len(m.rows) == 0 {
		return header + "\n\n" + style.DimText.Render("(no tool results in this session yet)") + "\n\n" + footer
	}

	var body strings.Builder
	for i, r := range m.rows {
		marker := "  "
		if i == m.cursor {
			marker = "> "
		}
		glyph := "✓"
		if r.IsError {
			glyph = "✗"
		}
		preview := r.Result
		if rs := []rune(preview); len(rs) > 60 {
			preview = string(rs[:60]) + "…"
		}
		// Most-recent-first comes from the input slice ordering; the
		// numbering shown matches that (top row is the highest index).
		fmt.Fprintf(&body, "%s%d. %-20s %s  %s\n", marker, len(m.rows)-i, r.Tool, glyph, preview)
	}
	return header + "\n\n" + body.String() + "\n" + footer
}
