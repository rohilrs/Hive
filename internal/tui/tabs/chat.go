package tabs

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/chat"
	"github.com/rohilrs/Hive/internal/tui/style"
)

// chatFrameView is one rendered message in the chat tab's history. It
// captures both the wire frame data and confirm-bookkeeping (Resolved,
// Approved) so the same slice indexes survive across frame updates.
type chatFrameView struct {
	Kind       string // "text" | "tool_result" | "tool_proposed" | "session" | "turn_done" | "error" | "user"
	Text       string
	Tool       string
	Result     string
	ToolCallID string  // only set for tool_proposed; lets us find the card to resolve
	CostUSD    float64 // for turn_done
	Resolved   bool    // for tool_proposed, true once chat.confirm sent
	Approved   bool    // for tool_proposed, set when resolved
	Cancelled  bool    // true when resolved via `c` keybind; renders as dim "✗ cancelled"
}

// ChatTab is the Phase 6.2 Bubbletea tab. Holds per-tab state including the
// running message history and any in-flight stream + pending confirm.
type ChatTab struct {
	input           textinput.Model
	frames          []chatFrameView
	sessionID       string         // captured from the "session" frame on first turn
	sessionName     string         // from session_info frame; manually settable via SetSessionName
	provider        string         // from session_info frame ("api" | "claude-code")
	model           string         // from turn_done frame's Model field
	turnCount       int            // incremented on each turn_done
	lastCost        float64        // cumulative cost as of the last turn_done (daemon sends a running total each turn)
	streaming       bool           // chat.send stream is active
	pendingConfirms map[string]int // tool_call_id → index into frames (the tool_proposed card to resolve)
	// autoApproveTools is the per-session approve-all set. Tool names in
	// here auto-resolve subsequent tool_proposed frames with Approve=true
	// without prompting. Populated by the `a` keybind. Cleared on Reset
	// (Ctrl-N / new session) — never persisted across sessions, never
	// written to the approval_rules table.
	autoApproveTools map[string]bool
	lastErr         error
	scrollOffset    int // history scroll, 0 = bottom
	width, height   int
	// Glamour markdown renderer: built on first use and rebuilt when width changes.
	mdRenderer      *glamour.TermRenderer
	mdRendererWidth int
	// streamSpinner animates while a chat.send stream is in flight. The
	// KeyHelp footer renders streamSpinner.View() so the operator can
	// tell at a glance whether the daemon is still working vs hung.
	streamSpinner spinner.Model
}

// NewChat builds the chat tab in its idle starting state.
func NewChat() *ChatTab {
	ti := textinput.New()
	ti.Placeholder = "Type a message and press Enter (Ctrl-K for session picker)…"
	ti.CharLimit = 4096
	ti.Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return &ChatTab{
		input:            ti,
		pendingConfirms:  map[string]int{},
		autoApproveTools: map[string]bool{},
		streamSpinner:    sp,
	}
}

func (t *ChatTab) Name() string  { return "Chat" }
func (t *ChatTab) Init() tea.Cmd { return textinput.Blink }

