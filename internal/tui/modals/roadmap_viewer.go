package modals

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// roadmapViewerMaxBytes caps how much of a roadmap file we read into memory
// for parsing. Real planner-produced roadmaps are <10 KB; the cap exists so
// a pathological huge file doesn't block the UI thread during NewRoadmapViewerModal.
const roadmapViewerMaxBytes = 256 * 1024

// RoadmapViewerModal is a read-only viewer of the planner-written roadmap
// at `<repo>/docs/superpowers/roadmaps/<slug>.md`. The modal parses the
// file synchronously on construction via internal/roadmap.Parse and shows
// each phase as a navigable row.
//
// Keys:
//
//	↑/k        move phase cursor up
//	↓/j        move phase cursor down
//	D          (capital) emit a decompose request for the cursored phase —
//	           root closes this modal, fires roadmap.decompose, and opens
//	           DecomposeConfirmModal with the proposals.
//	esc        close.
//
// Error cases (missing file, parse failure) render an inline error +
// keep esc working so the operator is never trapped.
type RoadmapViewerModal struct {
	projectSlug string
	repoPath    string

	// roadmap is the parsed document. nil when load/parse failed; the
	// err field carries the user-visible reason. Decompose key (D) is a
	// no-op while roadmap is nil.
	roadmap *roadmap.Roadmap

	phaseCursor int

	// focusRight selects which pane ↑/↓ (and j/k) act on: false = the phase
	// list (left), true = the detail body (right). Tab toggles it. Default
	// left so phase navigation works without first focusing. (Two-pane viewer)
	focusRight bool

	// detailScroll is the right-pane (phase body) scroll offset in lines from
	// the top. Reset to 0 whenever the phase selection changes. Update only
	// inc/decrements (loosely bounded by the body length); View does the
	// precise clamp so it never mutates state.
	detailScroll int

	// decomposing is set true on D while the (multi-second) roadmap.decompose
	// RPC runs, so the footer can show progress. The root swaps this modal out
	// when the result arrives. decomposingPhase is the phase number being run.
	decomposing      bool
	decomposingPhase string

	// phaseLabel is the coarse progress label from the latest decompose.progress
	// event (e.g. "preparing codebase context", "running model"), shown in the
	// decomposing footer so the operator sees the async job advance. Empty until
	// the first progress event; cleared when a new decompose starts.
	phaseLabel string

	// decomposeErr is the inline footer message when a roadmap.decompose RPC
	// fails, times out, or returns no proposals. Unlike `err` it does NOT
	// early-return the view — the phase list stays visible so the operator can
	// read the reason and re-press D. Cleared when a new decompose starts.
	decomposeErr string

	// err is the inline-rendered error message when load/parse failed
	// (file missing, invalid markdown, etc.). Empty on success.
	err string

	// loading is true when the working-tree read missed and we're awaiting the
	// daemon's branch-aware roadmap.content fetch (shared repo whose working tree
	// is on another branch). The root fires the fetch and calls SetContent/
	// SetError when it returns. While loading, the view shows a spinner instead
	// of the "no roadmap" error so a branch-only roadmap isn't reported missing.
	loading bool

	// syncing is set true on L while the (multi-second) roadmap.sync_linear RPC
	// runs; syncMsg holds the terminal result/error line shown in the footer.
	// Distinct from the decompose spinner so the two actions never clobber each
	// other's status.
	syncing bool
	syncMsg string

	width, height int
}

