package tabs

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/tui/style"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// EventsReader is the snapshot contract Events needs.
type EventsReader interface {
	RecentEvents() []TimedEvent
}

// eventFilter is the active filter mode for the firehose.
type eventFilter int

const (
	filterNoHeartbeat eventFilter = iota // default: everything except heartbeats + tool.decision
	filterAll                            // everything incl heartbeats
	filterRun                            // run.*
	filterStage                          // stage.*
	filterStall                          // stall.*
	filterTool                           // tool.decision
	filterApproval                       // approval.*
)

// Events renders the cross-run event firehose with a select-to-expand
// cursor: ↑↓ move the highlighted event, whose full detail shows in a
// pane below. Filter keys are letters (NOT numbers — the root intercepts
// 1-5 for tab switching).
type Events struct {
	snap   EventsReader
	filter eventFilter
	cursor int // index into the filtered list (0 = oldest shown)
	offset int // first visible row (list scroll), kept so cursor stays in view
	width  int
	height int
}

// NewEvents constructs the tab. Default filter hides heartbeats + tools.
func NewEvents(snap EventsReader) *Events {
	return &Events{snap: snap, filter: filterNoHeartbeat, cursor: -1}
}

func (e *Events) Name() string  { return "Events" }
func (e *Events) Init() tea.Cmd { return nil }
func (e *Events) KeyHelp() string {
	return "↑↓ select · a all · h hide-noise · r run · s stage · l stall · t tool · p approvals"
}

func (e *Events) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	setFilter := func(f eventFilter) { e.filter = f; e.cursor = -1; e.offset = 0 }
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		e.width = msg.Width
		e.height = msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if e.cursor < 0 {
				e.cursor = len(filterEvents(e.snap.RecentEvents(), e.filter)) - 1
			}
			if e.cursor > 0 {
				e.cursor--
			}
		case "down", "j":
			n := len(filterEvents(e.snap.RecentEvents(), e.filter))
			if e.cursor < 0 {
				e.cursor = n - 1
			} else if e.cursor < n-1 {
				e.cursor++
			}
		case "a":
			setFilter(filterAll)
		case "h":
			setFilter(filterNoHeartbeat)
		case "r":
			setFilter(filterRun)
		case "s":
			setFilter(filterStage)
		case "l":
			setFilter(filterStall)
		case "t":
			setFilter(filterTool)
		case "p":
			setFilter(filterApproval)
		}
	}
	return e, nil
}

func (e *Events) View() string {
	filtered := filterEvents(e.snap.RecentEvents(), e.filter)

	var b strings.Builder
	b.WriteString(style.Header.Render("Events firehose") + "  " +
		style.DimText.Render(filterLabel(e.filter)) + "\n\n")

	if len(filtered) == 0 {
		// Distinguish "nothing at all yet" from "filter excluded
		// everything" — the filter label above already says which
		// filter is active, but a no-events-at-all state deserves
		// a friendlier hint about what populates this tab.
		if len(e.snap.RecentEvents()) == 0 {
			b.WriteString(style.Hint.Render("No events yet — events stream in as runs, stages, and tools fire.") + "\n")
		} else {
			b.WriteString(style.Hint.Render("No events match current filter — press " + style.Key.Render("a") + " for all, " + style.Key.Render("h") + " to hide noise.") + "\n")
		}
		return b.String()
	}

	// Effective cursor: -1 means "follow newest" (track the tail as events
	// arrive); any explicit cursor is clamped into range.
	cur := e.cursor
	if cur < 0 || cur >= len(filtered) {
		cur = len(filtered) - 1
	}

	// Layout: reserve chrome (tab bar 2 + header 2 + col header 1 + status 2
	// + margin 2 = 9) and a fixed detail pane (detailRows + 1 separator).
	detailRows := 6
	listRows := e.height - 9 - (detailRows + 1)
	if listRows < 3 {
		listRows = 3
	}
	// Scroll the list so the cursor stays visible.
	if cur < e.offset {
		e.offset = cur
	}
	if cur >= e.offset+listRows {
		e.offset = cur - listRows + 1
	}
	if e.offset > len(filtered)-listRows {
		e.offset = len(filtered) - listRows
	}
	if e.offset < 0 {
		e.offset = 0
	}
	end := e.offset + listRows
	if end > len(filtered) {
		end = len(filtered)
	}

	// Column header + scroll position.
	pos := ""
	if len(filtered) > listRows {
		pos = fmt.Sprintf("  [%d/%d]", cur+1, len(filtered))
	}
	b.WriteString(style.DimText.Render(fmt.Sprintf("  %-8s  %-18s  %s%s", "TIME", "TYPE", "DETAILS", pos)) + "\n")

	// Scroll affordance: when rows above the window are hidden, render a
	// single dim "↑ N more" line indented to match the list's left margin
	// (the 2-cell cursor-marker column). Empty when offset == 0.
	if hint := style.ScrollHint("up", e.offset); hint != "" {
		b.WriteString("  " + hint + "\n")
	}

	for i := e.offset; i < end; i++ {
		ev := filtered[i]
		styled := eventTypeStyle(ev).Render(fmt.Sprintf("%-18s", string(ev.Type)))
		row := fmt.Sprintf("%-8s  %s  %s", ev.At.Format("15:04:05"), styled, FormatEventDetails(ev))
		b.WriteString(style.CursorMarker(i == cur) + row + "\n")
	}

	// Mirror affordance below the window when rows after `end` are hidden.
	if hint := style.ScrollHint("down", len(filtered)-end); hint != "" {
		b.WriteString("  " + hint + "\n")
	}

	// Detail pane: full, untruncated fields of the selected event.
	b.WriteString("\n" + style.DimText.Render("── selected ──────────") + "\n")
	b.WriteString(renderEventDetail(filtered[cur]))
	return b.String()
}

