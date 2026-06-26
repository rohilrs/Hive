package tui

import (
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/tui/tabs"
	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestRenderDrillInBasic(t *testing.T) {
	s := NewSnapshot()
	s.Runs["r1"] = &RunView{ID: "r1", Status: "running", TaskID: "t1"}
	s.Stages[10] = &StageView{ID: 10, RunID: "r1", Name: "implement", Iter: 0, StartedAt: 1000, Verdict: ""}
	s.Stages[11] = &StageView{ID: 11, RunID: "r1", Name: "review", Iter: 0, StartedAt: 2000, EndedAt: 3000, Verdict: "APPROVE"}
	s.recentEvents = append(s.recentEvents, tabs.TimedEvent{Type: rpc.EventStageStarted, Data: map[string]any{"run_id": "r1", "name": "implement"}})

	view := renderDrillIn(s, "r1", 100, 30, 0)
	if !strings.Contains(view, "implement") || !strings.Contains(view, "review") {
		t.Errorf("missing stage rows: %q", view)
	}
	// Phase 8.C.3 T6: verdict text replaced by colored glyph; APPROVE → ✓.
	if !strings.Contains(view, "✓") {
		t.Errorf("missing approve glyph for APPROVE verdict")
	}
	if !strings.Contains(view, "esc return") {
		t.Errorf("missing exit hint")
	}
}

func TestVerdictGlyph(t *testing.T) {
	cases := []struct {
		verdict  string
		running  bool
		contains string
	}{
		{"APPROVE", false, "✓"},
		{"approved", false, "✓"},
		{"CHANGES_REQUESTED", false, "↺"},
		{"changes_requested", true, "↺"},
		{"retry", false, "↺"},
		{"rejected", false, "✗"},
		{"", true, "…"},
		{"", false, "—"},
		{"some_unrecognized_verdict", false, "some_unrecognized_verdict"},
	}
	for _, c := range cases {
		got := verdictGlyph(c.verdict, c.running)
		if !strings.Contains(got, c.contains) {
			t.Errorf("verdictGlyph(%q,running=%v)=%q want substring %q", c.verdict, c.running, got, c.contains)
		}
	}
}

func TestRenderDrillInUnknownRun(t *testing.T) {
	s := NewSnapshot()
	view := renderDrillIn(s, "missing", 100, 30, 0)
	if !strings.Contains(view, "not found") {
		t.Errorf("missing 'not found' for missing run: %q", view)
	}
}

func TestFilterEventsForRunCaps50(t *testing.T) {
	var events []tabs.TimedEvent
	for i := 0; i < 60; i++ {
		events = append(events, tabs.TimedEvent{
			Type: rpc.EventStageStarted,
			Data: map[string]any{"run_id": "r1", "i": i},
		})
	}
	filtered := filterEventsForRun(events, "r1")
	if len(filtered) != 50 {
		t.Errorf("len(filtered)=%d want 50", len(filtered))
	}
}

func TestRenderDrillInEventsScrollAndNoOverflow(t *testing.T) {
	s := NewSnapshot()
	s.Runs["r1"] = &RunView{ID: "r1", Status: "running", TaskID: "t1"}
	// 60 wide tool.decision events for r1 (more than fit a height-20 panel).
	for i := 0; i < 60; i++ {
		ApplySnapshot(s, rpc.EventMessage{Type: rpc.EventToolDecision, Data: map[string]any{
			"run_id": "r1", "tool_name": "Bash", "decision": "approve",
			"arg": strings.Repeat("x", 200) + " idx", // intentionally very wide
		}})
	}
	height := 20
	// At scroll 0 (newest) the box must not exceed the screen: count lines.
	view := renderDrillIn(s, "r1", 100, height, 0)
	if lines := strings.Count(view, "\n") + 1; lines > height {
		t.Errorf("drill-in rendered %d lines > height %d (overflow/wrap)", lines, height)
	}
	// No rendered line should exceed the terminal width (no wrapping).
	for _, ln := range strings.Split(view, "\n") {
		if w := len([]rune(stripANSI(ln))); w > 100 {
			t.Errorf("line wider than terminal (%d): %q", w, ln)
		}
	}
	// Scrolling back shows a different window (older events).
	top := renderDrillIn(s, "r1", 100, height, 50)
	if top == view {
		t.Errorf("scrolled view should differ from newest view")
	}
}

// stripANSI removes lipgloss escape codes for width assertions.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestRenderDrillInNoScrollWhenEventsFit(t *testing.T) {
	s := NewSnapshot()
	s.Runs["r1"] = &RunView{ID: "r1", Status: "running", TaskID: "t1"}
	// 5 events — well under what fits a height-30 panel.
	for i := 0; i < 5; i++ {
		ApplySnapshot(s, rpc.EventMessage{Type: rpc.EventToolDecision, Data: map[string]any{
			"run_id": "r1", "tool_name": "Read", "decision": "approve", "arg": "file.go",
		}})
	}
	// Even with a non-zero scroll offset, all 5 events must still render
	// (the bug dropped the newest row when content fit).
	base := renderDrillIn(s, "r1", 100, 30, 0)
	scrolled := renderDrillIn(s, "r1", 100, 30, 3)
	countRead := func(v string) int { return strings.Count(v, "Read") }
	if countRead(base) != 5 || countRead(scrolled) != 5 {
		t.Errorf("events that fit must not scroll/drop: base=%d scrolled=%d (want 5,5)", countRead(base), countRead(scrolled))
	}
}