func (t *ChatTab) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	// Spinner tick: advance the dots only while streaming. When the
	// stream ends, drop the tick so the chain stops naturally.
	if tm, ok := msg.(spinner.TickMsg); ok {
		if t.streaming {
			upd, cmd := t.streamSpinner.Update(tm)
			t.streamSpinner = upd
			return t, cmd
		}
		return t, nil
	}
	switch m := msg.(type) {
	case ChatFrameMsg:
		f := m.Frame
		switch f.Kind {
		case "session":
			if f.Text != "" {
				t.sessionID = f.Text
			}
			// Don't append to frames — session is metadata.
			return t, nil
		case "session_info":
			var info struct {
				Name     string `json:"name"`
				Provider string `json:"provider"`
			}
			if err := json.Unmarshal([]byte(f.Result), &info); err == nil {
				if info.Name != "" {
					t.sessionName = info.Name
				}
				if info.Provider != "" {
					t.provider = info.Provider
				}
			}
			// Don't append to frames — session_info is metadata, not history.
			return t, nil
		case "turn_done":
			t.model = f.Model
			t.turnCount++
			t.lastCost = f.CostUSD
			// Append a turn_done card so the cost footer is visible inline.
			t.frames = append(t.frames, chatFrameView{
				Kind: "turn_done", Text: f.Model, CostUSD: f.CostUSD,
			})
		case "error":
			t.lastErr = errors.New(f.Text)
			t.frames = append(t.frames, chatFrameView{Kind: "error", Text: f.Text})
		default:
			// "text", "tool_result", "tool_proposed" — pass through.
			view := chatFrameView{Kind: f.Kind, Text: f.Text, Tool: f.Tool, Result: f.Result}
			if f.Kind == "tool_proposed" {
				var body struct {
					ToolCallID string `json:"tool_call_id"`
				}
				_ = json.Unmarshal([]byte(f.Result), &body)
				view.ToolCallID = body.ToolCallID
			}
			// Approve-all bypass (the `a` keybind populates
			// autoApproveTools per-session). If this tool was approved
			// session-wide, mark the card resolved immediately so the
			// render path shows ✓ without a y/n prompt, AND emit the
			// chat.confirm request without going through pendingConfirms.
			autoApprove := false
			if f.Kind == "tool_proposed" && view.ToolCallID != "" && t.autoApproveTools[view.Tool] {
				view.Resolved = true
				view.Approved = true
				autoApprove = true
			}
			t.frames = append(t.frames, view)
			if view.ToolCallID != "" && !autoApprove {
				// If a prior tool_proposed had the same tool_call_id (retry path or
				// daemon bug), the older card is no longer resolvable — mark it
				// resolved-as-denied so its UI doesn't dangle in "? [y/n]" forever.
				if existing, dup := t.pendingConfirms[view.ToolCallID]; dup {
					t.frames[existing].Resolved = true
					t.frames[existing].Approved = false
				}
				t.pendingConfirms[view.ToolCallID] = len(t.frames) - 1
			}
			if autoApprove {
				sessionID := t.sessionID
				toolCallID := view.ToolCallID
				return t, func() tea.Msg {
					return TabChatConfirmRequest{
						SessionID:  sessionID,
						ToolCallID: toolCallID,
						Approve:    true,
					}
				}
			}
		}
		return t, nil
	case ChatHistoryLoadedMsg:
		// Rebuild frames from the loaded session's messages. The order is
		// chronological (ASC by created_at from the daemon).
		t.sessionID = m.SessionID
		t.sessionName = m.Name
		t.provider = m.Provider
		t.lastCost = m.TotalCostUSD
		// model isn't in the persisted session row — empty until next turn_done.
		t.model = ""
		// Derive turnCount from the loaded history: each user-role message
		// corresponds to one completed turn (turn_done fires once per user
		// input). Without this, the metadata bar's cost + turns chips stay
		// hidden on resume since they're gated on turnCount > 0.
		t.turnCount = 0
		for _, msg := range m.Messages {
			if msg.Role == "user" {
				t.turnCount++
			}
		}
		t.frames = nil
		t.pendingConfirms = map[string]int{}
		t.autoApproveTools = map[string]bool{}
		t.scrollOffset = 0
		for _, msg := range m.Messages {
			view := chatFrameView{}
			switch msg.Role {
			case "user":
				view.Kind = "user"
				view.Text = msg.Content
			case "assistant":
				view.Kind = "text"
				view.Text = msg.Content
			case "tool":
				view.Kind = "tool_result"
				view.Tool = msg.ToolName
				view.Result = msg.Content
			case "error":
				view.Kind = "error"
				view.Text = msg.Content
			}
			if view.Kind != "" {
				t.frames = append(t.frames, view)
			}
		}
		return t, nil

	case ChatStreamStartedMsg:
		// Already in streaming state from the Enter handler; kick the
		// spinner tick so KeyHelp's "streaming…" indicator animates.
		return t, t.streamSpinner.Tick
	case ChatStreamEndedMsg:
		t.streaming = false
		if m.Err != nil {
			t.lastErr = m.Err
		}
		return t, nil
	case tea.WindowSizeMsg:
		t.width = m.Width
		t.height = m.Height
		return t, nil
	case tea.MouseMsg:
		// Wheel scroll history regardless of input state — wheel doesn't
		// conflict with typing, and matches every other chat UI's
		// expectation. Arrow-key scroll is still gated on empty input
		// because arrows compete with the textinput cursor.
		switch m.Type {
		case tea.MouseWheelUp:
			t.scrollOffset++
			return t, nil
		case tea.MouseWheelDown:
			if t.scrollOffset > 0 {
				t.scrollOffset--
			}
			return t, nil
		}
		return t, nil
	}
	// Scroll keybinds: up/down/pgup/pgdown only when input is empty, so
	// textinput still gets arrow keys for cursor movement when typing.
	if key, ok := msg.(tea.KeyMsg); ok && t.input.Value() == "" {
		switch key.Type {
		case tea.KeyUp:
			t.scrollOffset++
			return t, nil
		case tea.KeyDown:
			if t.scrollOffset > 0 {
				t.scrollOffset--
			}
			return t, nil
		case tea.KeyPgUp:
			t.scrollOffset += 5
			return t, nil
		case tea.KeyPgDown:
			t.scrollOffset -= 5
			if t.scrollOffset < 0 {
				t.scrollOffset = 0
			}
			return t, nil
		}
	}
	// Ctrl-N: start a fresh chat session. Gated on !streaming so a
	// mid-turn keystroke can't strand the in-flight stream against a
	// cleared sessionID. The new session's actual ID is assigned daemon-
	// side when the user sends their first message (chat.send →
	// InsertChatSession).
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "ctrl+n" && !t.streaming {
		t.Reset()
		return t, nil
	}
	// r keybind: open rename modal for the active session.
	// Gated on: empty input + non-empty sessionID so r typed mid-message is safe.
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyRunes && len(key.Runes) == 1 && key.Runes[0] == 'r' && t.input.Value() == "" && t.sessionID != "" {
		sessionID := t.sessionID
		currentName := t.sessionName
		return t, func() tea.Msg {
			return OpenChatRenameMsg{SessionID: sessionID, CurrentName: currentName}
		}
	}
	// t keybind: open the ChatToolResultPicker modal listing this
	// session's tool_result frames most-recent-first. Gated like r
	// rename: single rune t + empty input + at least one tool_result
	// frame in the session. When no tool_result frames exist, falls
	// through to the textinput so the user can start a message with t.
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyRunes && len(key.Runes) == 1 && key.Runes[0] == 't' && t.input.Value() == "" {
		var rows []ChatToolResultRow
		for i := len(t.frames) - 1; i >= 0; i-- {
			f := t.frames[i]
			if f.Kind != "tool_result" {
				continue
			}
			rows = append(rows, ChatToolResultRow{
				Tool:    f.Tool,
				Result:  f.Result,
				IsError: isToolResultError(f.Result),
			})
		}
		if len(rows) > 0 {
			return t, func() tea.Msg {
				return OpenChatToolResultPickerMsg{Rows: rows}
			}
		}
		// No tool_result frames — fall through to textinput so `t`
		// starts a typed message instead of being silently swallowed.
	}
	// y/n/a/c keybind: approve, deny, auto-approve, or cancel the latest
	// pending tool_proposed confirm. `a` does what `y` does AND adds the
	// tool name to the per-session auto-approve set so subsequent
	// tool_proposed frames for the same tool auto-resolve without
	// prompting. `c` resolves with Approved=false, Cancelled=true and
	// passes Reason="user cancelled, do not retry" so the model does not
	// retry the same tool call. Gated on: single rune y/n/a/c, empty
	// input (unambiguous), at least one pending.
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyRunes && len(key.Runes) == 1 {
		r := key.Runes[0]
		if (r == 'y' || r == 'n' || r == 'a' || r == 'c') && t.input.Value() == "" && len(t.pendingConfirms) > 0 {
			// Resolve the LATEST pending confirm (the one with the highest frame index).
			var (
				latestID  string
				latestIdx = -1
			)
			for id, idx := range t.pendingConfirms {
				if idx > latestIdx {
					latestID = id
					latestIdx = idx
				}
			}
			approve := r == 'y' || r == 'a'
			cancelled := r == 'c'
			t.frames[latestIdx].Resolved = true
			t.frames[latestIdx].Approved = approve
			t.frames[latestIdx].Cancelled = cancelled
			if r == 'a' {
				toolName := t.frames[latestIdx].Tool
				if toolName != "" {
					t.autoApproveTools[toolName] = true
				}
			}
			delete(t.pendingConfirms, latestID)
			sessionID := t.sessionID
			reason := ""
			if r == 'c' {
				reason = "user cancelled, do not retry"
			}
			return t, func() tea.Msg {
				return TabChatConfirmRequest{
					SessionID:  sessionID,
					ToolCallID: latestID,
					Approve:    approve,
					Reason:     reason,
				}
			}
		}
	}
	// e: open the edit-args modal for the latest pending tool_proposed.
	// Gated like y/n/a/c: single rune e + empty input + at least one pending.
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyRunes && len(key.Runes) == 1 && key.Runes[0] == 'e' && t.input.Value() == "" && len(t.pendingConfirms) > 0 {
		var (
			latestID  string
			latestIdx = -1
		)
		for id, idx := range t.pendingConfirms {
			if idx > latestIdx {
				latestID = id
				latestIdx = idx
			}
		}
		tool := t.frames[latestIdx].Tool
		// The tool_proposed frame's Result is the envelope:
		// {"tool_call_id":"...","input":{...}}. Extract just `input`
		// for the modal pre-fill so the user edits the actual tool args,
		// not the wire envelope.
		var body struct {
			Input json.RawMessage `json:"input"`
		}
		args := json.RawMessage("{}")
		if err := json.Unmarshal([]byte(t.frames[latestIdx].Result), &body); err == nil && len(body.Input) > 0 {
			args = body.Input
		}
		return t, func() tea.Msg {
			return OpenChatEditArgsMsg{ToolCallID: latestID, ToolName: tool, Args: args}
		}
	}
	// Existing Enter handler + textinput fall-through.
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyEnter {
		if t.streaming {
			return t, nil
		}
		txt := strings.TrimSpace(t.input.Value())
		if txt == "" {
			return t, nil
		}
		sessionID := t.sessionID
		t.streaming = true
		t.input.SetValue("")
		// Append a synthetic "user" frame so the user sees their own
		// message immediately, before the server starts streaming.
		t.frames = append(t.frames, chatFrameView{Kind: "user", Text: txt})
		// Kick the spinner tick alongside the send request so "(streaming…)"
		// animates from the moment the user presses Enter — the daemon's
		// ChatStreamStartedMsg can take a few hundred ms to arrive.
		return t, tea.Batch(
			func() tea.Msg {
				return TabChatSendRequest{Message: txt, SessionID: sessionID}
			},
			t.streamSpinner.Tick,
		)
	}
	var cmd tea.Cmd
	t.input, cmd = t.input.Update(msg)
	return t, cmd
}

