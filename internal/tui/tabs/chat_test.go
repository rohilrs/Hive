package tabs

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/chat"
)

// ansiRe matches ANSI escape sequences so tests can strip them before doing
// plain-text content assertions. Glamour inserts per-word ANSI codes which
// would otherwise break multi-word Contains checks.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[mGKHF]`)

// stripANSI removes ANSI terminal escape sequences from s.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func TestChatTabNameAndInitialState(t *testing.T) {
	tab := NewChat()
	if tab.Name() != "Chat" {
		t.Errorf("Name=%q, want %q", tab.Name(), "Chat")
	}
	if tab.streaming {
		t.Errorf("initial streaming=true, want false")
	}
	if len(tab.frames) != 0 {
		t.Errorf("initial frames non-empty: %d", len(tab.frames))
	}
}

func TestChatTabEnterEmitsSendRequest(t *testing.T) {
	tab := NewChat()
	tab.input.SetValue("hello world")

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyEnter})
	tab = updated.(*ChatTab)

	if cmd == nil {
		t.Fatal("Update returned nil cmd; want one emitting TabChatSendRequest")
	}
	// Enter now batches the send request with a spinner.Tick (so the
	// "streaming…" indicator animates from the moment the user presses
	// Enter, before the daemon's first frame arrives). Unwrap the batch.
	req, ok := findChatSendRequest(cmd())
	if !ok {
		t.Fatalf("Enter batch missing TabChatSendRequest; got %T", cmd())
	}
	if req.Message != "hello world" {
		t.Errorf("Message=%q, want %q", req.Message, "hello world")
	}
	if !tab.streaming {
		t.Errorf("streaming=false after send, want true")
	}
	if tab.input.Value() != "" {
		t.Errorf("input not cleared: %q", tab.input.Value())
	}
}

func TestChatTabEnterIgnoredWhenStreaming(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	tab.input.SetValue("should be ignored")
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyEnter})
	tab = updated.(*ChatTab)
	if cmd != nil {
		t.Errorf("expected nil cmd while streaming, got non-nil")
	}
	if !tab.streaming {
		t.Errorf("streaming flipped to false")
	}
	if len(tab.frames) != 0 {
		t.Errorf("frames non-empty: %+v", tab.frames)
	}
	if tab.input.Value() != "should be ignored" {
		t.Errorf("input mutated: %q", tab.input.Value())
	}
}

func TestChatTabEnterIgnoredWhenEmpty(t *testing.T) {
	tab := NewChat()
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyEnter})
	tab = updated.(*ChatTab)
	if cmd != nil {
		t.Errorf("expected nil cmd on empty input, got non-nil")
	}
	if tab.streaming {
		t.Errorf("streaming flipped to true")
	}
	if len(tab.frames) != 0 {
		t.Errorf("frames non-empty: %+v", tab.frames)
	}
}

func TestChatTabEnterAppendsUserFrame(t *testing.T) {
	tab := NewChat()
	tab.input.SetValue("a message")
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyEnter})
	tab = updated.(*ChatTab)
	if len(tab.frames) != 1 {
		t.Fatalf("frames=%d, want 1", len(tab.frames))
	}
	if tab.frames[0].Kind != "user" || tab.frames[0].Text != "a message" {
		t.Errorf("frame=%+v", tab.frames[0])
	}
}

func TestChatTabAppendsTextFrame(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{Kind: "text", Text: "hello back"}})
	tab = updated.(*ChatTab)
	if len(tab.frames) != 1 {
		t.Fatalf("frames=%d, want 1", len(tab.frames))
	}
	if tab.frames[0].Kind != "text" || tab.frames[0].Text != "hello back" {
		t.Errorf("frame=%+v", tab.frames[0])
	}
}

func TestChatTabAppendsToolResultFrame(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_result", Tool: "hive_status", Result: `{"pending_tasks":2}`,
	}})
	tab = updated.(*ChatTab)
	if len(tab.frames) != 1 || tab.frames[0].Kind != "tool_result" {
		t.Errorf("frames=%+v", tab.frames)
	}
	if tab.frames[0].Tool != "hive_status" {
		t.Errorf("Tool=%q", tab.frames[0].Tool)
	}
}

func TestChatTabSessionFrameCapturesSessionID(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{Kind: "session", Text: "sess-abc"}})
	tab = updated.(*ChatTab)
	if tab.sessionID != "sess-abc" {
		t.Errorf("sessionID=%q, want sess-abc", tab.sessionID)
	}
	// "session" frame should NOT appear in the rendered history (it's metadata).
	if len(tab.frames) != 0 {
		t.Errorf("session frame leaked into history: %+v", tab.frames)
	}
}

func TestChatTabTurnDoneAppendsCard(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "turn_done", Model: "claude-opus-4-7", CostUSD: 0.0123,
	}})
	tab = updated.(*ChatTab)
	if len(tab.frames) != 1 || tab.frames[0].Kind != "turn_done" {
		t.Fatalf("frames=%+v", tab.frames)
	}
	if tab.frames[0].Text != "claude-opus-4-7" || tab.frames[0].CostUSD != 0.0123 {
		t.Errorf("frame=%+v", tab.frames[0])
	}
}

func TestChatTabStreamEndedClearsStreaming(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatStreamEndedMsg{Err: nil})
	tab = updated.(*ChatTab)
	if tab.streaming {
		t.Error("streaming still true after ChatStreamEndedMsg")
	}
}

func TestChatTabStreamEndedWithErrSetsLastErr(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatStreamEndedMsg{Err: errors.New("boom")})
	tab = updated.(*ChatTab)
	if tab.lastErr == nil || tab.lastErr.Error() != "boom" {
		t.Errorf("lastErr=%v, want 'boom'", tab.lastErr)
	}
}

func TestChatTabErrorFrameSetsLastErr(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{Kind: "error", Text: "rpc fail"}})
	tab = updated.(*ChatTab)
	if tab.lastErr == nil || tab.lastErr.Error() != "rpc fail" {
		t.Errorf("lastErr=%v, want 'rpc fail'", tab.lastErr)
	}
	if len(tab.frames) != 1 || tab.frames[0].Kind != "error" {
		t.Errorf("frames=%+v", tab.frames)
	}
}

func TestTruncateMiddleShortPassthrough(t *testing.T) {
	got := truncateMiddle("hello", 10)
	if got != "hello" {
		t.Errorf("got %q, want passthrough", got)
	}
}

func TestTruncateMiddleLongCutsCleanly(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := truncateMiddle(long, 100)
	if len(got) > 100 {
		t.Errorf("len=%d, want <=100", len(got))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("missing ellipsis: %q", got)
	}
}

func TestTruncateMiddleHandlesUnicode(t *testing.T) {
	// 4-byte codepoints (👋) should be sliced rune-wise, not byte-wise.
	long := strings.Repeat("👋", 60) // 240 bytes, 60 runes
	got := truncateMiddle(long, 30)
	// Result must be valid UTF-8.
	for _, r := range got {
		_ = r // iterating successfully means no invalid sequences
	}
	if !strings.Contains(got, "...") {
		t.Errorf("missing ellipsis: %q", got)
	}
}

func TestChatTabToolProposedTracksPendingConfirm(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	tab.sessionID = "sess-1"
	// tool_proposed wire body shape (from daemonConfirmGate, see 6.1b-i):
	// chat.Frame{Kind:"tool_proposed", Tool:"hive_edit_task",
	//   Result:`{"tool_call_id":"cc-99","input":{...}}`}
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_proposed", Tool: "hive_edit_task",
		Result: `{"tool_call_id":"cc-99","input":{"task_id":"t1","title":"x"}}`,
	}})
	tab = updated.(*ChatTab)
	if len(tab.frames) != 1 || tab.frames[0].Kind != "tool_proposed" {
		t.Fatalf("frames=%+v", tab.frames)
	}
	if tab.frames[0].ToolCallID != "cc-99" {
		t.Errorf("ToolCallID=%q, want cc-99", tab.frames[0].ToolCallID)
	}
	if idx, ok := tab.pendingConfirms["cc-99"]; !ok || idx != 0 {
		t.Errorf("pendingConfirms[cc-99]=%v ok=%v, want 0/true", idx, ok)
	}
}

