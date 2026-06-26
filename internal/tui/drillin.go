package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/tui/style"
	"github.com/rohilrs/Hive/internal/tui/tabs"
)

// renderDrillIn returns the drill-in view for runID using the
// snapshot's stage + events data. Two-pane: left timeline, right
// events tail. eventScroll is rows back from the newest event (0 =
// newest); ↑↓ adjust it at the root level. Esc returns.
func renderDrillIn(s *Snapshot, runID string, width, height, eventScroll int) string {
	run := s.Runs[runID]
	stages := stagesForRun(s, runID)
	// Tolerate runs that aren't in the snapshot (dispatched before TUI
	// subscribed); we may still have stage rows from run.stages fetch.
	if run == nil && len(stages) == 0 {
		return style.DimText.Render("Run not found: "+runID) + "\n" +
			style.DimText.Render("(stages still loading or no data) — esc to return")
	}

	var leftBuf, rightBuf strings.Builder

	// Phase 3.7.1c: task context at top so users know what they're
	// drilled into.
	leftBuf.WriteString(style.Header.Render("Run "+runID) + "\n")
	if run != nil && run.TaskID != "" {
		if task, ok := s.Tasks[run.TaskID]; ok && task.Title != "" {
			leftBuf.WriteString("Task: " + task.Title + "\n")
		}
		leftBuf.WriteString(style.DimText.Render("task_id: "+run.TaskID) + "\n")
	}
	if run != nil && run.Pipeline != "" {
		leftBuf.WriteString(style.DimText.Render("pipeline: "+run.Pipeline) + "\n")
	}
	leftBuf.WriteString("\n")

	leftBuf.WriteString(style.Header.Render("Stage timeline") + "\n\n")
	if len(stages) == 0 {
		leftBuf.WriteString(style.DimText.Render("(no stages recorded yet — pipeline may be initializing)") + "\n")
	}
	for _, st := range stages {
		line := fmt.Sprintf("  %s/%d  %s", st.Name, st.Iter, verdictGlyph(st.Verdict, st.EndedAt == 0))
		if st.EndedAt == 0 {
			line += " " + style.Running.Render("(running)")
		}
		leftBuf.WriteString(line + "\n")
	}
	leftBuf.WriteString("\n")
	if run != nil {
		leftBuf.WriteString(style.DimText.Render("Status: "+run.Status) + "\n")
		if run.Summary != "" {
			leftBuf.WriteString(style.DimText.Render(run.Summary) + "\n")
		}
		// Phase 4.4 follow-up: the documenter is non-blocking, so a "done"
		// run may have skipped its docs. Surface it so the operator can
		// re-run with `hive document <run-id>`.
		if run.DocumentationSkipped {
			chip := style.NeedsAttention.Render(" docs:skipped ")
			leftBuf.WriteString(chip + " " + style.DimText.Render("hive document "+run.ID) + "\n")
			if run.DocumentationSkipReason != "" {
				leftBuf.WriteString(style.DimText.Render("  "+run.DocumentationSkipReason) + "\n")
			}
		}
	}

	// Phase 4.6: surface this run's pending approvals so a blocked worker
	// is visible from the drill-in (go to the Approvals tab to resolve).
	var pending []tabs.PendingApproval
	for _, pa := range s.PendingApprovals() {
		if pa.RunID == runID {
			pending = append(pending, pa)
		}
	}
	if len(pending) > 0 {
		leftBuf.WriteString("\n" + style.Running.Render(fmt.Sprintf("⚠ %d pending approval(s) — a/A approve · d/D deny", len(pending))) + "\n")
		for i, pa := range pending {
			// a/A/d/D act on the first (next) one — highlight it with the cursor marker
			marker := style.CursorMarker(i == 0)
			leftBuf.WriteString(style.DimText.Render(fmt.Sprintf("%s%s  %s", marker, pa.ToolName, compactArg(pa.Arg))) + "\n")
		}
	}

	// Width: each bordered panel renders to (contentWidth + 2) cells
	// (RoundedBorder adds 1 cell each side; padding is absorbed into
	// the declared Width — verified empirically). Two panels side by
	// side therefore need leftW + rightW = width - 4 to fit exactly.
	usable := width - 4
	if usable < 60 {
		usable = 60
	}
	halfWidth := usable / 2
	if halfWidth < 30 {
		halfWidth = 30
	}
	rightWidth := usable - halfWidth
	// Height: reserve 6 lines for the surrounding chrome (tab bar +
	// footer). panelHeight is the hard cap — see MaxHeight below.
	panelHeight := height - 6
	if panelHeight < 10 {
		panelHeight = 10
	}

	// Events: a scrollable window over this run's events, one row each
	// (truncated, no wrap, so the row math is exact). scrollWindow clamps
	// the offset so there's no scrolling when events fit (fixes the
	// last-row-disappears bug) and the box never overflows.
	runEvents := filterEventsForRun(s.recentEvents, runID)
	visible := drillEventRows(height)
	win, clamped := scrollWindow(runEvents, eventScroll, visible)
	scrollNote := ""
	if len(runEvents) > visible {
		endIdx := len(runEvents) - clamped
		startIdx := endIdx - len(win)
		scrollNote = fmt.Sprintf("  %d-%d/%d ↑↓", startIdx+1, endIdx, len(runEvents))
	}
	rightBuf.WriteString(style.Header.Render("Events") + style.DimText.Render(scrollNote) + "\n\n")
	if len(runEvents) == 0 {
		rightBuf.WriteString(style.DimText.Render("(no events captured since TUI subscribed)") + "\n")
	}
	lineWidth := rightWidth - 4 // panel padding/border budget
	if lineWidth < 10 {
		lineWidth = 10
	}
	for _, ev := range win {
		rightBuf.WriteString(truncateLine(fmt.Sprintf("  %-16s %s", ev.Type, tabs.FormatEventDetails(ev)), lineWidth) + "\n")
	}

	// Both buffers are capped to panelHeight CONTENT lines so the bordered
	// box (panelHeight+2 rows) never overflows the screen. We do NOT use
	// MaxHeight: it clips the rendered box *including* the border, which
	// ate the bottom border + the events box's right edge. Height alone
	// pads short content; the caps prevent tall content.
	leftPanel := style.Panel.Width(halfWidth).Height(panelHeight).Render(clipLinesTop(leftBuf.String(), panelHeight))
	rightPanel := style.PanelFocus.Width(rightWidth).Height(panelHeight).Render(clipLinesTop(rightBuf.String(), panelHeight))
	footer := "esc return · ↑↓ scroll events"
	if len(pending) > 0 {
		footer += " · a/A approve · d/D deny"
	}
	if run != nil && (run.Status == "running" || run.Status == "pending") {
		footer += " · x abandon"
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel) + "\n" +
		style.DimText.Render(footer)
}