func (t *ChatTab) View() string {
	// maxLineWidth is the usable inner width for history content.
	// Panel border (2) + padding (2) = 4 overhead. Conservative: floor at 40.
	maxLineWidth := t.width - 4
	if maxLineWidth <= 0 {
		maxLineWidth = 76 // sensible default when no WindowSizeMsg yet
	}

	historyContent := t.renderHistory(maxLineWidth)
	metaBar := t.renderMetadataBar()

	footerErr := ""
	if t.lastErr != nil && !t.streaming {
		footerErr = style.ErrorStyle.Render("last error: " + t.lastErr.Error())
	}

	// Flat fallback before the first WindowSizeMsg arrives, or when terminal
	// is too small for the two-box layout (history ≥5 + input 3 = 8 minimum).
	if t.width == 0 || t.height == 0 || t.height < 8 {
		if metaBar != "" {
			if footerErr != "" {
				return metaBar + "\n" + historyContent + "\n" + footerErr + "\n" + t.input.View()
			}
			return metaBar + "\n" + historyContent + "\n" + t.input.View()
		}
		if footerErr != "" {
			return historyContent + "\n" + footerErr + "\n" + t.input.View()
		}
		return historyContent + "\n" + t.input.View()
	}

	// Reserve rows for input box (border-top + input line + border-bottom = 3)
	// plus the optional err line above it.
	inputHeight := 3
	if footerErr != "" {
		inputHeight++
	}
	// History panel height: total height minus input area minus the 2 border
	// rows of the history panel itself, minus 1 row for the metadata bar if present.
	metaHeight := 0
	if metaBar != "" {
		metaHeight = 1
	}
	historyHeight := t.height - inputHeight - 2 - metaHeight
	if historyHeight < 3 {
		historyHeight = 3
	}

	// Slice history to fit historyHeight, honoring scrollOffset. Reserve rows
	// for the scroll-affordance hints when there's more history than fits, so
	// content + hints can never exceed historyHeight (the panel's row budget)
	// and push the root tab bar off-screen on scroll. (Dogfood fix B2)
	historyLines := strings.Split(historyContent, "\n")
	contentHeight := historyHeight
	if len(historyLines) > historyHeight {
		contentHeight -= 2 // up + down hint rows (≤1 row unused when only one shows)
		if contentHeight < 1 {
			contentHeight = 1
		}
	}

	// Clamp scrollOffset locally — View() must not mutate state.
	maxScroll := len(historyLines) - contentHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	clampedOffset := t.scrollOffset
	if clampedOffset > maxScroll {
		clampedOffset = maxScroll
	}

	// Compute the visible window: clampedOffset==0 means bottom (newest).
	end := len(historyLines) - clampedOffset
	if end < 0 {
		end = 0
	}
	if end > len(historyLines) {
		end = len(historyLines)
	}
	start := end - contentHeight
	if start < 0 {
		start = 0
	}
	visible := strings.Join(historyLines[start:end], "\n")

	// Scroll affordances: surface hidden content above/below the visible
	// window so the operator knows there's more history to scroll to.
	// Rendered inside the history panel (above/below the visible slice);
	// they share the panel's row budget so very-short windows trade a
	// content line for the hint. Empty strings (count==0) are no-ops.
	if hint := style.ScrollHint("up", start); hint != "" {
		visible = hint + "\n" + visible
	}
	if hint := style.ScrollHint("down", len(historyLines)-end); hint != "" {
		visible = visible + "\n" + hint
	}

	// Hard-clamp each rendered line to maxLineWidth so lipgloss soft-wrap
	// can't push content off the top of the screen (hiding the tab bar).
	// lipgloss.Width correctly counts visible columns (ignoring ANSI escape
	// sequences), so we gate on it: only call truncateLineByRune when the
	// line visibly overflows. Without this gate, glamour-styled lines whose
	// VISIBLE width is well within budget were getting spurious trailing
	// "…" because rune-count of the raw string included escape bytes.
	clampedLines := strings.Split(visible, "\n")
	for i, line := range clampedLines {
		if lipgloss.Width(line) > maxLineWidth {
			clampedLines[i] = truncateLineByRune(line, maxLineWidth+12)
		}
	}
	visible = strings.Join(clampedLines, "\n")

	// Panel width: subtract 2 so the border fits within the terminal width.
	panelWidth := t.width - 2

	historyPanel := style.Panel.Width(panelWidth).Height(historyHeight).Render(visible)
	inputPanel := style.PanelFocus.Width(panelWidth).Height(1).Render(t.input.View())

	if metaBar != "" {
		if footerErr != "" {
			return lipgloss.JoinVertical(lipgloss.Top, metaBar, historyPanel, footerErr, inputPanel)
		}
		return lipgloss.JoinVertical(lipgloss.Top, metaBar, historyPanel, inputPanel)
	}
	if footerErr != "" {
		return lipgloss.JoinVertical(lipgloss.Top, historyPanel, footerErr, inputPanel)
	}
	return lipgloss.JoinVertical(lipgloss.Top, historyPanel, inputPanel)
}

