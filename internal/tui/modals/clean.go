package modals

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// CleanModal is the TUI mirror of `hive clean` — a two-phase GC of per-run
// worktrees, scratch dirs, and hive/<run> branches for old terminal runs.
// Phase 1 always runs a DRY RUN so the operator sees exactly what would be
// reclaimed BEFORE confirming the destructive sweep.
//
// Flow:
//
//	[preview] ──RPCResultMsg{Kind:"cleanup_preview"}──► [confirm]
//	  confirm:
//	    enter/y → emit SubmitRequest{Kind:"cleanup_run"} (the REAL run)
//	    esc     → close (cancel).
//	[running] ──RPCResultMsg{Kind:"cleanup_done"}──► [done]
//	  done:
//	    esc     → close.
//
// v1 has NO keep-last / no-branches toggles — the daemon's [cleanup] defaults
// (keep_last_runs + delete branches) are fine for the TUI path; those flags
// stay CLI-only (cmd_clean.go --keep-last / --no-branches). The Client.Cleanup
// wrapper already plumbs them for a future revision.
type CleanModal struct {
	// state: "preview" (awaiting dry-run result) | "confirm" (showing the
	// dry-run plan, awaiting y/enter) | "running" (real sweep in flight) |
	// "done" (final result shown, awaiting esc).
	state string

	// res holds the parsed result of the LAST cleanup.run — the dry-run plan
	// in "confirm", the real outcome in "done".
	res cleanResult

	// scroll is the item-list scroll offset (the per-run reclaim list can be
	// long); Update inc/decrements, View clamps via windowLines.
	scroll int

	errMsg string // transport-level failure of the cleanup RPC itself

	width, height int
}

// cleanResult mirrors the daemon's cleanup.run result shape (see
// cmd_clean.go). JSON numbers decode as float64 through the map[string]any
// path, so parseCleanResult does the int/int64 narrowing.
type cleanResult struct {
	Runs   int
	Bytes  int64
	DryRun bool
	Kept   int
	Items  []cleanItem
	Errors []string
}

// cleanItem is one run that would be / was reclaimed.
type cleanItem struct {
	RunID  string
	Reason string
}

// NewCleanModal constructs the modal in its "preview" state. The root is
// expected to fire Client.Cleanup(dryRun=true) and dispatch the response back
// as RPCResultMsg{Kind:"cleanup_preview"}.
func NewCleanModal() *CleanModal {
	return &CleanModal{state: "preview"}
}

func (m *CleanModal) Title() string { return "Clean — reclaim old run artifacts" }

// Init returns nil — the root kicks off the dry-run preview (see app.go case
// tabs.TabCleanRequest).
func (m *CleanModal) Init() tea.Cmd { return nil }

func (m *CleanModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case RPCResultMsg:
		switch msg.Kind {
		case "cleanup_preview":
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				m.state = "confirm" // surface the error in a stable state
				return m, nil
			}
			m.errMsg = ""
			m.res = parseCleanResult(msg.Data)
			m.scroll = 0
			m.state = "confirm"
			return m, nil
		case "cleanup_done":
			if msg.Err != nil {
				m.errMsg = msg.Err.Error()
				m.state = "done"
				return m, nil
			}
			m.errMsg = ""
			m.res = parseCleanResult(msg.Data)
			m.scroll = 0
			m.state = "done"
			return m, nil
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "up", "k":
			if m.scroll > 0 {
				m.scroll--
			}
			return m, nil
		case "down", "j":
			if m.scroll < len(m.itemLines()) {
				m.scroll++
			}
			return m, nil
		case "enter", "y":
			// Confirm the real reclaim. Only valid in "confirm" — and only
			// when there's actually something to reclaim (an empty plan just
			// closes on esc; confirming a no-op is harmless but pointless).
			if m.state != "confirm" {
				return m, nil
			}
			m.state = "running"
			m.errMsg = ""
			return m, func() tea.Msg {
				return SubmitRequest{Kind: "cleanup_run", Params: map[string]any{}}
			}
		}
	}
	return m, nil
}