// NewRoadmapViewerModal reads + parses the roadmap synchronously. The
// constructor MUST NOT block on the network — only on a single os.ReadFile
// call (capped to roadmapViewerMaxBytes). Failure modes:
//
//   - File missing → err = "no roadmap at <path> — run `hive plan <slug>` first"
//   - Parse error  → err = "parse error: <reason>"
//
// In both cases the modal opens with roadmap=nil; the user sees the error +
// can press esc.
func NewRoadmapViewerModal(projectSlug, repoPath string) *RoadmapViewerModal {
	m := &RoadmapViewerModal{
		projectSlug: projectSlug,
		repoPath:    repoPath,
	}
	roadmapPath := filepath.Join(repoPath, "docs", "superpowers", "roadmaps", projectSlug+".md")
	body, err := os.ReadFile(roadmapPath)
	if err != nil {
		// Working-tree miss: the roadmap may live on the project's feature branch
		// while this (shared) repo is checked out elsewhere. Defer to the daemon's
		// branch-aware fetch — the root sees NeedsContent() and calls SetContent/
		// SetError. Do NOT report "missing" yet.
		m.loading = true
		return m
	}
	// V1 cap so a pathological file doesn't block the UI thread on a
	// huge slice. Real roadmaps are <10 KB; this cap is well beyond
	// anything the planner produces in practice.
	if len(body) > roadmapViewerMaxBytes {
		body = body[:roadmapViewerMaxBytes]
	}
	rm, perr := roadmap.Parse(body)
	if perr != nil {
		m.err = "parse error: " + perr.Error()
		return m
	}
	m.roadmap = rm
	return m
}

func (m *RoadmapViewerModal) Title() string { return "Roadmap: " + m.projectSlug }

// Init returns nil — no async work; the parse happens in the constructor.
func (m *RoadmapViewerModal) Init() tea.Cmd { return nil }

// NeedsContent reports that the working-tree read missed and the modal is
// awaiting the daemon's branch-aware roadmap.content fetch (the root fires it).
func (m *RoadmapViewerModal) NeedsContent() bool { return m.loading }

// Slug is the project this viewer shows, so the root can match a late-arriving
// content fetch to the still-open modal.
func (m *RoadmapViewerModal) Slug() string { return m.projectSlug }

// SetContent parses daemon-fetched roadmap markdown (clearing the loading
// state). Mirrors the constructor's cap + parse.
func (m *RoadmapViewerModal) SetContent(markdown string) {
	m.loading = false
	body := []byte(markdown)
	if len(body) > roadmapViewerMaxBytes {
		body = body[:roadmapViewerMaxBytes]
	}
	rm, perr := roadmap.Parse(body)
	if perr != nil {
		m.err = "parse error: " + perr.Error()
		return
	}
	m.roadmap = rm
	m.err = ""
}

// SetError records a failed content fetch (clearing loading) so the viewer shows
// the reason instead of an indefinite spinner.
func (m *RoadmapViewerModal) SetError(msg string) {
	m.loading = false
	m.err = msg
}