// flattenMarkdownTables replaces GFM markdown tables with stacked-block
// markdown so glamour doesn't render box-drawing chars that collide with the
// chat history panel border. Non-table markdown (headers, bold, lists, code)
// is left untouched.
//
// A markdown table is detected as a run of lines where:
//   - Every line starts with optional whitespace then '|'
//   - The second line is the separator (cells contain only -, :, |, space)
//
// The stacked-block form per chat output rendering spec §5:
//
//	**<name>**  ·  <handle>
//	  <detail-line>
//
// Row blocks are separated by blank lines. flattenTableBlock (in
// chat_format.go) performs column-role detection and elision.
func flattenMarkdownTables(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	i := 0
	for i < len(lines) {
		if !isTableLine(lines[i]) {
			out = append(out, lines[i])
			i++
			continue
		}
		// Collect contiguous table lines.
		start := i
		for i < len(lines) && isTableLine(lines[i]) {
			i++
		}
		block := lines[start:i]
		// Need at least header + separator + 1 row for a real table; otherwise
		// treat as plain | text and leave alone.
		if len(block) < 3 || !isSeparatorRow(block[1]) {
			out = append(out, block...)
			continue
		}
		flat := flattenTableBlock(block)
		out = append(out, flat...)
	}
	return strings.Join(out, "\n")
}

func isTableLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|")
}

func isSeparatorRow(line string) bool {
	t := strings.TrimSpace(line)
	// e.g. "|------|:----:|---|"
	for _, r := range t {
		switch r {
		case '|', '-', ':', ' ':
			// ok
		default:
			return false
		}
	}
	return strings.Contains(t, "-")
}

// markdownRenderer returns a glamour renderer sized to width, rebuilding
// only when the width changes. Falls back to nil on error (callers use raw text).
//
// Style selection: WithAutoStyle() falls back to "notty" (no ANSI) when
// stdout is not a TTY, which is always the case inside Bubbletea's alternate
// screen. We therefore use the "dark" style explicitly — suitable for the
// dark-background terminals where Bubbletea TUIs typically run. The
// GLAMOUR_STYLE env var overrides this when set (via WithEnvironmentConfig
// fallback logic).
func (t *ChatTab) markdownRenderer(width int) *glamour.TermRenderer {
	if width <= 0 {
		width = 78
	}
	if t.mdRenderer != nil && t.mdRendererWidth == width {
		return t.mdRenderer
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil
	}
	t.mdRenderer = r
	t.mdRendererWidth = width
	return t.mdRenderer
}

// renderMarkdown renders content using a chunked path: stacked blocks
// (bold headline + optional indented detail) are styled via lipgloss
// directly so the detail line sits IMMEDIATELY under the headline with
// no padding; everything else (prose, code, headings) still goes through
// glamour. The old all-glamour path required adding hard paragraph
// breaks between headline + detail just to defeat glamour's soft-break
// collapsing, which wasted vertical space — that workaround is gone.
func (t *ChatTab) renderMarkdown(content string, width int) string {
	content = flattenMarkdownTables(content)
	content = stripHandleBackticks(content)
	return t.renderMarkdownChunked(content, width)
}

// renderStackedBlockItem renders one parsed item as compact 1-or-2 line
// styled text. The bold name uses ChatHive (the chat-hive accent style);
// tail pieces are appended on the same line joined by "  ·  "; the detail
// (if any) goes on the very next line, indented two spaces with the dim
// style, with NO blank line between headline and detail.
func (t *ChatTab) renderStackedBlockItem(item stackedBlockItem) string {
	var b strings.Builder
	b.WriteString(style.ChatHive.Render(item.Name))
	for _, piece := range item.Tail {
		b.WriteString("  ·  ")
		b.WriteString(piece)
	}
	if item.Detail != "" {
		b.WriteString("\n  ")
		b.WriteString(style.DimText.Render(item.Detail))
	}
	return b.String()
}