func TestChatTabYApprovesLatestPending(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	tab.sessionID = "sess-1"
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_proposed", Tool: "hive_edit_task",
		Result: `{"tool_call_id":"cc-99","input":{}}`,
	}})

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	tab = updated.(*ChatTab)
	if cmd == nil {
		t.Fatal("y while pending confirm: cmd is nil; want TabChatConfirmRequest")
	}
	req, ok := cmd().(TabChatConfirmRequest)
	if !ok {
		t.Fatalf("got %T, want TabChatConfirmRequest", cmd())
	}
	if req.ToolCallID != "cc-99" || !req.Approve || req.SessionID != "sess-1" {
		t.Errorf("req=%+v", req)
	}
	if !tab.frames[0].Resolved || !tab.frames[0].Approved {
		t.Errorf("frame not marked resolved+approved: %+v", tab.frames[0])
	}
	if _, still := tab.pendingConfirms["cc-99"]; still {
		t.Errorf("pendingConfirms still has cc-99 after resolution")
	}
}

func TestChatTabNDeniesLatestPending(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	tab.sessionID = "sess-1"
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_proposed", Tool: "hive_edit_task",
		Result: `{"tool_call_id":"cc-99","input":{}}`,
	}})
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	tab = updated.(*ChatTab)
	req, ok := cmd().(TabChatConfirmRequest)
	if !ok {
		t.Fatalf("got %T, want TabChatConfirmRequest", cmd())
	}
	if req.Approve {
		t.Errorf("Approve=true, want false")
	}
	if tab.frames[0].Approved {
		t.Errorf("frame Approved=true after n press")
	}
	if !tab.frames[0].Resolved {
		t.Errorf("frame Resolved=false after n press")
	}
}

func TestChatTabYIgnoredWhenInputNonEmpty(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	tab.input.SetValue("typing y means I'm not confirming")
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_proposed", Tool: "hive_edit_task",
		Result: `{"tool_call_id":"cc-99","input":{}}`,
	}})
	_, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd != nil {
		// We want the y to be passed through to textinput, not consumed as confirm.
		// textinput.Update will return a cmd (blink/nil); the key assert is that
		// it's NOT a TabChatConfirmRequest.
		if msg := cmd(); msg != nil {
			if _, ok := msg.(TabChatConfirmRequest); ok {
				t.Errorf("y consumed as confirm despite non-empty input")
			}
		}
	}
}

func TestChatTabYIgnoredWhenNoPendingConfirm(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	_, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(TabChatConfirmRequest); ok {
				t.Errorf("y consumed as confirm despite no pending")
			}
		}
	}
}

func TestChatTabViewToolResultAsFirstFrame(t *testing.T) {
	// Defensive: if a tool_result is the very first frame (no preceding
	// user or text frame), View should still open a "hive" speaker block
	// rather than render an orphan card.
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "tool_result", Tool: "hive_status", Result: `{"pending_tasks":0}`},
	}
	out := tab.View()
	if strings.Count(out, "─── hive ───") != 1 {
		t.Errorf("expected exactly 1 hive header, got %d. View=\n%s",
			strings.Count(out, "─── hive ───"), out)
	}
	if !strings.Contains(out, "hive_status") {
		t.Errorf("tool name missing from rendered output. View=\n%s", out)
	}
}

func TestChatTabDuplicateToolCallIDAutoResolvesStale(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	// First tool_proposed
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_proposed", Tool: "hive_edit_task",
		Result: `{"tool_call_id":"cc-99","input":{"title":"v1"}}`,
	}})
	// Same id again
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "tool_proposed", Tool: "hive_edit_task",
		Result: `{"tool_call_id":"cc-99","input":{"title":"v2"}}`,
	}})
	if len(tab.frames) != 2 {
		t.Fatalf("frames=%d, want 2", len(tab.frames))
	}
	// Older card auto-marked resolved+denied:
	if !tab.frames[0].Resolved || tab.frames[0].Approved {
		t.Errorf("older card not auto-denied: %+v", tab.frames[0])
	}
	// Newer card stays pending:
	if tab.frames[1].Resolved {
		t.Errorf("newer card prematurely resolved: %+v", tab.frames[1])
	}
	// pendingConfirms now points at the newer card (index 1):
	if idx, ok := tab.pendingConfirms["cc-99"]; !ok || idx != 1 {
		t.Errorf("pendingConfirms[cc-99]=%v ok=%v, want 1/true", idx, ok)
	}
}

func TestChatTabViewGroupsConsecutiveAssistantFrames(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	// One user frame, then two consecutive text frames from the assistant.
	tab.frames = []chatFrameView{
		{Kind: "user", Text: "hi"},
		{Kind: "text", Text: "hello"},
		{Kind: "text", Text: "how are you"},
	}
	out := tab.View()
	// Match on the header separator pattern to count speaker blocks precisely
	// (avoids false positives from body text like "how are you" or "hive_tool").
	youHeaders := strings.Count(out, "─── you ───")
	hiveHeaders := strings.Count(out, "─── hive ───")
	if youHeaders != 1 {
		t.Errorf("expected exactly 1 'you' header, got %d. View=\n%s", youHeaders, out)
	}
	if hiveHeaders != 1 {
		t.Errorf("expected exactly 1 'hive' header (consecutive text frames merge), got %d. View=\n%s", hiveHeaders, out)
	}
	// Strip ANSI escape codes before content assertions: glamour inserts per-word
	// color codes which would split "how are you" across separate escape sequences.
	plain := stripANSI(out)
	if !strings.Contains(plain, "hello") || !strings.Contains(plain, "how are you") {
		t.Errorf("missing assistant text content. View=\n%s", out)
	}
}

func TestChatTabViewAlternatingTurnsEmitNewHeaders(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "user", Text: "Q1"},
		{Kind: "text", Text: "A1"},
		{Kind: "user", Text: "Q2"},
		{Kind: "text", Text: "A2"},
	}
	out := tab.View()
	if strings.Count(out, "─── you ───") != 2 {
		t.Errorf("expected 2 'you' headers, got %d. View=\n%s", strings.Count(out, "─── you ───"), out)
	}
	if strings.Count(out, "─── hive ───") != 2 {
		t.Errorf("expected 2 'hive' headers, got %d. View=\n%s", strings.Count(out, "─── hive ───"), out)
	}
}

func TestChatTabViewToolResultStaysUnderHive(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "user", Text: "list tasks"},
		{Kind: "text", Text: "Checking…"},
		{Kind: "tool_result", Tool: "hive_list_tasks", Result: `[{"id":"t1"}]`},
		{Kind: "text", Text: "Done, 1 task"},
	}
	out := tab.View()
	// Tool result should NOT introduce a new speaker header.
	if strings.Count(out, "─── hive ───") != 1 {
		t.Errorf("expected exactly 1 'hive' header (tool_result stays under hive), got %d. View=\n%s",
			strings.Count(out, "─── hive ───"), out)
	}
	if !strings.Contains(out, "hive_list_tasks") {
		t.Errorf("tool name missing from rendered output. View=\n%s", out)
	}
}

func TestChatTabViewTurnDoneIsHiveFooter(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "user", Text: "hi"},
		{Kind: "text", Text: "hello"},
		{Kind: "turn_done", Text: "claude-opus-4-7", CostUSD: 0.0034},
	}
	out := tab.View()
	// turn_done content should appear AFTER the hive text and NOT introduce a new header.
	if strings.Count(out, "─── hive ───") != 1 {
		t.Errorf("expected 1 'hive' header (turn_done is a footer), got %d. View=\n%s",
			strings.Count(out, "─── hive ───"), out)
	}
	if !strings.Contains(out, "$0.0034") || !strings.Contains(out, "claude-opus-4-7") {
		t.Errorf("turn_done footer content missing. View=\n%s", out)
	}
	// turn_done should come AFTER the assistant text.
	helloIdx := strings.Index(out, "hello")
	costIdx := strings.Index(out, "$0.0034")
	if helloIdx == -1 || costIdx == -1 || costIdx < helloIdx {
		t.Errorf("turn_done not after assistant text. hello@%d cost@%d", helloIdx, costIdx)
	}
}

