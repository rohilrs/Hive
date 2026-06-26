package modals

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// DecomposeConfirmModal renders the proposed-subtasks list returned by
// roadmap.decompose for one phase, and gates batch insertion behind a
// y/N confirm. On approval, the modal emits a SubmitRequest the root
// translates into a loop of task.add RPCs with metadata linking each
// inserted task back to its source phase + roadmap doc.
//
// Workflow:
//
//	NewDecomposeConfirmModal(...) ──► [list]
//	  list:
//	    y         → emit SubmitRequest{Kind:"roadmap_insert_tasks"}.
//	                submitting=true; modal stays open while root loops
//	                task.add per subtask.
//	    n / esc   → CloseMsg (no insert).
//	  RPCResultMsg{Kind:"roadmap_insert_tasks"}:
//	    Err == nil → CloseMsg (done).
//	    Err != nil → submitting=false; render errMsg inline; operator
//	                 can retry (y) or cancel (esc).
//
// Per-task confirm is intentionally NOT implemented in V1 — matches the
// CLI's batch behavior (cmd_roadmap.go's single y/N prompts the full
// list). Per-task gating is reserved as a future polish.
type DecomposeConfirmModal struct {
	projectSlug string

	// phase fields are passed through verbatim into the task.add
	// metadata map at submit time, so each inserted task carries a
	// stable link back to the source roadmap phase.
	phaseNumber string
	phaseTitle  string
	roadmapPath string
	specPaths   []string

	// subtasks is the LLM-proposed list. Order matches the daemon's
	// response (which mirrors the model's output order — typically
	// dependency-ordered).
	subtasks []decompose.ProposedSubtask

	// Two-pane state: cursor selects the proposal (left pane); focusRight
	// chooses which pane ↑/↓ act on (false=list, true=detail); detailScroll is
	// the right (detail) scroll offset, reset whenever the selection changes.
	cursor       int
	focusRight   bool
	detailScroll int

	submitting bool
	errMsg     string
}

// NewDecomposeConfirmModal builds the modal from a roadmap.decompose RPC
// response. All fields are copied so the modal's lifetime is independent
// of the caller's response struct.
func NewDecomposeConfirmModal(
	projectSlug, phaseNumber, phaseTitle, roadmapPath string,
	specPaths []string,
	subtasks []decompose.ProposedSubtask,
) *DecomposeConfirmModal {
	specCopy := append([]string(nil), specPaths...)
	subtaskCopy := append([]decompose.ProposedSubtask(nil), subtasks...)
	return &DecomposeConfirmModal{
		projectSlug: projectSlug,
		phaseNumber: phaseNumber,
		phaseTitle:  phaseTitle,
		roadmapPath: roadmapPath,
		specPaths:   specCopy,
		subtasks:    subtaskCopy,
	}
}

func (m *DecomposeConfirmModal) Title() string {
	// Match the CLI's heading style — "Phase <N>: <title>". Subtask count
	// appears in the body, not the title, to keep the title short.
	return "Phase " + m.phaseNumber + ": " + m.phaseTitle
}

func (m *DecomposeConfirmModal) Init() tea.Cmd { return nil }

func (m *DecomposeConfirmModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case RPCResultMsg:
		if msg.Kind != "roadmap_insert_tasks" {
			return m, nil
		}
		if msg.Err != nil {
			m.submitting = false
			m.errMsg = msg.Err.Error()
			return m, nil
		}
		// Success — root inserted (some or all) tasks. Close even on
		// partial success; the operator can re-run from the viewer if
		// any inserts failed (root may surface those via errMsg here in
		// a future polish, but V1 trusts the response).
		return m, func() tea.Msg { return CloseMsg{} }

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "n":
			return m, func() tea.Msg { return CloseMsg{} }
		case "tab":
			m.focusRight = !m.focusRight
			return m, nil
		case "up", "k":
			if m.focusRight {
				if m.detailScroll > 0 {
					m.detailScroll--
				}
			} else if m.cursor > 0 {
				m.cursor--
				m.detailScroll = 0
			}
			return m, nil
		case "down", "j":
			if m.focusRight {
				m.detailScroll++ // View clamps to the detail window
			} else if m.cursor < len(m.subtasks)-1 {
				m.cursor++
				m.detailScroll = 0
			}
			return m, nil
		case "y":
			if m.submitting {
				// In-flight — protect against double-y rapid-fire.
				return m, nil
			}
			if len(m.subtasks) == 0 {
				m.errMsg = "no subtasks to insert"
				return m, nil
			}
			m.submitting = true
			// Snapshot the fields the closure needs so a subsequent
			// modal state-change can't race the cmd.
			slug := m.projectSlug
			subtasks := m.subtasks
			phaseNumber := m.phaseNumber
			phaseTitle := m.phaseTitle
			roadmapPath := m.roadmapPath
			specPath := ""
			if len(m.specPaths) > 0 {
				// First linked spec is the primary, matching the CLI's
				// behavior in cmd_roadmap.go::runRoadmapDecompose.
				specPath = m.specPaths[0]
			}
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind: "roadmap_insert_tasks",
					Params: map[string]any{
						"project_slug": slug,
						"subtasks":     subtasks,
						"phase":        phaseNumber,
						"phase_title":  phaseTitle,
						"roadmap_path": roadmapPath,
						"spec_path":    specPath,
					},
				}
			}
		}
	}
	return m, nil
}

