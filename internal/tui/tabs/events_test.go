package tabs

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/pkg/rpc"
)

type fakeEventsSnap struct{ events []TimedEvent }

func (f *fakeEventsSnap) RecentEvents() []TimedEvent { return f.events }

func te(typ rpc.EventType, data map[string]any) TimedEvent {
	return TimedEvent{At: time.Now(), Type: typ, Data: data}
}

func TestEventsFilterDefaultExcludesHeartbeats(t *testing.T) {
	snap := &fakeEventsSnap{events: []TimedEvent{
		te(rpc.EventRunStarted, map[string]any{"run_id": "r1"}),
		te(rpc.EventDaemonHeartbeat, map[string]any{"ts": float64(123)}),
		te(rpc.EventStageStarted, map[string]any{"run_id": "r1", "name": "implement"}),
	}}
	e := NewEvents(snap)
	e.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := e.View()
	if !strings.Contains(view, "run.started") {
		t.Errorf("missing run.started: %q", view)
	}
	if strings.Contains(view, "daemon.heartbeat") {
		t.Errorf("heartbeats should be filtered by default: %q", view)
	}
}

func TestEventsLetterKeysCycleFilters(t *testing.T) {
	snap := &fakeEventsSnap{events: []TimedEvent{
		te(rpc.EventDaemonHeartbeat, map[string]any{}),
		te(rpc.EventRunStarted, map[string]any{"run_id": "r1"}),
	}}
	e := NewEvents(snap)
	e.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	// 'a' = all → heartbeats shown.
	e.Update(tea.KeyMsg{Runes: []rune{'a'}, Type: tea.KeyRunes})
	if !strings.Contains(e.View(), "daemon.heartbeat") {
		t.Errorf("filter 'a' should show heartbeats: %q", e.View())
	}
	// 'r' = run.* only → no heartbeats.
	e.Update(tea.KeyMsg{Runes: []rune{'r'}, Type: tea.KeyRunes})
	v := e.View()
	if strings.Contains(v, "daemon.heartbeat") {
		t.Errorf("filter 'r' should hide heartbeats: %q", v)
	}
	if !strings.Contains(v, "run.started") {
		t.Errorf("filter 'r' should keep run.started: %q", v)
	}
}

func TestEventsShowsTimestamp(t *testing.T) {
	at := time.Date(2026, 5, 24, 13, 45, 7, 0, time.Local)
	snap := &fakeEventsSnap{events: []TimedEvent{
		{At: at, Type: rpc.EventRunStarted, Data: map[string]any{"run_id": "r1"}},
	}}
	e := NewEvents(snap)
	e.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !strings.Contains(e.View(), "13:45:07") {
		t.Errorf("view missing timestamp: %q", e.View())
	}
}

func TestEventsEmpty(t *testing.T) {
	// No events at all → "No events yet" hint with stream-context.
	e := NewEvents(&fakeEventsSnap{})
	e.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if !strings.Contains(e.View(), "No events yet") {
		t.Errorf("empty view missing 'No events yet' hint: %q", e.View())
	}
}

func TestEventsFilterExcludesAll(t *testing.T) {
	// Events exist but the active filter matches none → filter-aware
	// hint (different copy than the "nothing at all yet" branch).
	snap := &fakeEventsSnap{events: []TimedEvent{
		te(rpc.EventRunStarted, map[string]any{"run_id": "r1"}),
	}}
	e := NewEvents(snap)
	e.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	// Switch to approvals filter; no approval events → filter empty.
	e.Update(tea.KeyMsg{Runes: []rune{'p'}, Type: tea.KeyRunes})
	if !strings.Contains(e.View(), "No events match current filter") {
		t.Errorf("filter-empty view missing helper: %q", e.View())
	}
}

func TestFilterEventsRunOnly(t *testing.T) {
	events := []TimedEvent{
		te(rpc.EventRunStarted, nil),
		te(rpc.EventStageStarted, nil),
		te(rpc.EventStallDetected, nil),
	}
	got := filterEvents(events, filterRun)
	if len(got) != 1 || got[0].Type != rpc.EventRunStarted {
		t.Errorf("run-only filter got=%+v", got)
	}
}

func TestEventsToolFilterAndDetailPane(t *testing.T) {
	snap := &fakeEventsSnap{events: []TimedEvent{
		{Type: rpc.EventRunStarted, Data: map[string]any{"task_title": "T"}},
		{Type: rpc.EventToolDecision, Data: map[string]any{"decision": "deny", "tool_name": "Bash", "arg": "make all"}},
	}}
	e := NewEvents(snap)
	e.height = 40
	// Default hides tool.decision.
	if strings.Contains(e.View(), "make all") {
		t.Error("tool.decision should be hidden by default")
	}
	// `t` focuses tool.decision; the detail pane shows its full fields.
	e.Update(tea.KeyMsg{Runes: []rune{'t'}, Type: tea.KeyRunes})
	v := e.View()
	if !strings.Contains(v, "make all") {
		t.Errorf("t filter should show tool.decision; got:\n%s", v)
	}
	if !strings.Contains(v, "selected") || !strings.Contains(v, "tool_name: Bash") {
		t.Errorf("detail pane should expand the selected event; got:\n%s", v)
	}
}
