package tabs

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// ActiveReader is the snapshot contract Active needs.
type ActiveReader interface {
	ActiveRuns() []ActiveRunSummary
	PendingApprovalRunIDs() map[string]bool
}

// Active tab: cross-project list of running pipelines.
type Active struct {
	snap     ActiveReader
	selected int
	width    int
	height   int
}

func NewActive(snap ActiveReader) *Active { return &Active{snap: snap} }

func (a *Active) Name() string  { return "Active" }
func (a *Active) Init() tea.Cmd { return nil }
func (a *Active) KeyHelp() string {
	return "↑↓ select · enter drill-in · d abandon · D doctor · x clean"
}

func (a *Active) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
	case tea.KeyMsg:
		runs := a.snap.ActiveRuns()
		switch msg.String() {
		case "up", "k":
			if a.selected > 0 {
				a.selected--
			}
		case "down", "j":
			if a.selected < len(runs)-1 {
				a.selected++
			}
		case "enter":
			if a.selected < len(runs) {
				rid := runs[a.selected].ID
				return a, func() tea.Msg { return DrillInRequest{RunID: rid} }
			}
		case "d":
			if a.selected < len(runs) {
				rid := runs[a.selected].ID
				return a, func() tea.Msg {
					return TabOpenModalRequest{
						Kind:         "confirm_abandon",
						InitialState: map[string]any{"run_id": rid},
					}
				}
			}
		case "D":
			// Capital D opens the Doctor modal (TUI mirror of `hive doctor`).
			// Daemon-global, not run-scoped — so it works even with no run
			// selected / no active runs.
			return a, func() tea.Msg { return TabDoctorRequest{} }
		case "x":
			// x opens the Clean modal (TUI mirror of `hive clean`). Also
			// daemon-global; the modal opens with a dry-run preview before
			// any destructive reclaim.
			return a, func() tea.Msg { return TabCleanRequest{} }
		}
	}
	return a, nil
}

func (a *Active) View() string {
	runs := a.snap.ActiveRuns()
	if len(runs) == 0 {
		return style.Hint.Render("No active runs — press " + style.Key.Render("1") + " for Projects, then " + style.Key.Render("n") + " to add a task.")
	}
	// Phase 4.3.1 #4: render children indented under their parent. Helper
	// produces tree-traversal order: each root + its sorted children;
	// snapshot already ID-sorts the input but groupRunsTreeOrder is
	// defensive about input order.
	runs = groupRunsTreeOrder(runs)

	needsApproval := a.snap.PendingApprovalRunIDs()

	var b strings.Builder
	b.WriteString(style.Header.Render("Active runs") + "\n\n")
	header := fmt.Sprintf("  %-22s  %-10s  %-10s  %-10s  %-4s  %-8s  %s",
		"RUN", "PROJECT", "PIPELINE", "STAGE", "ITER", "COST", "TITLE")
	b.WriteString(style.DimText.Render(header) + "\n")
	for i, r := range runs {
		marker := style.CursorMarker(i == a.selected)
		title := r.TaskTitle
		if needsApproval[r.ID] {
			// ⚠ flags a run blocked on a pending approval (go to Approvals).
			title = "⚠ " + title
		}
		// Child fix rows render with a tree marker so the parent→child
		// relationship is visible at a glance. The marker replaces the
		// first two cells of the RUN-column padding, so column widths
		// stay aligned.
		runCell := truncate(r.ID, 22)
		if r.ParentRunID != "" {
			marker = treeMarkerFor(runs, i)
			runCell = truncate(r.ID, 20) // 2 cells absorbed by the marker
		}
		line := fmt.Sprintf("%s%-22s  %-10s  %-10s  %-10s  %-4d  $%-7.4f  %s",
			marker, runCell, r.Project, r.Pipeline, r.Stage, r.Iter, r.CostUSD, title)
		b.WriteString(style.ForStatus(r.Status).Render(line) + "\n")
	}
	if len(needsApproval) > 0 {
		b.WriteString("\n" + style.Running.Render(fmt.Sprintf("⚠ %d run(s) awaiting approval — press 3 for the Approvals tab", len(needsApproval))))
	}
	return b.String()
}

// groupRunsTreeOrder returns runs in tree-traversal order: each root
// run followed by its children (sorted by ID — IDs are timestamp-based
// so ID order = creation order). One level deep — child fix runs don't
// spawn their own children today.
//
// A child whose parent isn't present in the input slice is treated as a
// root in its own right (partial-snapshot resilience: better to render
// the child than to drop it).
func groupRunsTreeOrder(runs []ActiveRunSummary) []ActiveRunSummary {
	// Stable input order first — children's render position depends on
	// the parent's, and we want consistent output regardless of caller.
	in := make([]ActiveRunSummary, len(runs))
	copy(in, runs)
	sort.Slice(in, func(i, j int) bool { return in[i].ID < in[j].ID })

	present := make(map[string]bool, len(in))
	for _, r := range in {
		present[r.ID] = true
	}

	byParent := make(map[string][]ActiveRunSummary)
	var roots []ActiveRunSummary
	for _, r := range in {
		if r.ParentRunID == "" || !present[r.ParentRunID] {
			// True root, or orphan child whose parent isn't visible.
			roots = append(roots, r)
		} else {
			byParent[r.ParentRunID] = append(byParent[r.ParentRunID], r)
		}
	}
	// byParent slices inherit the ID-sort from the pass above, so they
	// render in creation order.

	out := make([]ActiveRunSummary, 0, len(in))
	for _, root := range roots {
		out = append(out, root)
		out = append(out, byParent[root.ID]...)
	}
	return out
}

// treeMarkerFor returns the tree-drawing prefix for a child row at
// index i in the tree-ordered slice. Uses └ when this is the LAST
// child of its parent (next row is a different parent or end of list),
// otherwise ├. The marker is 2 cells wide so column widths line up.
func treeMarkerFor(runs []ActiveRunSummary, i int) string {
	r := runs[i]
	if r.ParentRunID == "" {
		return "  "
	}
	last := i == len(runs)-1 || runs[i+1].ParentRunID != r.ParentRunID
	if last {
		return "└ "
	}
	return "├ "
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