func TestChatTabViewErrorOpensErrorHeader(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "user", Text: "hi"},
		{Kind: "error", Text: "rpc failed"},
	}
	out := tab.View()
	if !strings.Contains(out, "─── error ───") {
		t.Errorf("expected '─── error ───' speaker header, view=\n%s", out)
	}
	if !strings.Contains(out, "rpc failed") {
		t.Errorf("error text missing. View=\n%s", out)
	}
}

func TestChatTabHandlesWindowSizeMsg(t *testing.T) {
	tab := NewChat()
	updated, _ := tab.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	tab = updated.(*ChatTab)
	if tab.width != 100 || tab.height != 30 {
		t.Errorf("width=%d height=%d, want 100/30", tab.width, tab.height)
	}
}

func TestChatTabUpArrowScrollsWhenInputEmpty(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	for i := 0; i < 50; i++ {
		tab.frames = append(tab.frames, chatFrameView{Kind: "text", Text: fmt.Sprintf("line %d", i)})
	}
	priorOffset := tab.scrollOffset // 0 initially
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyUp})
	tab = updated.(*ChatTab)
	if tab.scrollOffset <= priorOffset {
		t.Errorf("scrollOffset didn't increment on up: prior=%d now=%d", priorOffset, tab.scrollOffset)
	}
}

func TestChatTabUpArrowIgnoredWhenInputNonEmpty(t *testing.T) {
	tab := NewChat()
	tab.input.SetValue("typing something")
	tab.width, tab.height = 80, 20
	for i := 0; i < 50; i++ {
		tab.frames = append(tab.frames, chatFrameView{Kind: "text", Text: fmt.Sprintf("line %d", i)})
	}
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyUp})
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset=%d, want 0 (up should not scroll when input has content)", tab.scrollOffset)
	}
}

func TestChatTabDownArrowDecrementsScroll(t *testing.T) {
	tab := NewChat()
	tab.scrollOffset = 5
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyDown})
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 4 {
		t.Errorf("scrollOffset=%d, want 4", tab.scrollOffset)
	}
}

func TestChatTabDownArrowFloorsAtZero(t *testing.T) {
	tab := NewChat()
	tab.scrollOffset = 0
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyDown})
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset=%d, want 0 (no negative scroll)", tab.scrollOffset)
	}
}

// chatFrameForTest builds a wire-shape chat.Frame for the Update path tests.
func chatFrameForTest(kind, tool, result string) chat.Frame {
	return chat.Frame{Kind: kind, Tool: tool, Result: result}
}

func TestChatTabAKeyApprovesAndAddsToAutoApprove(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s-auto"
	// Set up a tool_proposed frame the way ChatFrameMsg would.
	tab.frames = []chatFrameView{{
		Kind: "tool_proposed", Tool: "hive_list_tasks", ToolCallID: "tc1",
	}}
	tab.pendingConfirms["tc1"] = 0

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	tab = updated.(*ChatTab)

	if !tab.frames[0].Resolved || !tab.frames[0].Approved {
		t.Errorf("frame[0] should be Resolved+Approved after a-key")
	}
	if _, still := tab.pendingConfirms["tc1"]; still {
		t.Errorf("tc1 should be removed from pendingConfirms after a-key")
	}
	if !tab.autoApproveTools["hive_list_tasks"] {
		t.Errorf("hive_list_tasks should be in autoApproveTools after a-key")
	}
	if cmd == nil {
		t.Fatal("nil cmd — a-key should emit ChatConfirmRequest")
	}
	req, ok := cmd().(TabChatConfirmRequest)
	if !ok {
		t.Fatalf("got %T, want TabChatConfirmRequest", cmd())
	}
	if !req.Approve || req.ToolCallID != "tc1" {
		t.Errorf("req=%+v, want Approve=true ToolCallID=tc1", req)
	}
}

func TestChatTabSubsequentToolProposedAutoApprovesWhenInSet(t *testing.T) {
	// After `a` adds a tool to the auto-approve set, the next
	// tool_proposed frame for the same tool must resolve immediately
	// without adding to pendingConfirms.
	tab := NewChat()
	tab.sessionID = "s-auto"
	tab.autoApproveTools["hive_list_tasks"] = true

	frame := chatFrameView{}
	_ = frame // unused — we build the wire-shape ChatFrameMsg below
	msg := ChatFrameMsg{Frame: chatFrameForTest("tool_proposed", "hive_list_tasks", `{"tool_call_id":"tc2"}`)}
	updated, cmd := tab.Update(msg)
	tab = updated.(*ChatTab)

	if len(tab.frames) != 1 {
		t.Fatalf("frames=%d, want 1", len(tab.frames))
	}
	if !tab.frames[0].Resolved || !tab.frames[0].Approved {
		t.Errorf("auto-approved frame must be Resolved+Approved")
	}
	if _, still := tab.pendingConfirms["tc2"]; still {
		t.Errorf("auto-approved frame must NOT enter pendingConfirms")
	}
	if cmd == nil {
		t.Fatal("auto-approve should emit a ChatConfirmRequest cmd")
	}
	req, ok := cmd().(TabChatConfirmRequest)
	if !ok {
		t.Fatalf("got %T, want TabChatConfirmRequest", cmd())
	}
	if !req.Approve || req.ToolCallID != "tc2" {
		t.Errorf("req=%+v, want auto-approve for tc2", req)
	}
}

func TestChatTabToolProposedForDifferentToolStillPrompts(t *testing.T) {
	tab := NewChat()
	tab.autoApproveTools["hive_list_tasks"] = true

	msg := ChatFrameMsg{Frame: chatFrameForTest("tool_proposed", "hive_add_task", `{"tool_call_id":"tc3"}`)}
	updated, cmd := tab.Update(msg)
	tab = updated.(*ChatTab)

	if tab.frames[0].Resolved {
		t.Errorf("different tool should NOT be auto-approved")
	}
	if _, in := tab.pendingConfirms["tc3"]; !in {
		t.Errorf("non-auto tool should enter pendingConfirms")
	}
	if cmd != nil {
		t.Errorf("non-auto tool should not emit a cmd: %v", cmd())
	}
}

func TestChatTabResetClearsAutoApproveTools(t *testing.T) {
	tab := NewChat()
	tab.autoApproveTools["hive_status"] = true
	tab.Reset()
	if len(tab.autoApproveTools) != 0 {
		t.Errorf("autoApproveTools survived Reset: %v", tab.autoApproveTools)
	}
}

func TestChatTabResetClearsAllSessionState(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "old-session"
	tab.sessionName = "old name"
	tab.provider = "claude-code"
	tab.model = "claude-opus-4-7"
	tab.turnCount = 3
	tab.lastCost = 0.5
	tab.scrollOffset = 5
	tab.frames = []chatFrameView{{Kind: "text", Text: "old"}}
	tab.pendingConfirms["tcid"] = 0
	tab.input.SetValue("draft text")

	tab.Reset()

	if tab.sessionID != "" {
		t.Errorf("sessionID=%q, want empty", tab.sessionID)
	}
	if tab.sessionName != "" {
		t.Errorf("sessionName=%q, want empty", tab.sessionName)
	}
	if tab.provider != "" || tab.model != "" {
		t.Errorf("provider=%q model=%q, want both empty", tab.provider, tab.model)
	}
	if tab.turnCount != 0 || tab.lastCost != 0 {
		t.Errorf("turnCount=%d lastCost=%f, want 0/0", tab.turnCount, tab.lastCost)
	}
	if len(tab.frames) != 0 {
		t.Errorf("frames=%d, want 0", len(tab.frames))
	}
	if len(tab.pendingConfirms) != 0 {
		t.Errorf("pendingConfirms=%d, want 0", len(tab.pendingConfirms))
	}
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset=%d, want 0", tab.scrollOffset)
	}
	if tab.input.Value() != "" {
		t.Errorf("input draft survived reset: %q", tab.input.Value())
	}
}