// renderEventDetail shows the selected event's type/time + all data
// fields (sorted, one per line) — the "expand" half of select-to-expand.
func renderEventDetail(ev TimedEvent) string {
	var b strings.Builder
	b.WriteString(eventTypeStyle(ev).Render(string(ev.Type)) + style.DimText.Render("  "+ev.At.Format("15:04:05")) + "\n")
	keys := make([]string, 0, len(ev.Data))
	for k := range ev.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(fmt.Sprintf("  %s: %v\n", k, ev.Data[k]))
	}
	return b.String()
}

func filterEvents(events []TimedEvent, f eventFilter) []TimedEvent {
	if f == filterAll {
		return events
	}
	var out []TimedEvent
	for _, ev := range events {
		t := string(ev.Type)
		switch f {
		case filterNoHeartbeat:
			// Default view also hides tool.decision — it's the loudest
			// event; `a` (all) shows it, and the drill-in shows per-run.
			if ev.Type == rpc.EventDaemonHeartbeat || ev.Type == rpc.EventToolDecision {
				continue
			}
		case filterRun:
			if !strings.HasPrefix(t, "run.") {
				continue
			}
		case filterStage:
			if !strings.HasPrefix(t, "stage.") {
				continue
			}
		case filterStall:
			if !strings.HasPrefix(t, "stall.") {
				continue
			}
		case filterTool:
			if ev.Type != rpc.EventToolDecision {
				continue
			}
		case filterApproval:
			if !strings.HasPrefix(t, "approval.") {
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

func filterLabel(f eventFilter) string {
	switch f {
	case filterAll:
		return "[all incl heartbeats]"
	case filterNoHeartbeat:
		return "[hiding heartbeats + tool.decision · a=all]"
	case filterRun:
		return "[run.* only]"
	case filterStage:
		return "[stage.* only]"
	case filterStall:
		return "[stall.* only]"
	case filterTool:
		return "[tool.decision only]"
	case filterApproval:
		return "[approval.* only]"
	}
	return ""
}

// eventTypeStyle colors the type column by event class so the firehose
// is scannable: runs cyan, stage-ends green/yellow by verdict, stalls
// red/yellow, heartbeats dim.
func eventTypeStyle(ev TimedEvent) lipgloss.Style {
	switch {
	case ev.Type == rpc.EventDaemonHeartbeat:
		return style.DimText
	case ev.Type == rpc.EventRunEnded:
		if st, _ := ev.Data["status"].(string); st != "" {
			return style.ForStatus(st)
		}
		return style.Done
	case ev.Type == rpc.EventRunStarted:
		return style.Running
	case strings.HasPrefix(string(ev.Type), "stall."):
		return style.NeedsAttention
	case strings.HasPrefix(string(ev.Type), "stage."):
		return style.Running
	default:
		return style.DimText
	}
}