func (m *RoadmapViewerModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case RPCResultMsg:
		// The root forwards roadmap.decompose lifecycle here. Three shapes:
		//
		//   1. async START — Data["_decomposing"]==true: the start RPC acked and
		//      the job is registered; keep/refresh the spinner while we wait for
		//      the decompose.proposed/failed event. (D already set decomposing,
		//      but this also handles a re-entry / label reset.)
		//   2. PROGRESS — Data["_decomposing_label"] set: update the footer label.
		//   3. TERMINAL failure / empty — Err set (or no proposals): drop the
		//      spinner + surface the reason inline so the operator can re-press D.
		//      (Success swaps in the confirm modal at the root, never reaching here.)
		if msg.Kind == "roadmap_decompose_open" {
			if label, ok := msg.Data["_decomposing_label"].(string); ok && label != "" {
				m.decomposing = true
				m.phaseLabel = label
				return m, nil
			}
			if dec, _ := msg.Data["_decomposing"].(bool); dec && msg.Err == nil {
				m.decomposing = true
				m.decomposeErr = ""
				m.phaseLabel = ""
				if ph, ok := msg.Data["phase"].(string); ok && ph != "" {
					m.decomposingPhase = ph
				}
				return m, nil
			}
			m.decomposing = false
			m.phaseLabel = ""
			if msg.Err != nil {
				m.decomposeErr = "decompose failed: " + msg.Err.Error()
			} else {
				m.decomposeErr = "decompose returned no proposals — adjust the phase/spec and retry"
			}
		}
		// Terminal result of an L → roadmap.sync_linear round-trip. On success
		// report the milestone count; on error surface the reason. The daemon
		// no-ops (0 milestones) when the project's Linear source isn't write-back
		// bound, so call that out specifically rather than implying success.
		if msg.Kind == "roadmap_sync_linear" {
			m.syncing = false
			if msg.Err != nil {
				m.syncMsg = "✗ Linear sync failed: " + msg.Err.Error()
			} else {
				n := 0
				if v, ok := msg.Data["milestones"].(float64); ok {
					n = int(v)
				}
				if n == 0 {
					m.syncMsg = "Linear sync: 0 milestones — is the Linear source write-back bound?"
				} else {
					m.syncMsg = fmt.Sprintf("✓ synced %d milestone(s) to Linear", n)
				}
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "tab":
			// Toggle focus between the phase list (left) and the body (right).
			m.focusRight = !m.focusRight
			return m, nil
		case "up", "k":
			if m.roadmap == nil {
				return m, nil
			}
			if m.focusRight {
				if m.detailScroll > 0 {
					m.detailScroll--
				}
			} else if m.phaseCursor > 0 {
				m.phaseCursor--
				m.detailScroll = 0
			}
			return m, nil
		case "down", "j":
			if m.roadmap == nil {
				return m, nil
			}
			if m.focusRight {
				if m.detailScroll < m.detailLineCount() {
					m.detailScroll++ // View clamps precisely to the pane window
				}
			} else if m.phaseCursor < len(m.roadmap.Phases)-1 {
				m.phaseCursor++
				m.detailScroll = 0
			}
			return m, nil
		case "D":
			// Decompose the cursored phase. Emits a SubmitRequest the root
			// translates into roadmap.decompose RPC + DecomposeConfirmModal.
			// No-op when the roadmap failed to load/parse OR when the
			// cursor is out of range (defensive — phases list non-empty
			// from successful Parse).
			if m.roadmap == nil || m.phaseCursor < 0 || m.phaseCursor >= len(m.roadmap.Phases) {
				return m, nil
			}
			phase := m.roadmap.Phases[m.phaseCursor]
			roadmapPath := filepath.Join(m.repoPath, "docs", "superpowers", "roadmaps", m.projectSlug+".md")
			slug := m.projectSlug
			phaseNum := phase.Number
			phaseTitle := phase.Title
			specPaths := append([]string(nil), phase.SpecPaths...)
			// Decompose calls Sonnet (a few seconds). Flag it so the footer
			// shows progress — otherwise D feels like "nothing happened" while
			// the RPC runs. The root swaps this modal out when the result lands.
			m.decomposing = true
			m.decomposingPhase = phaseNum
			m.decomposeErr = "" // clear any prior failure on a fresh attempt
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind: "roadmap_decompose_open",
					Params: map[string]any{
						"project_slug": slug,
						"repo_path":    m.repoPath,
						"phase":        phaseNum,
						"phase_title":  phaseTitle,
						"roadmap_path": roadmapPath,
						"spec_paths":   specPaths,
					},
				}
			}
		case "L":
			// Push the whole roadmap to Linear (document + per-phase milestones).
			// Whole-roadmap action, not phase-scoped, so it works even while the
			// cursor sits on any phase. No-op guard: needs a parsed roadmap.
			if m.roadmap == nil || m.syncing {
				return m, nil
			}
			slug := m.projectSlug
			m.syncing = true
			m.syncMsg = ""
			return m, func() tea.Msg {
				return SubmitRequest{
					Kind:   "roadmap_sync_linear",
					Params: map[string]any{"project_slug": slug},
				}
			}
		}
	}
	return m, nil
}