func TestChatTabCtrlNResetsSessionWhenNotStreaming(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "old"
	tab.frames = []chatFrameView{{Kind: "text", Text: "x"}}
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	tab = updated.(*ChatTab)
	if tab.sessionID != "" {
		t.Errorf("sessionID survived Ctrl-N: %q", tab.sessionID)
	}
	if len(tab.frames) != 0 {
		t.Errorf("frames survived Ctrl-N: %d", len(tab.frames))
	}
}

func TestChatTabCtrlNIgnoredWhileStreaming(t *testing.T) {
	// Mid-turn Ctrl-N must not strand the in-flight stream against a
	// cleared sessionID. Keep state intact until streaming finishes.
	tab := NewChat()
	tab.sessionID = "live"
	tab.streaming = true
	tab.frames = []chatFrameView{{Kind: "text", Text: "in-flight"}}
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	tab = updated.(*ChatTab)
	if tab.sessionID != "live" {
		t.Errorf("sessionID cleared mid-stream: %q", tab.sessionID)
	}
	if len(tab.frames) != 1 {
		t.Errorf("frames cleared mid-stream: %d", len(tab.frames))
	}
}

func TestChatTabMouseWheelUpScrollsRegardlessOfInput(t *testing.T) {
	// Wheel scroll fires even when the input has content (no arrow-key
	// gate) — wheel can't conflict with typing.
	tab := NewChat()
	tab.input.SetValue("typing something")
	tab.width, tab.height = 80, 20
	prior := tab.scrollOffset
	updated, _ := tab.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	tab = updated.(*ChatTab)
	if tab.scrollOffset <= prior {
		t.Errorf("scrollOffset didn't increment on wheel up: prior=%d now=%d", prior, tab.scrollOffset)
	}
}

func TestChatTabMouseWheelDownDecrementsAndFloorsAtZero(t *testing.T) {
	tab := NewChat()
	tab.scrollOffset = 3
	updated, _ := tab.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 2 {
		t.Errorf("scrollOffset=%d, want 2", tab.scrollOffset)
	}
	// Wheel down at 0 stays at 0.
	tab.scrollOffset = 0
	updated, _ = tab.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset=%d, want 0 (no negative scroll on wheel)", tab.scrollOffset)
	}
}

func TestChatTabViewRendersTwoBoxesWhenSized(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 24
	tab.frames = []chatFrameView{{Kind: "user", Text: "hi"}}
	out := tab.View()
	// Both boxes use rounded borders → at least two top-border corners (╭).
	topCorners := strings.Count(out, "╭")
	if topCorners < 2 {
		t.Errorf("expected 2+ ╭ (history box + input box), got %d. View=\n%s", topCorners, out)
	}
}

func TestChatTabViewFlatWhenUnsized(t *testing.T) {
	tab := NewChat()
	// width/height both 0 = no WindowSizeMsg yet → flat fallback
	tab.frames = []chatFrameView{{Kind: "user", Text: "hi"}}
	out := tab.View()
	// Flat mode → no rounded-border corners
	if strings.Contains(out, "╭") {
		t.Errorf("flat fallback rendered border corners. View=\n%s", out)
	}
}

func TestChatTabPgDownFloorsAtZero(t *testing.T) {
	tab := NewChat()
	tab.scrollOffset = 3
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset=%d, want 0 (PgDown of 5 from 3 should floor at 0)", tab.scrollOffset)
	}
}

func TestChatTabSessionInfoFrameSetsNameProvider(t *testing.T) {
	tab := NewChat()
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "session_info", Result: `{"name":"smoke run","provider":"claude-code"}`,
	}})
	tab = updated.(*ChatTab)
	if tab.sessionName != "smoke run" {
		t.Errorf("sessionName=%q, want 'smoke run'", tab.sessionName)
	}
	if tab.provider != "claude-code" {
		t.Errorf("provider=%q, want 'claude-code'", tab.provider)
	}
	if len(tab.frames) != 0 {
		t.Errorf("session_info leaked into frames: %+v", tab.frames)
	}
}

func TestChatTabSessionInfoIgnoresEmptyFields(t *testing.T) {
	tab := NewChat()
	tab.sessionName = "existing"
	tab.provider = "api"
	updated, _ := tab.Update(ChatFrameMsg{Frame: chat.Frame{
		Kind: "session_info", Result: `{"name":"","provider":""}`,
	}})
	tab = updated.(*ChatTab)
	if tab.sessionName != "existing" {
		t.Errorf("sessionName overwritten to empty: %q", tab.sessionName)
	}
	if tab.provider != "api" {
		t.Errorf("provider overwritten to empty: %q", tab.provider)
	}
}

func TestChatTabTurnDoneTracksModelAndCount(t *testing.T) {
	tab := NewChat()
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{Kind: "turn_done", Model: "claude-opus-4-7", CostUSD: 0.01}})
	_, _ = tab.Update(ChatFrameMsg{Frame: chat.Frame{Kind: "turn_done", Model: "claude-opus-4-7", CostUSD: 0.0345}})
	if tab.model != "claude-opus-4-7" {
		t.Errorf("model=%q", tab.model)
	}
	if tab.turnCount != 2 {
		t.Errorf("turnCount=%d, want 2", tab.turnCount)
	}
	if tab.lastCost != 0.0345 {
		t.Errorf("lastCost=%f, want 0.0345", tab.lastCost)
	}
}

func TestChatTabViewIncludesMetadataBar(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "sess-1"
	tab.sessionName = "smoke approved"
	tab.provider = "claude-code"
	tab.model = "claude-opus-4-7[1m]"
	tab.turnCount = 3
	tab.lastCost = 0.0345
	out := tab.View()
	if !strings.Contains(out, "smoke approved") {
		t.Errorf("name missing: %s", out)
	}
	if !strings.Contains(out, "cc") {
		t.Errorf("provider abbreviation 'cc' missing: %s", out)
	}
	if !strings.Contains(out, "opus-4-7") {
		t.Errorf("model abbreviation missing: %s", out)
	}
	if !strings.Contains(out, "3 turns") {
		t.Errorf("turn count missing: %s", out)
	}
	if !strings.Contains(out, "$0.0345") {
		t.Errorf("cost missing: %s", out)
	}
}

func TestChatTabSetSessionName(t *testing.T) {
	tab := NewChat()
	tab.SetSessionName("renamed")
	if tab.sessionName != "renamed" {
		t.Errorf("sessionName=%q, want 'renamed'", tab.sessionName)
	}
}

func TestChatTabMetadataBarEmptyWhenNoSession(t *testing.T) {
	tab := NewChat()
	out := tab.View()
	// Fresh tab, no sessionID, no sessionName → no metadata bar.
	if strings.Contains(out, " · ") {
		t.Errorf("unexpected metadata separator in fresh tab view: %s", out)
	}
}

func TestChatTabViewMetadataBarApiProviderPassesThrough(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "sess-1"
	tab.sessionName = "api session"
	tab.provider = "api"
	tab.model = "claude-haiku-4-5"
	out := tab.View()
	// "api" isn't abbreviated; "cc" should NOT appear
	if !strings.Contains(out, "api") {
		t.Errorf("api provider missing: %s", out)
	}
	if strings.Contains(out, "·  cc ") {
		t.Errorf("unexpected cc abbreviation for api provider: %s", out)
	}
}

func TestChatTabRKeyOpensRenameMsg(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "sess-1"
	tab.sessionName = "old name"
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd == nil {
		t.Fatal("r should emit OpenChatRenameMsg when input empty + sessionID set")
	}
	open, ok := cmd().(OpenChatRenameMsg)
	if !ok {
		t.Fatalf("got %T, want OpenChatRenameMsg", cmd())
	}
	if open.SessionID != "sess-1" || open.CurrentName != "old name" {
		t.Errorf("open=%+v", open)
	}
	_ = updated
}

func TestChatTabRKeyIgnoredWhenNoSession(t *testing.T) {
	tab := NewChat()
	_, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(OpenChatRenameMsg); ok {
				t.Errorf("r emitted rename msg with no sessionID")
			}
		}
	}
}