// renderMarkdownChunked splits content into stacked-block runs vs. other
// content. Stacked blocks render via renderStackedBlockItem (lipgloss
// direct, no glamour padding). Other content renders via glamour as
// before. Items in the same stacked-block run are separated by ONE blank
// line; transitions between glamour chunks and stacked-block chunks are
// joined by a single newline (glamour already brings its own spacing).
func (t *ChatTab) renderMarkdownChunked(content string, width int) string {
	lines := strings.Split(content, "\n")
	var out strings.Builder
	var glamourBuf []string

	flushGlamour := func() {
		if len(glamourBuf) == 0 {
			return
		}
		chunk := strings.Join(glamourBuf, "\n")
		chunk = strings.TrimRight(chunk, "\n \t")
		glamourBuf = nil
		if chunk == "" {
			return
		}
		rendered := t.renderViaGlamour(chunk, width)
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(rendered)
	}

	i := 0
	for i < len(lines) {
		line := lines[i]
		if looksLikeStackedHeadline(line) {
			flushGlamour()
			item, consumed, ok := parseStackedBlock(lines, i)
			if !ok {
				glamourBuf = append(glamourBuf, line)
				i++
				continue
			}
			item = canonicalizeStackedBlock(item)
			if out.Len() > 0 {
				// Blank line between consecutive stacked items AND between
				// a glamour chunk and a stacked item (visual breathing room).
				out.WriteString("\n\n")
			}
			out.WriteString(t.renderStackedBlockItem(item))
			i += consumed
			// Swallow a single blank-line separator if it follows; the run
			// stays contiguous so the next block-or-prose joins cleanly.
			if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			continue
		}
		glamourBuf = append(glamourBuf, line)
		i++
	}
	flushGlamour()
	return out.String()
}

// renderViaGlamour wraps the glamour Render call with the trim-trailing-
// whitespace cleanup that downstream code depends on (no doubled gaps
// between consecutive frames). Falls back to the raw chunk on any error.
func (t *ChatTab) renderViaGlamour(content string, width int) string {
	r := t.markdownRenderer(width)
	if r == nil {
		return content
	}
	out, err := r.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimRight(out, "\n \t")
}