// clipLinesTop keeps at most n lines from the top of s (so headers stay
// visible). Prevents a tall buffer from overflowing its bordered panel.
func clipLinesTop(s string, n int) string {
	if n < 1 {
		n = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// scrollWindow returns the slice of items to show for a scroll offset
// (rows back from the END; 0 = newest at bottom) and the clamped offset.
// offset is clamped to [0, max(0, len-visible)] so there's no scrolling
// when content fits (and the window never overruns the slice).
func scrollWindow[T any](items []T, offset, visible int) (window []T, clamped int) {
	if visible < 1 {
		visible = 1
	}
	maxOff := len(items) - visible
	if maxOff < 0 {
		maxOff = 0
	}
	if offset > maxOff {
		offset = maxOff
	}
	if offset < 0 {
		offset = 0
	}
	end := len(items) - offset
	start := end - visible
	if start < 0 {
		start = 0
	}
	return items[start:end], offset
}

// drillEventRows is the number of event rows the drill-in events panel
// can show. Shared by renderDrillIn + the root key handler so the scroll
// clamp can't drift from the layout.
func drillEventRows(height int) int {
	panelHeight := height - 6
	if panelHeight < 10 {
		panelHeight = 10
	}
	v := panelHeight - 3 // header + scroll-status + blank
	if v < 1 {
		v = 1
	}
	return v
}

// truncateLine clips a single (unstyled) line to n runes so it occupies
// exactly one terminal row — prevents wide event lines from wrapping and
// overflowing the events panel.
func truncateLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// verdictGlyph maps a stage's verdict + running flag to a colored
// glyph that scans faster than the verdict text during debugging.
// Ordered: explicit verdict wins (APPROVE/CHANGES_REQUESTED + the
// future-proofed lowercase/rejected variants); else if the stage is
// still running, show the cyan ellipsis; else a dim em-dash for the
// empty/finished-with-no-verdict case. Unrecognized verdicts render
// the text via DimText so future verdict kinds don't disappear
// silently.
//
// StageView has no Status field (running is derived from EndedAt==0
// upstream); the bool keeps the helper provider-agnostic + testable.
func verdictGlyph(verdict string, running bool) string {
	switch verdict {
	case "APPROVE", "approved":
		return style.Done.Render("✓")
	case "CHANGES_REQUESTED", "changes_requested", "retry":
		return style.NeedsAttention.Render("↺")
	case "rejected":
		return style.ErrorStyle.Render("✗")
	}
	if verdict == "" {
		if running {
			return style.Running.Render("…")
		}
		return style.DimText.Render("—")
	}
	return style.DimText.Render(verdict)
}

// compactArg trims + single-lines a tool arg for the drill-in summary.
func compactArg(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 50 {
		s = s[:49] + "…"
	}
	return s
}

func stagesForRun(s *Snapshot, runID string) []*StageView {
	var out []*StageView
	for _, st := range s.Stages {
		if st.RunID == runID {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}

func filterEventsForRun(events []tabs.TimedEvent, runID string) []tabs.TimedEvent {
	var out []tabs.TimedEvent
	for _, ev := range events {
		if id, _ := ev.Data["run_id"].(string); id == runID {
			out = append(out, ev)
		}
	}
	if len(out) > 50 {
		out = out[len(out)-50:]
	}
	return out
}