func TestChatTabRKeyIgnoredWhenInputNonEmpty(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "sess-1"
	tab.input.SetValue("typing words including r")
	_, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, ok := msg.(OpenChatRenameMsg); ok {
				t.Errorf("r emitted rename msg despite non-empty input")
			}
		}
	}
}

func TestChatTabKeyHelpMentionsRename(t *testing.T) {
	tab := NewChat()
	help := tab.KeyHelp()
	if !strings.Contains(help, "r rename") {
		t.Errorf("KeyHelp missing rename hint: %q", help)
	}
}

func TestChatTabViewClampsLongHistoryLinesToWidth(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 40, 20
	tab.frames = []chatFrameView{{
		Kind:   "tool_result",
		Tool:   "hive_list_projects",
		Result: strings.Repeat("x", 500),
	}}
	out := tab.View()
	// No rendered line should exceed tab.width by more than the ANSI + border
	// budget (border 2 + padding 2 + ANSI tolerance 20 = 24 extra runes max).
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > tab.width+24 {
			t.Errorf("line exceeds width budget: len=%d, width=%d, line=%q",
				len([]rune(line)), tab.width, line)
		}
	}
}

func TestChatTabMetadataBarClampedToWidth(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 30, 20
	tab.sessionID = "sess-1"
	tab.sessionName = strings.Repeat("very-long-session-name-", 5)
	tab.provider = "claude-code"
	tab.model = "claude-opus-4-7"
	tab.turnCount = 99
	bar := tab.renderMetadataBar()
	// Allow +20 for ANSI escape bytes from lipgloss styles.
	if len([]rune(bar)) > tab.width+20 {
		t.Errorf("metadata bar too long for width %d: %d runes (%q)", tab.width, len([]rune(bar)), bar)
	}
}

func TestChatTabViewRendersTransientToolErrorSoftly(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{
		{Kind: "tool_result", Tool: "hive_list_projects", Result: `<tool_use_error>Error: No such tool available: hive_list_projects</tool_use_error>`},
	}
	out := tab.View()
	// Transient hint must appear.
	if !strings.Contains(out, "transient") {
		t.Errorf("transient hint missing for tool_use_error: %s", out)
	}
	// Raw error body must NOT appear — only the soft hint is shown.
	if strings.Contains(out, "No such tool available") {
		t.Errorf("raw tool_use_error leaked into render: %s", out)
	}
	// The tool name label must still appear so users know which tool was called.
	if !strings.Contains(out, "hive_list_projects") {
		t.Errorf("tool name missing from transient error render: %s", out)
	}
}

func TestChatTabHistoryLoadedRebuildsFrames(t *testing.T) {
	tab := NewChat()
	// Pre-populate something that should be wiped.
	tab.frames = []chatFrameView{{Kind: "user", Text: "stale"}}
	tab.scrollOffset = 5
	updated, _ := tab.Update(ChatHistoryLoadedMsg{
		SessionID: "sess-resumed",
		Messages: []ChatHistoryMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "hello"},
			{Role: "tool", Content: `{"x":1}`},
			{Role: "error", Content: "boom"},
		},
	})
	tab = updated.(*ChatTab)
	if tab.sessionID != "sess-resumed" {
		t.Errorf("sessionID=%q, want 'sess-resumed'", tab.sessionID)
	}
	if len(tab.frames) != 4 {
		t.Fatalf("frames=%d, want 4 (stale frame should be wiped)", len(tab.frames))
	}
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset=%d, want 0 (should reset to bottom on resume)", tab.scrollOffset)
	}
	if tab.frames[0].Kind != "user" || tab.frames[0].Text != "first" {
		t.Errorf("frame[0]=%+v", tab.frames[0])
	}
	if tab.frames[1].Kind != "text" || tab.frames[1].Text != "hello" {
		t.Errorf("frame[1]=%+v", tab.frames[1])
	}
	if tab.frames[2].Kind != "tool_result" {
		t.Errorf("frame[2].Kind=%q, want tool_result", tab.frames[2].Kind)
	}
	if tab.frames[3].Kind != "error" {
		t.Errorf("frame[3].Kind=%q, want error", tab.frames[3].Kind)
	}
}

func TestChatTabViewRendersSuccessfulToolResultAsCheckmark(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{{
		Kind: "tool_result", Tool: "hive_list_projects",
		Result: `[{"id":"p1","slug":"hive","name":"Hive"}]`,
	}}
	out := tab.View()
	if !strings.Contains(out, "hive_list_projects") {
		t.Errorf("tool name missing: %s", out)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("success checkmark missing: %s", out)
	}
	// Body should NOT be in the rendered output.
	if strings.Contains(out, "p1") || strings.Contains(out, "slug") {
		t.Errorf("tool body leaked into compact render: %s", out)
	}
}

func TestChatTabViewRendersStubToolResultAsUnavailable(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{{
		Kind: "tool_result", Tool: "hive_resume",
		Result: `{"error":"not available in this version","tool":"hive_resume"}`,
	}}
	out := tab.View()
	if !strings.Contains(out, "hive_resume") {
		t.Errorf("tool name missing: %s", out)
	}
	if !strings.Contains(out, "(unavailable)") {
		t.Errorf("unavailable marker missing: %s", out)
	}
	if strings.Contains(out, "not available in this version") {
		t.Errorf("stub body leaked: %s", out)
	}
}

func TestChatTabRendersMarkdownBold(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{
		{Kind: "text", Text: "You have **3 projects**: Hive, Smoke, Test"},
	}
	out := tab.View()
	// Glamour converts **bold** to ANSI bold codes — the raw asterisks should be gone.
	if strings.Contains(out, "**3 projects**") {
		t.Errorf("literal markdown asterisks leaked through glamour: %s", out)
	}
	// The text content should still be present in some form.
	if !strings.Contains(out, "3 projects") {
		t.Errorf("project count text missing entirely: %s", out)
	}
}

func TestChatTabRendersMarkdownList(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{
		{Kind: "text", Text: "Three projects:\n- Hive\n- Smoke\n- Test"},
	}
	out := tab.View()
	// Glamour renders list items with a bullet glyph or its own marker.
	// Regardless of rendering style, the content text should still be present.
	if !strings.Contains(out, "Hive") || !strings.Contains(out, "Smoke") || !strings.Contains(out, "Test") {
		t.Errorf("list items missing from rendered output: %s", out)
	}
}

func TestFlattenMarkdownTablesBasic(t *testing.T) {
	in := `before
| col1 | col2 |
|------|------|
| a    | bcdef |
| longer | g |
after`
	out := flattenMarkdownTables(in)
	// Separator row must be gone.
	if strings.Contains(out, "|------|") {
		t.Errorf("separator row not removed: %s", out)
	}
	// Pipe-delimited data rows must be gone.
	if strings.Contains(out, "| a    | bcdef |") {
		t.Errorf("pipes not stripped: %s", out)
	}
	// Cell content should still be present as stacked blocks.
	if !strings.Contains(out, "a") || !strings.Contains(out, "bcdef") || !strings.Contains(out, "longer") {
		t.Errorf("content lost: %s", out)
	}
	// Before/after text untouched.
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("non-table content damaged: %s", out)
	}
	// Output must NOT be wrapped in a code fence (new stacked-block format).
	if strings.Contains(out, "```") {
		t.Errorf("unexpected code fence in stacked-block output: %s", out)
	}
}

func TestFlattenMarkdownTablesIgnoresNonTablePipes(t *testing.T) {
	// Single line with | but no separator row → not a table
	in := "use | as a pipe operator | in shell"
	out := flattenMarkdownTables(in)
	if out != in {
		t.Errorf("non-table pipe line was modified: got %q, want %q", out, in)
	}
}

