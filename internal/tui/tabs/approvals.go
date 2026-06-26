package tabs

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// ApprovalsReader is the narrow snapshot contract the Approvals tab needs.
type ApprovalsReader interface {
	PendingApprovals() []PendingApproval
}

// Approvals is the tiered pending-approval inbox (Phase 4.6). Workers
// blocked on an unmatched tool (ask mode) show here; the operator
// approves/denies (optionally remembering a rule).
type Approvals struct {
	snap   ApprovalsReader
	cursor int
	width  int
	height int
}

func NewApprovals(snap ApprovalsReader) *Approvals { return &Approvals{snap: snap} }

func (a *Approvals) Name() string  { return "Approvals" }
func (a *Approvals) Init() tea.Cmd { return nil }
func (a *Approvals) KeyHelp() string {
	return "↑↓ select · a approve · A approve+remember · d deny · D deny+remember · j jump to run"
}

func (a *Approvals) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
	case tea.KeyMsg:
		items := a.snap.PendingApprovals()
		switch msg.String() {
		case "up":
			if a.cursor > 0 {
				a.cursor--
			}
		case "down":
			if a.cursor < len(items)-1 {
				a.cursor++
			}
		case "a", "enter":
			return a, a.resolve(items, "approve", false)
		case "A":
			return a, a.resolve(items, "approve", true)
		case "d":
			return a, a.resolve(items, "deny", false)
		case "D":
			return a, a.resolve(items, "deny", true)
		case "j":
			if a.cursor < len(items) {
				rid := items[a.cursor].RunID
				return a, func() tea.Msg { return DrillInRequest{RunID: rid} }
			}
		}
	}
	return a, nil
}

// resolve emits a TabApprovalResolveRequest for the cursor item. When
// remember is set on a Bash tool, it derives a glob from the command's
// first token (e.g. "make all" -> "make *").
func (a *Approvals) resolve(items []PendingApproval, decision string, remember bool) tea.Cmd {
	if a.cursor >= len(items) {
		return nil
	}
	it := items[a.cursor]
	argMatcher := ""
	if remember && it.ToolName == "Bash" {
		argMatcher = firstToken(it.Arg) + " *"
	}
	return func() tea.Msg {
		return TabApprovalResolveRequest{
			ApprovalID: it.ApprovalID, Decision: decision, Remember: remember,
			ToolName: it.ToolName, ArgMatcher: argMatcher,
		}
	}
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func (a *Approvals) View() string {
	items := a.snap.PendingApprovals()
	if len(items) == 0 {
		return style.Hint.Render("No pending approvals — workers proceed when their tool calls match policy; unmatched ones wait here for your decision.")
	}
	if a.cursor >= len(items) {
		a.cursor = len(items) - 1
	}
	var b strings.Builder
	b.WriteString(style.Header.Render(fmt.Sprintf("Pending approvals (%d)", len(items))) + "\n\n")
	// Items are pre-sorted by task title; print a header at each task
	// boundary so the inbox is organized by the task awaiting approval.
	currentTask := ""
	for i, it := range items {
		if it.TaskTitle != currentTask {
			if currentTask != "" {
				b.WriteString("\n")
			}
			b.WriteString(style.Header.Render(it.TaskTitle) + style.DimText.Render("  ("+it.Stage+")") + "\n")
			currentTask = it.TaskTitle
		}
		marker := style.CursorMarker(i == a.cursor)
		line := fmt.Sprintf("%s[%-7s] %-10s  %s", marker, it.Tier, it.ToolName, truncateArg(it.Arg, 80))
		if i == a.cursor {
			line = style.Running.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func truncateArg(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