func (m *CleanModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n\n")

	switch m.state {
	case "preview":
		b.WriteString(style.NeedsAttention.Render("⏳ scanning run artifacts…") + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	case "running":
		b.WriteString(style.NeedsAttention.Render(fmt.Sprintf("⏳ reclaiming %d run(s)…", m.res.Runs)) + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	if m.errMsg != "" {
		verb := "preview"
		if m.state == "done" {
			verb = "reclaim"
		}
		b.WriteString(style.InlineError.Render("✗ "+verb+" failed: "+m.errMsg) + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	// Summary line: what would-be (confirm) / was (done) reclaimed.
	if m.state == "done" {
		b.WriteString(style.Done.Render(fmt.Sprintf(
			"✓ reclaimed %d run(s), %s freed (kept %d)",
			m.res.Runs, humanizeBytes(m.res.Bytes), m.res.Kept)) + "\n\n")
	} else { // confirm
		b.WriteString(fmt.Sprintf(
			"Would reclaim %s run(s), %s (keeping %s most-recent)\n\n",
			style.Key.Render(fmt.Sprintf("%d", m.res.Runs)),
			style.Key.Render(humanizeBytes(m.res.Bytes)),
			style.Key.Render(fmt.Sprintf("%d", m.res.Kept))))
	}

	// Per-run item list, windowed so a long reclaim set never overflows.
	if len(m.res.Items) > 0 {
		// Budget: total height minus title (2), summary (2), item-list
		// errors block (0-2), and footer (1) + modal chrome (4). Clamp min.
		bodyH := height - 9
		if bodyH < 3 {
			bodyH = 3
		}
		win := windowLines(m.itemLines(), bodyH, m.scroll)
		b.WriteString(strings.Join(win, "\n") + "\n\n")
	} else if m.state == "confirm" {
		b.WriteString(style.Hint.Render("nothing to reclaim — all runs are within the keep window") + "\n\n")
	}

	// Any per-item errors the daemon reported (failed removals) render inline.
	if len(m.res.Errors) > 0 {
		for _, e := range m.res.Errors {
			b.WriteString(style.InlineError.Render("! "+e) + "\n")
		}
		b.WriteString("\n")
	}

	switch m.state {
	case "confirm":
		b.WriteString(style.Hint.Render(
			style.Key.Render("↑↓") + " scroll · " +
				style.Danger.Render("enter/y") + " reclaim · " +
				style.Key.Render("esc") + " cancel"))
	case "done":
		b.WriteString(style.Hint.Render(
			style.Key.Render("↑↓") + " scroll · " +
				style.Key.Render("esc") + " close"))
	}
	return m.frame(b.String(), width)
}

// itemLines renders the per-run reclaim list: "<run-id>  <reason>" per row.
func (m *CleanModal) itemLines() []string {
	lines := make([]string, 0, len(m.res.Items))
	for _, it := range m.res.Items {
		lines = append(lines, "  "+style.DimText.Render(it.RunID)+"  "+it.Reason)
	}
	return lines
}

// parseCleanResult narrows the daemon's cleanup.run result (decoded as
// map[string]any, so all numbers are float64) into the typed cleanResult.
func parseCleanResult(data map[string]any) cleanResult {
	var r cleanResult
	if v, ok := data["runs"].(float64); ok {
		r.Runs = int(v)
	}
	if v, ok := data["bytes"].(float64); ok {
		r.Bytes = int64(v)
	}
	if v, ok := data["dry_run"].(bool); ok {
		r.DryRun = v
	}
	if v, ok := data["kept"].(float64); ok {
		r.Kept = int(v)
	}
	if items, ok := data["items"].([]any); ok {
		for _, raw := range items {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			ci := cleanItem{}
			ci.RunID, _ = it["run_id"].(string)
			ci.Reason, _ = it["reason"].(string)
			r.Items = append(r.Items, ci)
		}
	}
	if errs, ok := data["errors"].([]any); ok {
		for _, raw := range errs {
			if s, ok := raw.(string); ok && s != "" {
				r.Errors = append(r.Errors, s)
			}
		}
	}
	return r
}

// humanizeBytes renders a byte count as a human-readable size (B/KiB/MiB/...).
// Mirrors cmd_clean.go's humanBytes so the TUI and CLI agree on formatting.
func humanizeBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}

// frame wraps the modal body in the standard rounded-border + capped width.
func (m *CleanModal) frame(content string, width int) string {
	return style.Modal.Width(frameWidth(width)).Render(content)
}