func TestFlattenMarkdownTablesPreservesSurroundingMarkdown(t *testing.T) {
	in := `## Heading

Here are the **bold** items:

| name | value |
|------|-------|
| foo  | 1     |
| bar  | 2     |

And a list:
- one
- two`
	out := flattenMarkdownTables(in)
	if !strings.Contains(out, "## Heading") {
		t.Errorf("heading lost: %s", out)
	}
	if !strings.Contains(out, "**bold**") {
		t.Errorf("bold lost: %s", out)
	}
	if !strings.Contains(out, "- one") || !strings.Contains(out, "- two") {
		t.Errorf("list lost: %s", out)
	}
	if strings.Contains(out, "|------|") {
		t.Errorf("separator still present: %s", out)
	}
	// Table content should be rendered as stacked blocks — NOT in a code fence.
	if strings.Contains(out, "```") {
		t.Errorf("unexpected code fence in stacked-block output: %s", out)
	}
	if !strings.Contains(out, "foo") || !strings.Contains(out, "bar") {
		t.Errorf("table row content lost: %s", out)
	}
}

// TestChatTabViewDoesNotTruncateStyledLineThatVisiblyFits asserts that a
// glamour-styled line (with embedded ANSI escape sequences) whose VISIBLE
// width is well within the panel budget does NOT get a spurious trailing
// "…". Before this fix, truncateLineByRune ran unconditionally and counted
// the ANSI escape bytes toward the rune length, overshooting the budget
// and appending an ellipsis to short bold/italic lines.
func TestChatTabViewDoesNotTruncateStyledLineThatVisiblyFits(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	// Use a "text" frame that glamour will style (bold heading): visible
	// width is ~30 columns, well under the panel's ~76-col budget, but
	// the rendered string contains \x1b[...m bold codes that inflate the
	// raw rune count past +12 tolerance.
	tab.frames = []chatFrameView{{
		Kind: "text",
		Text: "**Hive (smoke test)**  ·  hive",
	}}
	out := tab.View()
	// Only check lines that contain our content — the textinput placeholder
	// at the bottom of the view legitimately ends with "…" and we don't want
	// to false-positive on it.
	plain := stripANSI(out)
	if !strings.Contains(plain, "Hive (smoke test)") || !strings.Contains(plain, "hive") {
		t.Fatalf("content missing from view: %s", plain)
	}
	for _, line := range strings.Split(plain, "\n") {
		if !strings.Contains(line, "Hive (smoke test)") {
			continue
		}
		if strings.HasSuffix(strings.TrimRight(line, " │╮╯"), "…") {
			t.Errorf("unexpected trailing '…' on visibly-fitting styled line: %q\nfull view:\n%s", line, out)
		}
	}
}

func TestChatTabRendersTableWithoutBoxChars(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{{
		Kind: "text",
		Text: "Projects:\n\n| name | slug |\n|------|------|\n| Hive | hive |\n| Smoke | smoke-test |",
	}}
	out := tab.View()
	plain := stripANSI(out)
	// Box-drawing chars used by glamour table rendering must NOT appear
	// (they collide with our panel border).
	for _, bad := range []string{"┌", "┐", "└", "┘", "├", "┤", "┬", "┴", "┼", "═"} {
		if strings.Contains(out, bad) {
			t.Errorf("table-box char %q rendered in chat tab view; expected flattened table: %s", bad, out)
		}
	}
	// Content should still appear (visible somewhere, possibly with ANSI styling around it).
	if !strings.Contains(plain, "Hive") || !strings.Contains(plain, "Smoke") {
		t.Errorf("table content missing: %s", plain)
	}
}

func TestMetadataBarShowsAutoApproveChip(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s1"
	tab.width = 100
	tab.autoApproveTools["hive_list_tasks"] = true
	tab.autoApproveTools["hive_status"] = true
	out := tab.renderMetadataBar()
	plain := stripANSI(out)
	if !strings.Contains(plain, "auto: hive_list_tasks, hive_status") {
		t.Errorf("chip missing or wrong order: %q", plain)
	}
}

func TestMetadataBarOmitsAutoApproveChipWhenEmpty(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s1"
	tab.width = 100
	out := tab.renderMetadataBar()
	plain := stripANSI(out)
	if strings.Contains(plain, "auto:") {
		t.Errorf("chip should be hidden when set is empty: %q", plain)
	}
}

// TestRenderMetadataBarCostStylesByThreshold confirms the cost chip's
// foreground SGR shifts with the session-cost thresholds (V1 picks):
// under $1 → Done (green), $1+ → NeedsAttention (yellow), $5+ →
// ErrorStyle (red). Lipgloss downgrades hex foregrounds to the closest
// ANSI16/256 entry depending on the active color profile; under headless
// CI we observe bright-ANSI16 SGRs (92/93/91). Skips when no ANSI is
// emitted (mirrors style_test.go::TestKeyIsBold's gating pattern).
func TestRenderMetadataBarCostStylesByThreshold(t *testing.T) {
	cases := []struct {
		name       string
		cost       float64
		wantSGR    string // bright-ANSI16 SGR observed under headless lipgloss
		costSubstr string
	}{
		{"under_dollar_green", 0.5, "\x1b[92m", "$0.5000"},
		{"over_dollar_yellow", 1.5, "\x1b[93m", "$1.5000"},
		{"over_five_red", 6.0, "\x1b[91m", "$6.0000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLICOLOR_FORCE", "1") // coax termenv into emitting color even without TTY
			tab := NewChat()
			tab.sessionID = "s1"
			tab.width = 100
			tab.turnCount = 1
			tab.lastCost = tc.cost
			out := tab.renderMetadataBar()
			if !strings.Contains(out, "\x1b[") {
				t.Skip("ANSI output not emitted in this env; skipping")
			}
			plain := stripANSI(out)
			if !strings.Contains(plain, tc.costSubstr) {
				t.Errorf("cost chip %q missing from bar: %q", tc.costSubstr, plain)
			}
			if !strings.Contains(out, tc.wantSGR) {
				t.Errorf("expected SGR %q for cost=%.2f; bar=%q",
					tc.wantSGR, tc.cost, out)
			}
		})
	}
}

// TestRenderMetadataBarAutoChipUsesAttentionColor confirms the persistent
// auto-approve chip renders in NeedsAttention (yellow) — the
// "I forgot I turned that on" foot-gun signal. Skip-fallback matches
// style_test.go's TestKeyIsBold pattern; SGR target is the bright-ANSI16
// yellow lipgloss picks under headless profiles.
func TestRenderMetadataBarAutoChipUsesAttentionColor(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	tab := NewChat()
	tab.sessionID = "s1"
	tab.width = 100
	tab.autoApproveTools["hive_add_task"] = true
	out := tab.renderMetadataBar()
	if !strings.Contains(out, "\x1b[") {
		t.Skip("ANSI output not emitted in this env; skipping")
	}
	plain := stripANSI(out)
	if !strings.Contains(plain, "auto:") {
		t.Errorf("auto chip missing from bar: %q", plain)
	}
	if !strings.Contains(out, "\x1b[93m") {
		t.Errorf("expected NeedsAttention bright-yellow SGR for auto chip; bar=%q", out)
	}
}

func TestChatTabPreservesScrollOffsetWhenFrameArrives(t *testing.T) {
	// When the user is scrolled up (offset > 0) and a new frame
	// arrives, scrollOffset must NOT reset to 0 — the user should
	// keep reading where they were. Sticky-bottom only applies at
	// offset == 0.
	tab := NewChat()
	tab.width, tab.height = 80, 20
	for i := 0; i < 50; i++ {
		tab.frames = append(tab.frames, chatFrameView{Kind: "text", Text: fmt.Sprintf("line %d", i)})
	}
	tab.scrollOffset = 10 // simulate user having scrolled up

	// Simulate a new frame arriving via ChatFrameMsg.
	msg := ChatFrameMsg{Frame: chatFrameForTest("text", "", "new content")}
	updated, _ := tab.Update(msg)
	tab = updated.(*ChatTab)

	if tab.scrollOffset != 10 {
		t.Errorf("scrollOffset mutated to %d after frame arrival; want 10 (preserved)", tab.scrollOffset)
	}
}