func (m *DecomposeConfirmModal) View(width, height int) string {
	w := frameWidth(width)
	budget := w - 8 // two panel chromes (border 2 + padding 2 each)
	if budget < 24 {
		budget = 24
	}

	// Header: phase heading + count · roadmap path.
	header := []string{
		style.ModalTitle.Render(truncateRunes(m.Title(), budget)),
		style.DimText.Render(truncateRunes(formatProposalCount(len(m.subtasks))+" · Roadmap: "+m.roadmapPath, budget)),
	}
	// Footer: error inline + key hint.
	var footer []string
	if m.errMsg != "" {
		footer = append(footer, style.InlineError.Render(truncateRunes(m.errMsg, budget)))
	}
	if m.submitting {
		footer = append(footer, style.Hint.Render("inserting…"))
	} else {
		nav := "select"
		if m.focusRight {
			nav = "scroll detail"
		}
		footer = append(footer, style.Hint.Render(
			style.Key.Render("y")+" insert all · "+
				style.Key.Render("tab")+" switch · "+
				style.Key.Render("↑↓")+" "+nav+" · "+
				style.Key.Render("n")+"/"+style.Key.Render("esc")+" cancel"))
	}

	// Pane vertical budget: total minus header, footer, the two blank
	// separators (2), the modal chrome (4), and the panel borders (2).
	bodyH := height - len(header) - len(footer) - 8
	if bodyH < 3 {
		bodyH = 3
	}
	leftContentW := budget * 2 / 5
	if leftContentW < 12 {
		leftContentW = 12
	}
	rightContentW := budget - leftContentW
	if rightContentW < 12 {
		rightContentW = 12
	}

	// Left pane: one row per proposal, cursor-marked, auto-scrolled.
	rows := make([]string, len(m.subtasks))
	for i, st := range m.subtasks {
		// -4: panel padding (2) + cursor marker (2).
		row := leftRow(i+1, st, leftContentW-4)
		if i == m.cursor {
			rows[i] = style.CursorMarker(true) + style.ModalTitle.Render(row)
		} else {
			rows[i] = style.CursorMarker(false) + row
		}
	}
	leftWin := windowLines(rows, bodyH, leftScrollFirst(m.cursor, len(rows), bodyH))

	// Right pane: the selected proposal's full detail, wrapped + scrolled.
	var rightWin []string
	if m.cursor >= 0 && m.cursor < len(m.subtasks) {
		rightWin = windowLines(proposalDetailLines(m.subtasks[m.cursor], rightContentW-2), bodyH, m.detailScroll)
	}

	leftStyle, rightStyle := style.PanelFocus, style.Panel
	if m.focusRight {
		leftStyle, rightStyle = style.Panel, style.PanelFocus
	}
	leftBox := leftStyle.Width(leftContentW).Height(bodyH).Render(strings.Join(leftWin, "\n"))
	rightBox := rightStyle.Width(rightContentW).Height(bodyH).Render(strings.Join(rightWin, "\n"))
	panes := lipgloss.JoinHorizontal(lipgloss.Top, leftBox, rightBox)

	return style.Modal.Width(w).Render(strings.Join(
		[]string{strings.Join(header, "\n"), panes, strings.Join(footer, "\n")}, "\n\n"))
}

// formatProposalCount renders the "N proposals" header. Pluralizes on N>1
// to keep the modal readable without a count assertion at the call site.
func formatProposalCount(n int) string {
	if n == 1 {
		return "1 proposal"
	}
	return strconv.Itoa(n) + " proposals"
}

// leftRow formats one proposal as a single left-pane list row:
// "N. [priority] title" truncated to width. When the subtask has a MergeFrom
// ref (existing-work reconciliation), "  ← merges <ref>" is appended before
// truncation so the operator sees the merge intent at a glance.
func leftRow(index int, st decompose.ProposedSubtask, width int) string {
	priority := st.Priority
	if priority == "" {
		priority = "P3"
	}
	label := strconv.Itoa(index) + ". [" + priority + "] " + st.Title
	if st.MergeFrom != "" {
		label += "  ← merges " + st.MergeFrom
	}
	return truncateRunes(label, width)
}

// proposalDetailLines builds the right-pane detail for a proposal: the title
// (wrapped + bold), a priority/pipeline meta line, then the full body wrapped
// to width (so nothing is cut off).
func proposalDetailLines(st decompose.ProposedSubtask, width int) []string {
	priority := st.Priority
	if priority == "" {
		priority = "P3"
	}
	pipeline := st.Pipeline
	if pipeline == "" {
		pipeline = "build"
	}
	var out []string
	for _, row := range wrapToWidth(st.Title, width) {
		out = append(out, style.ModalTitle.Render(row))
	}
	out = append(out, style.DimText.Render("["+priority+" / "+pipeline+"]"), "")
	if strings.TrimSpace(st.Body) == "" {
		out = append(out, "(no description)")
		return out
	}
	for _, line := range strings.Split(strings.TrimRight(st.Body, "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapToWidth(line, width)...)
	}
	return out
}
