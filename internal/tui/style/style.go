// Package style centralizes lipgloss styles for the TUI so per-tab
// code doesn't redefine colors / borders. Adjust here to retheme.
package style

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

// Status color helpers — used by run rows + stage rows.
var (
	Running        = lipgloss.NewStyle().Foreground(lipgloss.Color("#00d7ff")) // cyan
	Done           = lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff5f")) // green
	NeedsAttention = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffd700")) // yellow
	ErrorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5f5f")) // red
	Pending        = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")) // gray
	Header         = lipgloss.NewStyle().Bold(true).Underline(true)
	DimText        = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
)

// Chat speaker headers — bold-colored to scan; body stays default-color.
// Used by internal/tui/tabs/chat.go to delineate turn ownership.
var (
	ChatYou   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00d7ff")) // cyan
	ChatHive  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ff79c6")) // magenta-pink
	ChatToolL = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))            // dim, for "· tool_name " label
)

// Tab indicator styles.
var (
	TabActive   = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("#00d7ff"))
	TabInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("#666666"))
)

// Daemon-down banner.
var DaemonDownBanner = lipgloss.NewStyle().
	Background(lipgloss.Color("#ff0000")).
	Foreground(lipgloss.Color("#ffffff")).
	Bold(true).
	Padding(0, 1)

// Panel borders.
var (
	Panel      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	PanelFocus = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#00d7ff")).Padding(0, 1)
)

// Modal chrome — rounded border + padding. Modals compose their own
// content; this style is the wrapper. Width is set per-modal via
// Modal.Width(w) at render time.
var Modal = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	Padding(1, 2)

// ModalTitle is the bold heading rendered at the top of a modal body.
var ModalTitle = lipgloss.NewStyle().Bold(true)

// Hint is the readable alias for DimText. Use Hint for short footer
// help lines (e.g. "enter submit · esc cancel"); use DimText for
// general dimmed body content. Both produce identical output today.
var Hint = DimText

// InlineError is the readable alias for ErrorStyle when rendering an
// error message inside a modal body (or a row that briefly shows an
// error before reverting). ErrorStyle is the long-standing name; new
// callers should prefer InlineError for clarity.
var InlineError = ErrorStyle

// Key styles a single keybind glyph (e.g. "q", "ctrl+k") so the key
// reads as the active element and the description fades. Use in
// footer + per-tab key-help lines.
var Key = lipgloss.NewStyle().Bold(true)

// Danger styles a destructive verb in a confirm modal so the operator
// sees the action's gravity before pressing y. Aliases ErrorStyle for
// now; a future theme could diverge (e.g. red+underline) without
// touching call sites.
var Danger = ErrorStyle

// CursorMarker returns the 2-cell row prefix used by list-style tabs
// to mark the current cursor row. Active marker is "▸ " colored via
// Running; inactive is "  " (two spaces) so column alignment is
// preserved across rows.
//
// Standardize all tabs on this helper instead of ad-hoc literals so
// the cursor glyph and color stay coherent.
func CursorMarker(active bool) string {
	if active {
		return Running.Render("▸ ")
	}
	return "  "
}

// ScrollHint renders a "↑ N more" or "↓ N more" indicator in dim
// style to surface that more content exists above/below the visible
// window. Returns "" when count == 0 so callers can unconditionally
// include it. direction must be "up" or "down".
func ScrollHint(direction string, count int) string {
	if count <= 0 {
		return ""
	}
	var arrow string
	switch direction {
	case "up":
		arrow = "↑"
	case "down":
		arrow = "↓"
	default:
		return ""
	}
	return DimText.Render(fmt.Sprintf("%s %d more", arrow, count))
}

// ForStatus returns a style appropriate for a run/task status string.
func ForStatus(status string) lipgloss.Style {
	switch status {
	case "running":
		return Running
	case "done":
		return Done
	case "needs_attention":
		return NeedsAttention
	case "error":
		return ErrorStyle
	case "pending":
		return Pending
	default:
		return DimText
	}
}

// GateGlyph maps a sequenced task's gate_state to a compact 1-rune glyph with a
// status-appropriate color, for the Sequence modal's per-task list and the
// Projects-tab badge. Unknown/none gates render as a dim open circle.
func GateGlyph(gate string) string {
	switch gate {
	case "satisfied":
		return Done.Render("✓")
	case "skipped":
		return DimText.Render("⊘")
	case "awaiting_merge":
		return Running.Render("◑")
	case "merge_failed":
		return Danger.Render("✗")
	case "pr_open":
		return Running.Render("◔")
	case "built":
		return Running.Render("▣")
	default: // none
		return DimText.Render("◯")
	}
}