func TestChatTabStaysAtBottomWhenAlreadyThere(t *testing.T) {
	// Counterpart: when the user is at the bottom (offset == 0), a
	// new frame should keep them at the bottom (showing newest).
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.scrollOffset = 0
	msg := ChatFrameMsg{Frame: chatFrameForTest("text", "", "newest")}
	updated, _ := tab.Update(msg)
	tab = updated.(*ChatTab)
	if tab.scrollOffset != 0 {
		t.Errorf("scrollOffset moved away from bottom: %d", tab.scrollOffset)
	}
}

func TestChatTabRendersCancelledDistinctFromDenied(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{
		{Kind: "tool_proposed", Tool: "hive_add_task", ToolCallID: "tc1", Resolved: true, Approved: false, Cancelled: true},
		{Kind: "tool_proposed", Tool: "hive_run_now", ToolCallID: "tc2", Resolved: true, Approved: false, Cancelled: false},
	}
	out := tab.renderHistory(76)
	plain := stripANSI(out)
	if !strings.Contains(plain, "cancelled") {
		t.Errorf("cancelled frame missing 'cancelled' label: %q", plain)
	}
	if strings.Count(plain, "cancelled") != 1 {
		t.Errorf("'cancelled' label should appear exactly once (only the Cancelled frame), got %d in %q", strings.Count(plain, "cancelled"), plain)
	}
}

func TestChatTabPendingHintShowsAllResolveKeys(t *testing.T) {
	tab := NewChat()
	tab.width, tab.height = 80, 20
	tab.frames = []chatFrameView{
		{Kind: "tool_proposed", Tool: "hive_add_task", ToolCallID: "tc1"},
	}
	out := tab.renderHistory(76)
	plain := stripANSI(out)
	if !strings.Contains(plain, "[y/n/a/e/c]") {
		t.Errorf("pending hint missing all resolve keys: %q", plain)
	}
}

func TestChatTabCKeyMarksFrameCancelledAndEmitsReason(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s-cancel"
	tab.frames = []chatFrameView{{
		Kind: "tool_proposed", Tool: "hive_add_task", ToolCallID: "tc1",
	}}
	tab.pendingConfirms["tc1"] = 0

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	tab = updated.(*ChatTab)

	if !tab.frames[0].Resolved {
		t.Errorf("frame should be Resolved after c")
	}
	if !tab.frames[0].Cancelled {
		t.Errorf("frame should be Cancelled after c")
	}
	if tab.frames[0].Approved {
		t.Errorf("frame should NOT be Approved after c")
	}
	if _, still := tab.pendingConfirms["tc1"]; still {
		t.Errorf("pendingConfirms should not retain tc1 after c-key")
	}
	if cmd == nil {
		t.Fatal("expected a TabChatConfirmRequest cmd")
	}
	msg := cmd()
	req, ok := msg.(TabChatConfirmRequest)
	if !ok {
		t.Fatalf("got %T, want TabChatConfirmRequest", msg)
	}
	if req.Approve {
		t.Errorf("Approve=true on cancel; want false")
	}
	if req.Reason != "user cancelled, do not retry" {
		t.Errorf("Reason=%q, want 'user cancelled, do not retry'", req.Reason)
	}
	if req.ToolCallID != "tc1" {
		t.Errorf("ToolCallID=%q, want tc1", req.ToolCallID)
	}
}

func TestChatTabCKeyGatedOnNonEmptyInput(t *testing.T) {
	// c is gated like y/n/a: empty input + at least one pending.
	tab := NewChat()
	tab.sessionID = "s"
	tab.input.SetValue("not empty")
	tab.frames = []chatFrameView{{Kind: "tool_proposed", Tool: "x", ToolCallID: "tc1"}}
	tab.pendingConfirms["tc1"] = 0
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	tab = updated.(*ChatTab)
	if tab.frames[0].Resolved {
		t.Errorf("c should not fire when input non-empty")
	}
}

func TestChatTabCKeyNoOpWhenNoPending(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s"
	tab.frames = []chatFrameView{{Kind: "tool_proposed", Tool: "x", ToolCallID: "tc1", Resolved: true}}
	// pendingConfirms intentionally empty
	updated, _ := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	tab = updated.(*ChatTab)
	// Frame should be unchanged
	if tab.frames[0].Cancelled {
		t.Errorf("c should not mark a frame as Cancelled when nothing pending")
	}
}

func TestChatTabEKeyEmitsOpenEditModal(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s"
	// daemonConfirmGate.Propose sends the wire envelope shape:
	// {"tool_call_id":"...","input":{...}}. The e-keybind handler
	// must extract `input` before passing to the modal.
	envelope := json.RawMessage(`{"tool_call_id":"cc-12345","input":{"title":"original"}}`)
	expectedArgs := `{"title":"original"}`
	tab.frames = []chatFrameView{{
		Kind: "tool_proposed", Tool: "hive_add_task", ToolCallID: "tc-edit",
		Result: string(envelope),
	}}
	tab.pendingConfirms["tc-edit"] = 0

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	_ = updated
	if cmd == nil {
		t.Fatal("expected OpenChatEditArgsMsg cmd")
	}
	msg, ok := cmd().(OpenChatEditArgsMsg)
	if !ok {
		t.Fatalf("got %T, want OpenChatEditArgsMsg", cmd())
	}
	if msg.ToolCallID != "tc-edit" {
		t.Errorf("ToolCallID=%q, want tc-edit", msg.ToolCallID)
	}
	if msg.ToolName != "hive_add_task" {
		t.Errorf("ToolName=%q, want hive_add_task", msg.ToolName)
	}
	if string(msg.Args) != expectedArgs {
		t.Errorf("Args=%s, want %s (extracted from envelope)", msg.Args, expectedArgs)
	}
}

func TestChatTabEKeyHandlesMalformedEnvelopeAsEmpty(t *testing.T) {
	// Defensive: if the Result doesn't parse as the envelope shape
	// (e.g. a frame from a different code path), fall back to
	// empty args rather than passing garbage to the modal.
	tab := NewChat()
	tab.sessionID = "s"
	tab.frames = []chatFrameView{{
		Kind: "tool_proposed", Tool: "x", ToolCallID: "tc1",
		Result: "not valid json",
	}}
	tab.pendingConfirms["tc1"] = 0

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	_ = updated
	msg, ok := cmd().(OpenChatEditArgsMsg)
	if !ok {
		t.Fatalf("got %T, want OpenChatEditArgsMsg", cmd())
	}
	if string(msg.Args) != "{}" {
		t.Errorf("Args=%s, want '{}' fallback for malformed envelope", msg.Args)
	}
}

func TestChatTabEKeyGatedOnNonEmptyInput(t *testing.T) {
	tab := NewChat()
	tab.sessionID = "s"
	tab.input.SetValue("typing")
	tab.frames = []chatFrameView{{Kind: "tool_proposed", Tool: "x", ToolCallID: "tc1", Result: "{}"}}
	tab.pendingConfirms["tc1"] = 0
	_, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if cmd != nil {
		// Could be the textinput's blink — only fail if the cmd produces OpenChatEditArgsMsg
		msg := cmd()
		if _, ok := msg.(OpenChatEditArgsMsg); ok {
			t.Errorf("e should not fire when input non-empty")
		}
	}
}

func TestChatTabResolveByEditMarksFrameApproved(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{{Kind: "tool_proposed", Tool: "x", ToolCallID: "tc1"}}
	tab.pendingConfirms["tc1"] = 0

	tab.ResolveByEdit("tc1", json.RawMessage(`{"k":"v"}`))

	if !tab.frames[0].Resolved || !tab.frames[0].Approved {
		t.Errorf("frame should be Resolved+Approved; got %+v", tab.frames[0])
	}
	if _, still := tab.pendingConfirms["tc1"]; still {
		t.Errorf("pendingConfirms should not retain tc1 after ResolveByEdit")
	}
}

func TestChatTabResolveByEditUnknownIDNoOp(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{{Kind: "tool_proposed", Tool: "x", ToolCallID: "tc1"}}
	tab.pendingConfirms["tc1"] = 0
	tab.ResolveByEdit("unknown", nil)
	if tab.frames[0].Resolved {
		t.Errorf("frame should not be touched for unknown ID")
	}
}