// renderHistory renders the full message history into a plain string using
// the speaker-header grouping logic. Extracted from View so the line-count
// can be computed before slicing the visible window.
// maxLineWidth is the usable inner width in rune columns (used to cap
// tool_result / tool_proposed bodies so they don't hard-wrap and inflate
// the line count beyond the budget).
func (t *ChatTab) renderHistory(maxLineWidth int) string {
	// tool_proposed body prefix adds ~14 runes ("  [proposed] name "); use a
	// slightly shorter cap so the rendered line still fits.
	proposedBodyWidth := maxLineWidth - 14
	if proposedBodyWidth < 10 {
		proposedBodyWidth = 10
	}

	var b strings.Builder
	currentSpeaker := "" // "", "you", "hive", "error"

	emitHeader := func(speaker string) {
		if currentSpeaker != "" {
			b.WriteString("\n")
		}
		var label string
		switch speaker {
		case "you":
			label = style.ChatYou.Render("─── you ───")
		case "hive":
			label = style.ChatHive.Render("─── hive ───")
		case "error":
			label = style.ErrorStyle.Render("─── error ───")
		}
		b.WriteString(label + "\n")
		currentSpeaker = speaker
	}

	for _, f := range t.frames {
		switch f.Kind {
		case "user":
			if currentSpeaker != "you" {
				emitHeader("you")
			}
			b.WriteString(t.renderMarkdown(f.Text, maxLineWidth) + "\n")
		case "text":
			if currentSpeaker != "hive" {
				emitHeader("hive")
			}
			b.WriteString(t.renderMarkdown(f.Text, maxLineWidth) + "\n")
		case "tool_result":
			if currentSpeaker != "hive" {
				emitHeader("hive")
			}
			label := style.ChatToolL.Render("  · " + f.Tool + " ")
			switch classifyToolResult(f.Result) {
			case "transient":
				// MCP startup-race miss: model retries on next message and usually
				// succeeds. Soft-render so it doesn't look like a real failure.
				b.WriteString(label + style.DimText.Render("(transient)") + "\n")
			case "unavailable":
				// Stub tools (hive_resume, hive_predict — see deferred.md): tool
				// is advertised but the daemon backend isn't built yet.
				b.WriteString(label + style.DimText.Render("(unavailable)") + "\n")
			default:
				b.WriteString(label + style.Done.Render("✓") + "\n")
			}
		case "tool_proposed":
			if currentSpeaker != "hive" {
				emitHeader("hive")
			}
			label := "  [proposed] " + f.Tool + " " + truncateMiddle(f.Result, proposedBodyWidth)
			switch {
			case f.Resolved && f.Approved:
				b.WriteString(style.Done.Render("✓ "+label) + "\n")
			case f.Resolved && f.Cancelled:
				b.WriteString(style.DimText.Render("✗ cancelled "+label) + "\n")
			case f.Resolved && !f.Approved:
				b.WriteString(style.ErrorStyle.Render("✗ "+label) + "\n")
			default:
				b.WriteString(style.NeedsAttention.Render("? "+label+"  [y/n/a/e/c]") + "\n")
			}
		case "turn_done":
			if currentSpeaker != "hive" {
				emitHeader("hive")
			}
			b.WriteString(style.DimText.Render(fmt.Sprintf("(%s · $%.4f)", f.Text, f.CostUSD)) + "\n")
		case "error":
			if currentSpeaker != "error" {
				emitHeader("error")
			}
			b.WriteString(style.ErrorStyle.Render(f.Text) + "\n")
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// classifyToolResult returns "transient", "unavailable", or "ok"
// based on substring markers in the tool_result body. This is the
// single source of truth used by both the inline render (3-way
// glyph/style distinction) and isToolResultError (2-way boolean).
func classifyToolResult(result string) string {
	if strings.Contains(result, "tool_use_error") {
		return "transient"
	}
	if strings.Contains(result, "not available in this version") {
		return "unavailable"
	}
	return "ok"
}

// isToolResultError classifies a tool_result Result body as an error
// for both the inline render (compact status glyph) and the new
// tool-result picker collection. Thin wrapper over classifyToolResult
// so the picker and the inline render stay in sync.
func isToolResultError(result string) bool {
	return classifyToolResult(result) != "ok"
}

// truncateMiddle keeps both ends visible when a tool_result body is long.
// 6.2 leaves expansion-on-focus deferred — full bodies are shown via
// `hive chat history <session>` (6.1b-ii) for now.
//
// Operates on runes, not bytes, so multi-byte UTF-8 codepoints at the cut
// point produce well-formed strings (lipgloss width calculations require
// valid UTF-8).
func truncateMiddle(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	half := (max - 3) / 2
	return string(rs[:half]) + "..." + string(rs[len(rs)-half:])
}

// truncateLineByRune hard-truncates a single rendered line to maxRunes,
// appending "…" when cut. This prevents lipgloss soft-wrapping from inflating
// the actual row count beyond the budget, which would push the tab bar off
// screen. ANSI escape bytes count toward rune length but not visible columns,
// so callers should add a small tolerance (≈12 for typical lipgloss styles).
func truncateLineByRune(s string, maxRunes int) string {
	if maxRunes < 4 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

// renderMetadataBar produces the single-line header above the history
// box: "[name] · provider · model · $cost · N turns". Returns "" when
// there's nothing to show (no session yet).
func (t *ChatTab) renderMetadataBar() string {
	if t.sessionID == "" && t.sessionName == "" {
		return ""
	}
	parts := []string{}
	if t.sessionName != "" {
		parts = append(parts, style.ChatHive.Render("["+t.sessionName+"]"))
	} else if t.sessionID != "" {
		// Show a truncated session id if no name yet.
		short := t.sessionID
		if rs := []rune(short); len(rs) > 12 {
			short = string(rs[:12]) + "…"
		}
		parts = append(parts, style.DimText.Render("["+short+"]"))
	}
	if t.provider != "" {
		short := t.provider
		// "api" is already short; only the verbose "claude-code" needs an abbreviation.
		if short == "claude-code" {
			short = "cc"
		}
		parts = append(parts, style.DimText.Render(short))
	}
	if t.model != "" {
		// "claude-opus-4-7[1m]" → "opus-4-7"
		short := strings.TrimSuffix(t.model, "[1m]")
		short = strings.TrimPrefix(short, "claude-")
		parts = append(parts, style.DimText.Render(short))
	}
	if t.turnCount > 0 {
		// Cost chip threshold-colored: green under $1, yellow at $1+, red at $5+.
		// Low session cost = normal; persistent climb past these breakpoints is
		// the kind of "is this still running?" signal worth tagging.
		costStr := fmt.Sprintf("$%.4f", t.lastCost)
		var costStyle lipgloss.Style
		switch {
		case t.lastCost >= 5.0:
			costStyle = style.ErrorStyle
		case t.lastCost >= 1.0:
			costStyle = style.NeedsAttention
		default:
			costStyle = style.Done
		}
		parts = append(parts, costStyle.Render(costStr))
		turnsLabel := fmt.Sprintf("%d turn", t.turnCount)
		if t.turnCount != 1 {
			turnsLabel += "s"
		}
		parts = append(parts, style.DimText.Render(turnsLabel))
	}
	if len(t.autoApproveTools) > 0 {
		names := make([]string, 0, len(t.autoApproveTools))
		for name := range t.autoApproveTools {
			names = append(names, name)
		}
		sort.Strings(names)
		chip := "auto: " + strings.Join(names, ", ")
		// Auto-approve set persists across turns — surface it in attention-yellow
		// so users notice the "I forgot I turned that on" foot-gun.
		parts = append(parts, style.NeedsAttention.Render(chip))
	}
	out := strings.Join(parts, " · ")
	// Single-line guarantee: clamp to terminal width so the bar never wraps
	// and consumes extra rows (which would shift the history panel off-screen).
	// ANSI escape bytes from lipgloss styles count toward rune length but not
	// visible columns, so we allow +20 headroom before cutting.
	if t.width > 0 {
		out = truncateLineByRune(out, t.width+20)
	}
	return out
}

// SetSessionID lets the root inject a sessionID (e.g. from the session
// picker resuming a past chat). The chat tab uses it on the next Enter
// to continue that session instead of starting a fresh one.
//
// Resets the cached session metadata fields (name/provider/model/cost/
// turn count) so the metadata bar doesn't show stale data from a prior
// session. The real values get populated by the follow-up
// ChatHistoryLoadedMsg the root dispatches with the picked session's
// persisted metadata + messages.
func (t *ChatTab) SetSessionID(id string) {
	t.sessionID = id
	t.sessionName = ""
	t.provider = ""
	t.model = ""
	t.turnCount = 0
	t.lastCost = 0
	t.lastErr = nil
}

// SessionID returns the current sessionID. Used by the root to detect
// when the active chat session was deleted from the picker so it can
// reset the chat tab to a fresh state.
func (t *ChatTab) SessionID() string { return t.sessionID }

// Reset clears all per-session state so the next Enter starts a fresh
// chat session. Triggered by Ctrl-N (in-tab) or by selecting the
// "+ New session" entry in the picker.
//
// Notable: streaming flag is intentionally NOT cleared — if a stream is
// in flight, the user shouldn't be able to start a new session until it
// finishes (the Ctrl-N keybind is gated on !streaming for the same
// reason). Reset() is therefore safe to call without aborting an
// in-flight stream by accident.
func (t *ChatTab) Reset() {
	t.sessionID = ""
	t.sessionName = ""
	t.provider = ""
	t.model = ""
	t.turnCount = 0
	t.lastCost = 0
	t.frames = nil
	t.pendingConfirms = map[string]int{}
	t.autoApproveTools = map[string]bool{}
	t.scrollOffset = 0
	t.lastErr = nil
	t.input.SetValue("")
}

// SetSessionName lets the root inject a new session name (e.g. from the
// rename modal). Updates the metadata bar immediately.
func (t *ChatTab) SetSessionName(name string) {
	t.sessionName = name
}

// ResolveByEdit marks the named tool_proposed frame as Resolved+Approved
// and removes it from pendingConfirms. Called by the root after the
// ChatEditArgsModal closes with a successful submit. The matching
// TabChatConfirmRequest with EditedInput is dispatched separately to
// the daemon by the root.
//
// The second parameter is unused today but accepted to keep the
// interface stable for a future "render the edited args inline"
// enhancement.
func (t *ChatTab) ResolveByEdit(toolCallID string, _ json.RawMessage) {
	idx, ok := t.pendingConfirms[toolCallID]
	if !ok {
		return
	}
	if idx >= 0 && idx < len(t.frames) {
		t.frames[idx].Resolved = true
		t.frames[idx].Approved = true
	}
	delete(t.pendingConfirms, toolCallID)
}

// SeedPendingConfirmForTest inserts a synthetic tool_proposed frame into the
// tab's frame list and registers it in pendingConfirms. Only for use in tests
// that need a pending confirm without driving a full streaming path.
func (t *ChatTab) SeedPendingConfirmForTest(toolCallID string) {
	t.frames = append(t.frames, chatFrameView{
		Kind:       "tool_proposed",
		ToolCallID: toolCallID,
		Tool:       "test_tool",
	})
	t.pendingConfirms[toolCallID] = len(t.frames) - 1
}

// IsFrameResolvedForTest returns true if the tool_proposed frame for the given
// toolCallID has been marked Resolved+Approved (i.e. ResolveByEdit was called
// successfully). Only for use in tests.
func (t *ChatTab) IsFrameResolvedForTest(toolCallID string) bool {
	// pendingConfirms is deleted on resolve; also check frame state directly.
	for _, f := range t.frames {
		if f.ToolCallID == toolCallID {
			return f.Resolved && f.Approved
		}
	}
	return false
}

func (t *ChatTab) KeyHelp() string {
	// A pending tool proposal takes priority over the streaming spinner: the
	// model pauses mid-turn (streaming=true) awaiting the confirm, so the
	// spinner must not hide how to action it. (Dogfood fix)
	if len(t.pendingConfirms) > 0 {
		return style.NeedsAttention.Render("tool proposed: y approve · n deny · a approve-all · e edit · c cancel")
	}
	if t.streaming {
		return t.streamSpinner.View() + " streaming…"
	}
	return "enter send · ↑↓/PgUp/PgDn/wheel scroll · y/n/a confirm · c cancel · e edit · t tool-results · r rename · ctrl+n new"
}

// chat.Frame is referenced in streaming frame handling; keep import live.
var _ = chat.Frame{}