func (m *RoadmapViewerModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n\n")

	if m.loading {
		b.WriteString(style.Hint.Render("loading roadmap from the feature branch…") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	if m.err != "" {
		b.WriteString(style.InlineError.Render(m.err) + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	if m.roadmap == nil || len(m.roadmap.Phases) == 0 {
		// Defensive — Parse rejects no-phase roadmaps, so this branch
		// shouldn't reach in practice. Keep it for symmetry with err.
		b.WriteString(style.Hint.Render("no phases found in roadmap") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	// Two-pane layout: left = phase list (navigable), right = the selected
	// phase's full body. Both panes are bounded to the available height with
	// scroll affordances, so a long roadmap or phase never overflows + clips
	// the modal. Tab toggles which pane ↑/↓ scroll. (Two-pane viewer)
	w := frameWidth(width)
	// Vertical budget for the pane boxes: total height minus the title (2),
	// the footer (2), the modal frame chrome (border 2 + padding 2 = 4), and
	// the panel boxes' own border rows (2). Clamp to a usable minimum.
	bodyH := height - 10
	if bodyH < 3 {
		bodyH = 3
	}
	// Pane content widths. Each panel adds 4 cols (border 2 + padding 2); both
	// boxes must fit the modal's inner content width (w-8 for the two chromes).
	budget := w - 8
	if budget < 24 {
		budget = 24
	}
	leftContentW := budget * 2 / 5
	if leftContentW < 12 {
		leftContentW = 12
	}
	rightContentW := budget - leftContentW
	if rightContentW < 12 {
		rightContentW = 12
	}

	// Left pane: one row per phase, cursor-marked, auto-scrolled to keep the
	// selection on-screen.
	phaseRows := make([]string, len(m.roadmap.Phases))
	for i, p := range m.roadmap.Phases {
		// -4: the panel's horizontal padding (2) + the cursor marker (2). Lines
		// wider than the panel's content area would soft-wrap and overflow the
		// pane height, so truncate to fit exactly.
		label := truncateRunes("Phase "+p.Number+" — "+p.Title, leftContentW-4)
		if i == m.phaseCursor {
			phaseRows[i] = style.CursorMarker(true) + style.ModalTitle.Render(label)
		} else {
			phaseRows[i] = style.CursorMarker(false) + label
		}
	}
	leftWin := windowLines(phaseRows, bodyH, leftScrollFirst(m.phaseCursor, len(phaseRows), bodyH))

	// Right pane: the selected phase's full body + spec links, scrolled.
	// -2 for the panel's horizontal padding (prevents soft-wrap → overflow).
	rightWin := windowLines(detailLines(m.roadmap.Phases[m.phaseCursor], rightContentW-2), bodyH, m.detailScroll)

	leftStyle, rightStyle := style.PanelFocus, style.Panel
	if m.focusRight {
		leftStyle, rightStyle = style.Panel, style.PanelFocus
	}
	leftBox := leftStyle.Width(leftContentW).Height(bodyH).Render(strings.Join(leftWin, "\n"))
	rightBox := rightStyle.Width(rightContentW).Height(bodyH).Render(strings.Join(rightWin, "\n"))
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftBox, rightBox) + "\n\n")

	if m.decomposing {
		detail := "grounding + Sonnet; up to a few min on large repos"
		if m.phaseLabel != "" {
			detail = m.phaseLabel
		}
		b.WriteString(style.NeedsAttention.Render("⏳ decomposing phase " + m.decomposingPhase + "… (" + detail + ")"))
	} else if m.syncing {
		b.WriteString(style.NeedsAttention.Render("⏳ syncing roadmap to Linear…"))
	} else {
		// Surface a decompose failure above the hints (phase list stays visible
		// so the operator can re-press D after fixing the cause).
		if m.decomposeErr != "" {
			b.WriteString(style.InlineError.Render("✗ "+m.decomposeErr) + "\n")
		}
		// Surface the last Linear-sync result/error (cleared on the next sync).
		if m.syncMsg != "" {
			if strings.HasPrefix(m.syncMsg, "✗") {
				b.WriteString(style.InlineError.Render(m.syncMsg) + "\n")
			} else {
				b.WriteString(style.Done.Render(m.syncMsg) + "\n")
			}
		}
		act := "move phase"
		if m.focusRight {
			act = "scroll body"
		}
		b.WriteString(style.Hint.Render(
			style.Key.Render("tab") + " switch · " +
				style.Key.Render("↑↓") + " " + act + " · " +
				style.Key.Render("D") + " decompose · " +
				style.Key.Render("L") + " sync→linear · " +
				style.Key.Render("esc") + " close"))
	}

	return m.frame(b.String(), width)
}

// frameWidth returns the modal content width. The two-pane viewer wants room,
// so it uses most of the terminal width (minus a small margin), capped at 140
// to stay readable on ultrawide screens. Falls back to 80 before the first
// WindowSizeMsg (width==0).
func frameWidth(width int) int {
	if width <= 0 {
		return 80
	}
	w := width - 4
	if w > 140 {
		w = 140
	}
	if w < 24 {
		w = 24
	}
	return w
}

// frame wraps the modal body in the standard rounded-border + capped width.
func (m *RoadmapViewerModal) frame(content string, width int) string {
	return style.Modal.Width(frameWidth(width)).Render(content)
}

// detailLineCount upper-bounds the wrapped right-pane row count for the cursored
// phase, used by Update to stop scroll runaway. It wraps at a deliberately
// narrow width (more wrapping = more rows) so it is always ≥ the real rendered
// count at any pane width; View does the precise clamp.
func (m *RoadmapViewerModal) detailLineCount() int {
	if m.roadmap == nil || m.phaseCursor < 0 || m.phaseCursor >= len(m.roadmap.Phases) {
		return 0
	}
	return len(detailLines(m.roadmap.Phases[m.phaseCursor], 16))
}

// windowLines returns at most `height` visible lines of `lines`, starting at
// firstVisible (clamped), adding ↑/↓ scroll affordances that SHARE the height
// budget when there is off-window content. The ≤height guarantee is what keeps
// a pane from overflowing and clipping the modal. (Two-pane viewer)
func windowLines(lines []string, height, firstVisible int) []string {
	if height < 1 {
		height = 1
	}
	if len(lines) <= height {
		return lines
	}
	content := height - 2 // reserve up + down hint rows (≤1 unused when only one shows)
	if content < 1 {
		content = 1
	}
	maxFirst := len(lines) - content
	if firstVisible > maxFirst {
		firstVisible = maxFirst
	}
	if firstVisible < 0 {
		firstVisible = 0
	}
	end := firstVisible + content
	if end > len(lines) {
		end = len(lines)
	}
	win := append([]string{}, lines[firstVisible:end]...)
	if firstVisible > 0 {
		win = append([]string{style.ScrollHint("up", firstVisible)}, win...)
	}
	if end < len(lines) {
		win = append(win, style.ScrollHint("down", len(lines)-end))
	}
	return win
}

// formField is one labelled field block in a form modal. slot is the field's
// focus-ring index, used to locate the focused field's first line for auto-scroll.
type formField struct {
	slot  int
	label string
	value string
}

// buildFieldLines flattens form fields into a single []string (each field is
// "label\n  value" followed by a blank separator) and returns the index of the
// FOCUSED field's first line so the caller can keep it visible while windowing.
// focusedLine is 0 when no field matches focusIdx (defensive).
func buildFieldLines(fields []formField, focusIdx int) (lines []string, focusedLine int) {
	for _, f := range fields {
		labelLine := len(lines)
		lines = append(lines, f.label)
		lines = append(lines, "  "+f.value)
		if f.slot == focusIdx {
			// Anchor the auto-scroll on the VALUE line (label+1), not the label.
			// Each field is a 2-line block; anchoring the label put the value
			// (where the cursor / selector options live) one row past the window
			// bottom — so the focused field's value scrolled off-screen (you
			// could never see e.g. the last field's options). The value line
			// keeps the editable row on screen.
			focusedLine = labelLine + 1
		}
		lines = append(lines, "") // blank separator between fields
	}
	// Trim the trailing separator so it doesn't waste a budget row.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines, focusedLine
}

// framedModalBody composes a height-bounded form modal: a pinned title (top), a
// scrolling field section in the middle, and a pinned footer (bottom). The field
// lines are windowed via windowLines so the FOCUSED field (focusedLine = the
// index of its first line in fieldLines) stays visible, while the title + footer
// never scroll off. The whole thing is wrapped in style.Modal at the given width
// with MaxHeight(height) as a hard backstop against overflow.
//
// When height <= 0 (not yet laid out) it renders unbounded — the pre-viewport
// behavior — so callers/tests that pass height 0 still get every field.
//
// Budget math: style.Modal adds 4 chrome rows (border 2 + padding 2). The title
// occupies titleRows (measured by wrapping at innerW) + 1 blank line, and the
// footer occupies footerRows measured by rendering it at innerW — so a long
// error message that wraps to extra visual lines is counted correctly. The
// remainder is the field budget, floored at 3 so windowLines always has room
// for content + a scroll affordance.
func framedModalBody(title string, fieldLines []string, focusedLine int, footer string, width, height int) string {
	if height <= 0 {
		body := title + "\n\n" + strings.Join(fieldLines, "\n") + "\n\n" + footer
		return style.Modal.Width(width).Render(body)
	}
	const modalChrome = 4
	// Compute the inner content width (inside the modal's border + padding) so
	// we can measure how many visual rows the title and footer occupy after
	// word-wrap. This prevents a long error message from being under-counted by
	// strings.Count("\n"), which only sees literal newlines, not wrap-induced ones.
	innerW := width - style.Modal.GetHorizontalFrameSize()
	if innerW < 1 {
		innerW = 1
	}
	measureStyle := lipgloss.NewStyle().Width(innerW)
	// +1 on EACH for the blank line the body inserts around them. The body is
	// `title + "\n\n" + fields + "\n\n" + footer` — a blank after the title AND
	// before the footer. Counting only the title's blank left the body 1 row
	// taller than the budget → MaxHeight clipped the bottom border (visible once
	// the field window filled on scroll).
	footerRows := lipgloss.Height(measureStyle.Render(footer)) + 1 // +1 for the blank line before the footer
	titleRows := lipgloss.Height(measureStyle.Render(title)) + 1   // +1 for the blank line after the title
	fieldBudget := height - modalChrome - titleRows - footerRows
	if fieldBudget < 3 {
		fieldBudget = 3
	}
	win := windowLines(fieldLines, fieldBudget, leftScrollFirst(focusedLine, len(fieldLines), fieldBudget))
	// When scrolling (the fields don't fit), windowLines emits a variable row
	// count — one scroll hint at the top/bottom edges, two in the middle — which
	// made the modal grow/shrink by a row as you scrolled. Pad to the full
	// fieldBudget so the box height stays CONSTANT while scrolling. Only when
	// windowing: a short form that fits stays a short modal (no blank padding).
	if len(fieldLines) > fieldBudget {
		for len(win) < fieldBudget {
			win = append(win, "")
		}
	}
	body := title + "\n\n" + strings.Join(win, "\n") + "\n\n" + footer
	return style.Modal.Width(width).MaxHeight(height).Render(body)
}

// leftScrollFirst returns the first-visible phase index that keeps `cursor` on
// screen in a pane of `height` rows (anchors the cursor at the window bottom
// once it scrolls past the fold).
func leftScrollFirst(cursor, total, height int) int {
	if total <= height {
		return 0
	}
	content := height - 2
	if content < 1 {
		content = 1
	}
	if cursor < content {
		return 0
	}
	return cursor - content + 1
}

// detailLines builds the right-pane content for a phase: its full body (each
// line truncated to width to prevent wrap — wrapping would overflow the pane
// height) followed by a Spec-links line.
func detailLines(p roadmap.Phase, width int) []string {
	var out []string
	// Phase title header (wrapped + bold) so the full title is readable even
	// when the left-pane list truncates it.
	for _, row := range wrapToWidth("Phase "+p.Number+" — "+p.Title, width) {
		out = append(out, style.ModalTitle.Render(row))
	}
	out = append(out, "")
	// Body: wrapped (not truncated) so no line is cut off, in the terminal's
	// default foreground so it stays legible on a black background.
	for _, line := range strings.Split(strings.TrimRight(p.Body, "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapToWidth(line, width)...)
	}
	if len(p.SpecPaths) > 0 {
		out = append(out, "")
		out = append(out, wrapToWidth("Spec: "+strings.Join(p.SpecPaths, ", "), width)...)
	}
	return out
}

// wrapToWidth wraps s to at most `width` columns: on word boundaries where
// possible, hard-breaking any over-long token, so NO produced line exceeds
// width. (A produced line wider than the pane would soft-wrap in lipgloss and
// silently overflow the pane height.)
func wrapToWidth(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	return strings.Split(wrap.String(wordwrap.String(s, width), width), "\n")
}

// truncateRunes clamps s to at most max runes, appending an ellipsis when cut.
// Operates on PLAIN text (callers style afterwards), so the rune count matches
// the visible width.
func truncateRunes(s string, max int) string {
	if max < 1 {
		max = 1
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return string(r[:1])
	}
	return string(r[:max-1]) + "…"
}