func TestChatTabKeyHelpAdvertisesCancelAndEdit(t *testing.T) {
	tab := NewChat()
	help := tab.KeyHelp()
	if !strings.Contains(help, "c cancel") {
		t.Errorf("KeyHelp missing 'c cancel': %q", help)
	}
	if !strings.Contains(help, "e edit") {
		t.Errorf("KeyHelp missing 'e edit': %q", help)
	}
}

func TestChatTabTKeyEmitsOpenPickerMsg(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "text", Text: "hi"},
		{Kind: "tool_result", Tool: "hive_status", Result: `{"pending_tasks":3}`},
		{Kind: "text", Text: "ack"},
		{Kind: "tool_result", Tool: "hive_list_projects", Result: `[{"slug":"hive"}]`},
	}

	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if cmd == nil {
		t.Fatal("expected OpenChatToolResultPickerMsg cmd")
	}
	msg, ok := cmd().(OpenChatToolResultPickerMsg)
	if !ok {
		t.Fatalf("got %T, want OpenChatToolResultPickerMsg", cmd())
	}
	if len(msg.Rows) != 2 {
		t.Fatalf("rows=%d, want 2", len(msg.Rows))
	}
	// Most-recent-first: row 0 must be hive_list_projects (added last).
	if msg.Rows[0].Tool != "hive_list_projects" {
		t.Errorf("Rows[0].Tool=%q, want hive_list_projects (most-recent-first)", msg.Rows[0].Tool)
	}
	if msg.Rows[1].Tool != "hive_status" {
		t.Errorf("Rows[1].Tool=%q, want hive_status", msg.Rows[1].Tool)
	}
	// State non-mutation: emitting the picker must not touch frames or input.
	// Catches regressions that mutate t.frames or t.input.Value() while
	// building the rows.
	pk := updated.(*ChatTab)
	if len(pk.frames) != 4 {
		t.Errorf("expected frames untouched (len=4); got len=%d", len(pk.frames))
	}
	if pk.input.Value() != "" {
		t.Errorf("expected input untouched (empty); got %q", pk.input.Value())
	}
}

func TestChatTabTKeyGatedOnNonEmptyInput(t *testing.T) {
	tab := NewChat()
	tab.input.SetValue("typing")
	tab.frames = []chatFrameView{
		{Kind: "tool_result", Tool: "hive_status", Result: `{}`},
	}
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if cmd != nil {
		if _, ok := cmd().(OpenChatToolResultPickerMsg); ok {
			t.Errorf("expected t to be ignored when input non-empty")
		}
	}
	// Fall-through: the t keypress must reach the textinput when the gate
	// rejects the picker (catches silent-swallow regressions).
	pk := updated.(*ChatTab)
	if pk.input.Value() != "typingt" {
		t.Errorf("expected t appended to input (fall-through); got %q", pk.input.Value())
	}
}

func TestChatTabTKeyGatedOnNoToolResults(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{{Kind: "text", Text: "hi"}}
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if cmd != nil {
		if _, ok := cmd().(OpenChatToolResultPickerMsg); ok {
			t.Errorf("expected t to be ignored when no tool_result frames")
		}
	}
	// Fall-through: the t keypress must reach the textinput when no
	// tool_result frames exist (catches silent-swallow regressions).
	pk := updated.(*ChatTab)
	if pk.input.Value() != "t" {
		t.Errorf("expected t to reach textinput (fall-through); got input value %q", pk.input.Value())
	}
}

func TestChatTabTKeyIsErrorClassification(t *testing.T) {
	tab := NewChat()
	tab.frames = []chatFrameView{
		{Kind: "tool_result", Tool: "hive_status", Result: `{"ok":true}`},
		{Kind: "tool_result", Tool: "hive_broken", Result: `<tool_use_error>Error: No such tool available: hive_broken</tool_use_error>`},
		{Kind: "tool_result", Tool: "hive_resume", Result: `{"error":"not available in this version","tool":"hive_resume"}`},
	}
	updated, cmd := tab.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	_ = updated
	msg := cmd().(OpenChatToolResultPickerMsg)
	// Most-recent-first: rows[0] is hive_resume, rows[1] is hive_broken, rows[2] is hive_status.
	if !msg.Rows[0].IsError {
		t.Errorf("hive_resume row IsError=false; want true (contains 'not available in this version')")
	}
	if !msg.Rows[1].IsError {
		t.Errorf("hive_broken row IsError=false; want true (contains 'tool_use_error')")
	}
	if msg.Rows[2].IsError {
		t.Errorf("hive_status row IsError=true; want false (no error markers)")
	}
}

func TestChatTabKeyHelpAdvertisesToolResults(t *testing.T) {
	tab := NewChat()
	help := tab.KeyHelp()
	if !strings.Contains(help, "t tool-results") {
		t.Errorf("KeyHelp missing 't tool-results': %q", help)
	}
}

// findChatSendRequest searches a tea.Msg for a TabChatSendRequest.
// Top-level TabChatSendRequest is returned as-is; otherwise the batch
// is unrolled and each child cmd evaluated. Used by tests that need
// to assert against batched Enter cmds (which combine the send request
// with a spinner.Tick).
func findChatSendRequest(msg tea.Msg) (TabChatSendRequest, bool) {
	switch v := msg.(type) {
	case TabChatSendRequest:
		return v, true
	case tea.BatchMsg:
		for _, c := range v {
			if c == nil {
				continue
			}
			if found, ok := findChatSendRequest(c()); ok {
				return found, true
			}
		}
	}
	return TabChatSendRequest{}, false
}

// TestChatTabKeyHelpUsesSpinnerWhenStreaming verifies that KeyHelp swaps
// out the static keybind hint for a spinner + "streaming…" indicator
// while a chat.send stream is in flight. Spinner animation is timer-
// driven; we just confirm the text marker is present and the static
// keybind hint is suppressed.
func TestChatTabKeyHelpUsesSpinnerWhenStreaming(t *testing.T) {
	tab := NewChat()
	tab.streaming = true
	help := tab.KeyHelp()
	if !strings.Contains(help, "streaming") {
		t.Errorf("KeyHelp during streaming missing 'streaming': %q", help)
	}
	if strings.Contains(help, "tool-results") {
		t.Errorf("KeyHelp during streaming should suppress static hint; got %q", help)
	}
}

// TestChatTabKeyHelpShowsConfirmWhilePending verifies that a pending tool
// proposal surfaces its confirm keys in the footer EVEN while streaming —
// the model pauses mid-turn (streaming=true) awaiting the confirm, so the
// spinner must not hide how to action it. Dogfood fix.
func TestChatTabKeyHelpShowsConfirmWhilePending(t *testing.T) {
	tab := NewChat()
	tab.streaming = true // mid-turn: the proposal arrives before turn_done
	tab.SeedPendingConfirmForTest("tc1")
	help := stripANSI(tab.KeyHelp())
	if !strings.Contains(help, "approve") {
		t.Errorf("KeyHelp with a pending confirm should show the confirm keys, got: %q", help)
	}
	if strings.Contains(help, "streaming") {
		t.Errorf("KeyHelp with a pending confirm should show the confirm keys, not the spinner; got: %q", help)
	}
}

// TestChatViewNeverExceedsHeight: the chat tab's View must render within its
// height budget even when scrolled — otherwise the overflow pushes the root
// tab bar off-screen. Regression for the dogfood "scrolling hides the nav
// tabs" bug (the scroll-affordance hints were added beyond historyHeight).
func TestChatViewNeverExceedsHeight(t *testing.T) {
	tab := NewChat()
	for i := 0; i < 60; i++ {
		tab.frames = append(tab.frames, chatFrameView{Kind: "text", Text: fmt.Sprintf("history line %d", i)})
	}
	tab.width = 80
	tab.height = 20
	for _, off := range []int{0, 5, 20, 40} { // bottom, scrolled mid, scrolled far
		tab.scrollOffset = off
		rows := strings.Count(tab.View(), "\n") + 1
		if rows > tab.height {
			t.Errorf("scrollOffset=%d: View rendered %d rows > height %d (tab bar would be clipped)", off, rows, tab.height)
		}
	}
}

