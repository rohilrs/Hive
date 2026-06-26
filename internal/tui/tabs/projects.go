package tabs

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rohilrs/Hive/internal/tui/style"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// ProjectsReader is the narrow snapshot contract Projects needs.
type ProjectsReader interface {
	AllProjects() []ProjectSummary
	RunsForProject(projectID string) []RunSummary
	TasksForProject(projectID string) []TaskSummary
	PendingApprovalRunIDs() map[string]bool
	// SequenceStatus returns the cached sequence.status view for a sequenced
	// project (nil when not sequenced / not yet fetched) — drives the row
	// badge + seeds the edit modal's target/policy (Phase 4).
	SequenceStatusFor(projectID string) *rpc.SeqStatusView
}

// itemKind distinguishes the entries the user can navigate in the
// right pane. Run rows drill in; task rows dispatch a run.
type itemKind int

const (
	itemRun itemKind = iota
	itemTask
)

// maxRecentDonePerProject caps how many recently-completed tasks the Projects
// task list shows per project. Recent-done is a rolling newest-first feed with
// no time expiry, so without a cap finished work accumulates and crowds out the
// running/queued rows the operator actually acts on.
const maxRecentDonePerProject = 5

// visibleItem is one navigable row in the right pane. Computed in
// visual render order so the J/K cursor maps directly to what the
// user sees (3.7.1c fix — earlier cursor indexed sort-by-ID which
// didn't match render order).
type visibleItem struct {
	Kind itemKind
	ID   string // run ID or task ID
	Run  *RunSummary
	Task *TaskSummary
}

// Projects tab: left sidebar of projects, right pane shows the
// selected project's runs grouped by status + pending tasks.
type Projects struct {
	snap      ProjectsReader
	selected  int // sidebar project cursor
	rowCursor int // right-pane cursor (visual order over runs + tasks)
	width     int
	height    int
	// focusedPane tracks which split is driving navigation so the
	// border color can reflect it. "sidebar" = ↑↓ moves project
	// selection; "main" = j/k moves row cursor. Empty string is
	// treated as "sidebar" (default focus).
	focusedPane string
}

// NewProjects constructs the tab with a snapshot reader.
func NewProjects(snap ProjectsReader) *Projects {
	return &Projects{snap: snap}
}

func (p *Projects) Name() string  { return "Projects" }
func (p *Projects) Init() tea.Cmd { return nil }
func (p *Projects) KeyHelp() string {
	// Keep this under ~110 display cells: the root footer renders the per-tab
	// help on ONE line (truncated to width with "…"), so a long string clips the
	// rightmost keys. Adding a keybind here means tightening labels to refit
	// (this has regressed before — audit width on every addition).
	return "↑↓ nav · ↵ opn · n/N new · e edt · d del · s src · f cfg · S seq · P pln · R rd · H hlt · G grd · C rsv · M mrg"
}

// visibleItems returns the right-pane rows in the order they're
// rendered: needs-attention runs → running runs → recent-done runs →
// queued tasks. The cursor indexes this list directly.
func (p *Projects) visibleItems() []visibleItem {
	projs := p.snap.AllProjects()
	if p.selected >= len(projs) {
		return nil
	}
	projID := projs[p.selected].ID
	runs := p.snap.RunsForProject(projID)
	running, _, recentDone := partitionRuns(runs)
	tasks := p.snap.TasksForProject(projID)

	// A needs_attention task whose runs are done (stuck-merge case) is shown in
	// the needs-attention lane below; drop its done run from recent-done so it
	// doesn't appear twice.
	naTaskIDs := map[string]bool{}
	for i := range tasks {
		if tasks[i].Status == "needs_attention" {
			naTaskIDs[tasks[i].ID] = true
		}
	}
	if len(naTaskIDs) > 0 {
		filtered := recentDone[:0]
		for _, r := range recentDone {
			if r.TaskID != "" && naTaskIDs[r.TaskID] {
				continue
			}
			filtered = append(filtered, r)
		}
		recentDone = filtered
	}
	// Cap recent-done so completed work doesn't crowd out running/queued rows.
	// recentDone is newest-first (partitionRuns sorts EndedAt DESC), so the
	// slice keeps the most recently finished tasks.
	if len(recentDone) > maxRecentDonePerProject {
		recentDone = recentDone[:maxRecentDonePerProject]
	}

	out := make([]visibleItem, 0, len(running)+len(recentDone)+len(tasks))
	addRuns := func(rs []RunSummary) {
		for i := range rs {
			r := rs[i]
			out = append(out, visibleItem{Kind: itemRun, ID: r.ID, Run: &r})
		}
	}
	// Needs attention: tasks whose latest run failed or was abandoned.
	// Shown as task rows (not the abandoned/failed run rows) so the user
	// can re-run or delete them via the task detail modal.
	for i := range tasks {
		t := tasks[i]
		if t.Status == "needs_attention" {
			out = append(out, visibleItem{Kind: itemTask, ID: t.ID, Task: &t})
		}
	}
	addRuns(running)
	addRuns(recentDone)
	// Queued: tasks not yet dispatched. Once acted on, a task either has
	// a running/done run row above or sits in the needs-attention lane.
	for i := range tasks {
		t := tasks[i]
		if t.Status == "" || t.Status == "pending" {
			out = append(out, visibleItem{Kind: itemTask, ID: t.ID, Task: &t})
		}
	}
	return out
}

func (p *Projects) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width = msg.Width
		p.height = msg.Height
	case tea.MouseMsg:
		// Wheel scrolls the main task list by moving the row cursor (the
		// viewport auto-follows it) — so a long list is scrollable without
		// knowing the j/k keys. Focuses main so the cursor + border reflect it.
		switch msg.Type {
		case tea.MouseWheelDown:
			p.focusedPane = "main"
			if items := p.visibleItems(); p.rowCursor < len(items)-1 {
				p.rowCursor++
			}
		case tea.MouseWheelUp:
			p.focusedPane = "main"
			if p.rowCursor > 0 {
				p.rowCursor--
			}
		}
	case tea.KeyMsg:
		switch msg.String() {
		case "up":
			p.focusedPane = "sidebar"
			if p.selected > 0 {
				p.selected--
				p.rowCursor = 0
			}
		case "down":
			p.focusedPane = "sidebar"
			projs := p.snap.AllProjects()
			if p.selected < len(projs)-1 {
				p.selected++
				p.rowCursor = 0
			}
		case "j": // up (user preference, 3.7.5 — j=up, k=down)
			p.focusedPane = "main"
			if p.rowCursor > 0 {
				p.rowCursor--
			}
		case "k": // down
			p.focusedPane = "main"
			items := p.visibleItems()
			if p.rowCursor < len(items)-1 {
				p.rowCursor++
			}
		case "enter":
			items := p.visibleItems()
			if p.rowCursor < 0 || p.rowCursor >= len(items) {
				return p, nil
			}
			it := items[p.rowCursor]
			switch it.Kind {
			case itemRun:
				rid := it.ID
				return p, func() tea.Msg { return DrillInRequest{RunID: rid} }
			case itemTask:
				// Phase 3.7.4: Enter on a task opens the detail view
				// (view/edit body, run, delete) rather than dispatching
				// immediately. 3.7.5: pass associated runs so the modal
				// shows this task's run history.
				tid := it.ID
				title := it.Task.Title
				var runRows []map[string]any
				projs := p.snap.AllProjects()
				if p.selected < len(projs) {
					for _, r := range p.snap.RunsForProject(projs[p.selected].ID) {
						if r.TaskID == tid {
							runRows = append(runRows, map[string]any{
								"id": r.ID, "status": r.Status, "summary": r.Summary,
							})
						}
					}
				}
				return p, func() tea.Msg {
					return TabOpenModalRequest{
						Kind: "task_detail",
						InitialState: map[string]any{
							"task_id": tid, "title": title, "runs": runRows,
						},
					}
				}
			}
		case "N":
			return p, func() tea.Msg {
				return TabOpenModalRequest{Kind: "new_project"}
			}
		case "n":
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				slug := projs[p.selected].Slug
				return p, func() tea.Msg {
					return TabOpenModalRequest{
						Kind:         "new_task",
						InitialState: map[string]any{"project_slug": slug},
					}
				}
			}
		case "P":
			// Open planner session for the selected project. Routes through
			// root → Chat tab → Client.StreamPlannerChat (kind=plan).
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				slug := projs[p.selected].Slug
				return p, func() tea.Msg { return TabPlannerOpenRequest{ProjectSlug: slug} }
			}
			return p, nil
		case "e":
			// Open the Edit Project modal pre-filled with the selected
			// project's current name + repo_path. Submit fires project.edit
			// via the root handler; slug stays immutable.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				proj := projs[p.selected]
				req := TabEditProjectRequest{
					Slug:         proj.Slug,
					Name:         proj.Name,
					RepoPath:     proj.RepoPath,
					Status:       proj.Status,
					DispatchMode: proj.DispatchMode,
					// Target branch is the always-set [scheduler] base, independent
					// of dispatch mode — seed it from the project view so MANUAL
					// projects pre-fill their configured target (not the "main"
					// default). Previously this was only seeded from the sequenced
					// cache below, which is nil for manual projects → modal showed "main".
					Target:            proj.TargetBranch,
					FeatureBranch:     proj.FeatureBranch,
					MergeMethod:       proj.MergeMethod,
					TaskAutoIntegrate: proj.AutoIntegrate,
					AutoFixCI:         proj.AutoFixCI,
					CanSequence:       proj.CanSequence,
				}
				// Advancement policy is sequenced-only — seed it from the cached
				// sequence.status when the project is sequenced.
				if st := p.snap.SequenceStatusFor(proj.ID); st != nil {
					req.Policy = st.Policy
				}
				return p, func() tea.Msg { return req }
			}
			return p, nil
		case "S":
			// Open the Sequence modal for a sequenced project (no-op otherwise).
			// Uses 'S' (not 'q') because the root binds 'q' to quit-TUI, which
			// is handled before tab keys.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) && projs[p.selected].DispatchMode == "sequenced" {
				proj := projs[p.selected]
				return p, func() tea.Msg {
					return TabSequenceRequest{Slug: proj.Slug, ProjectID: proj.ID}
				}
			}
			return p, nil
		case "d":
			// Open the Delete Project confirm modal. Counts come from the
			// snapshot so the modal can show the cascading scope (tasks +
			// runs that will be cascaded out by the daemon's project.delete
			// RPC) without a fresh round-trip.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				proj := projs[p.selected]
				taskCount := len(p.snap.TasksForProject(proj.ID))
				runCount := len(p.snap.RunsForProject(proj.ID))
				return p, func() tea.Msg {
					return TabDeleteProjectRequest{
						Slug:      proj.Slug,
						TaskCount: taskCount,
						RunCount:  runCount,
					}
				}
			}
			return p, nil
		case "s":
			// Open the Sources modal for the selected project. The root
			// handler opens the modal in a "loading" state and fires
			// sources.list to populate it; the operator can then bind /
			// unbind any of the 3 source kinds (github / linear / inbox).
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				slug := projs[p.selected].Slug
				return p, func() tea.Msg {
					return TabSourcesRequest{ProjectSlug: slug}
				}
			}
			return p, nil
		case "f":
			// Open the read-only Repo Config viewer for the selected project's
			// repo (~/.hive/repos/<key>/config.toml). RepoPath is required to
			// resolve the per-repo config.RepoKey; the modal shows a "no repo
			// path" notice when it's empty.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				proj := projs[p.selected]
				return p, func() tea.Msg {
					return TabRepoConfigRequest{Slug: proj.Slug, RepoPath: proj.RepoPath}
				}
			}
			return p, nil
		case "R":
			// Open the Roadmap Viewer modal for the selected project.
			// The modal reads the planner-written roadmap at
			// <repo>/docs/superpowers/roadmaps/<slug>.md and lets the
			// operator pick a phase to decompose. RepoPath is required
			// to resolve the on-disk path; surfaced via the 8.C.2 T1
			// ProjectSummary.RepoPath addition.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				proj := projs[p.selected]
				return p, func() tea.Msg {
					return TabRoadmapViewerRequest{
						ProjectSlug: proj.Slug,
						RepoPath:    proj.RepoPath,
					}
				}
			}
			return p, nil
		case "H":
			// Open the feature-branch health-check modal for the selected
			// project. No-op when the project has no feature branch configured.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				proj := projs[p.selected]
				if proj.FeatureBranch == "" {
					return p, nil // no feature branch → nothing to health-check
				}
				return p, func() tea.Msg {
					return TabFeatureBranchHealthRequest{
						Slug:     proj.Slug,
						RepoPath: proj.RepoPath,
						Feature:  proj.FeatureBranch,
						Target:   proj.TargetBranch,
					}
				}
			}
			return p, nil
		case "G":
			// Open the Graduate modal for the selected project. No-op when the
			// project has no feature branch configured (nothing to graduate —
			// mirrors the H health-check gating). The modal opens in its confirm
			// state; submit fires the async project.graduate RPC.
			projs := p.snap.AllProjects()
			if p.selected < len(projs) {
				proj := projs[p.selected]
				if proj.FeatureBranch == "" {
					return p, nil // no feature branch → nothing to graduate
				}
				return p, func() tea.Msg {
					return TabGraduateRequest{
						Slug:    proj.Slug,
						Feature: proj.FeatureBranch,
						Target:  proj.TargetBranch,
					}
				}
			}
			return p, nil
		case "C":
			// Dispatch the conflict resolver for a stuck (needs_attention) task.
			// No-op on any other row kind or task status — the resolver only
			// makes sense when a task is stuck waiting for human conflict
			// resolution.
			//
			// The spec also lists "awaiting_merge" as a valid trigger, but
			// "awaiting_merge" is a run gate_state (tasks.gate_state), NOT a
			// task status surfaced via TaskSummary.Status. In the snapshot,
			// tasks only ever carry status "pending" | "running" |
			// "needs_attention" | "done"; a task whose run is awaiting_merge
			// will have Status="done" (run ended successfully) and its
			// integration chip (pr_open / ci / merged) communicates the gate
			// state. Those tasks are not stuck and don't need the resolver.
			// If that mapping ever changes, add "awaiting_merge" here.
			items := p.visibleItems()
			if p.rowCursor < 0 || p.rowCursor >= len(items) {
				return p, nil
			}
			it := items[p.rowCursor]
			if it.Kind != itemTask || it.Task.Status != "needs_attention" {
				return p, nil
			}
			tid := it.ID
			return p, func() tea.Msg { return TabResolveTaskRequest{TaskID: tid} }
		case "M":
			// Recover a task parked at the terminal merge_failed gate via
			// merge.retry (reconcile if the PR already merged, else re-arm the
			// merge queue).
			//
			// TaskSummary.Status does not carry gate_state directly —
			// merge_failed surfaces as needs_attention in the snapshot (same as
			// conflict-stuck tasks). We gate on needs_attention here; the daemon
			// refuses a non-merge_failed task, so the restriction is enforced
			// server-side.
			items := p.visibleItems()
			if p.rowCursor < 0 || p.rowCursor >= len(items) {
				return p, nil
			}
			it := items[p.rowCursor]
			if it.Kind != itemTask || it.Task.Status != "needs_attention" {
				return p, nil
			}
			tid := it.ID
			return p, func() tea.Msg { return TabMergeRetryRequest{TaskID: tid} }
		}
	}
	return p, nil
}

func (p *Projects) View() string {
	projs := p.snap.AllProjects()
	if len(projs) == 0 {
		return style.DimText.Render("No projects registered. Add one with N or `hive task add --project <slug> ...`")
	}
	sort.Slice(projs, func(i, j int) bool { return projs[i].Name < projs[j].Name })

	var sidebar, main strings.Builder

	// Inner content height for both panels (panel border adds 2 rows). The tab
	// is sized to its chrome-adjusted budget (app.refreshTabSizes), so the
	// joined panels must fit within p.height or they push the root tab bar
	// off-screen.
	innerH := p.height - 2
	if innerH < 3 {
		innerH = 3
	}

	// Panel widths + their inner content widths. lipgloss .Width is the
	// content+padding width (the rounded border sits outside), so a row wider
	// than contentW soft-wraps onto extra visual rows and overflows the panel
	// height — clipping the bottom border. We truncate every row to contentW so
	// it never wraps. (Headless test renderers can hide this; real terminals
	// wrap — hence the explicit truncation.)
	sidebarWidth := 24
	mainWidth := p.width - sidebarWidth - 4
	if mainWidth < 20 {
		mainWidth = 20
	}
	sidebarContentW := sidebarWidth - 2
	mainContentW := mainWidth - 2

	sidebar.WriteString(style.Header.Render("Projects") + "\n\n")
	for i, pp := range projs {
		marker := style.CursorMarker(i == p.selected)
		line := marker + pp.Name
		if i == p.selected {
			line = style.Running.Render(line)
		}
		sidebar.WriteString(ansi.Truncate(line, sidebarContentW, "…") + "\n")
		// Sequenced projects get a compact badge line (phase progress + gate
		// counts) so their state is glanceable without opening the modal.
		if pp.DispatchMode == "sequenced" {
			if badge := sequencedBadge(p.snap.SequenceStatusFor(pp.ID)); badge != "" {
				sidebar.WriteString(ansi.Truncate("  "+badge, sidebarContentW, "…") + "\n")
			}
		}
	}

	if p.selected < len(projs) {
		sel := projs[p.selected]
		main.WriteString(ansi.Truncate(style.Header.Render(sel.Name+" ("+sel.Slug+")"), mainContentW, "…") + "\n\n")
		if sel.FeatureBranch != "" {
			branchLine := style.DimText.Render("⎇ " + sel.FeatureBranch + " → " + sel.TargetBranch)
			if sel.AutoIntegrate {
				branchLine += " " + style.Key.Render("⤳ auto")
			}
			main.WriteString(ansi.Truncate(branchLine, mainContentW, "…") + "\n\n")
		}

		items := p.visibleItems()
		// Clamp the cursor in case items shrank since last render.
		if p.rowCursor >= len(items) && len(items) > 0 {
			p.rowCursor = len(items) - 1
		}
		// Bound the list to the available height and scroll it around the
		// cursor (with ↑/↓ "more" affordances) so a long task list can't
		// overflow and push the root chrome off-screen.
		lines, cursorLine := buildItemLines(items, p.rowCursor, p.snap.PendingApprovalRunIDs())
		// Truncate each row to the panel content width so long titles can't
		// soft-wrap (which would overflow the panel and clip its bottom border).
		for i := range lines {
			lines[i] = ansi.Truncate(lines[i], mainContentW, "…")
		}
		chromeAbove := 2 // project header + its blank line
		if sel.FeatureBranch != "" {
			chromeAbove += 2 // branch line + its blank line
		}
		listBudget := innerH - chromeAbove
		if listBudget < 1 {
			listBudget = 1
		}
		if p.height <= 0 {
			// Not yet laid out (no WindowSizeMsg) — don't clip; render in full.
			listBudget = len(lines)
		}
		win := windowRows(lines, listBudget, scrollFirstForCursor(cursorLine, len(lines), listBudget))
		main.WriteString(strings.Join(win, "\n"))
	}

	sidebarPanel := style.Panel
	mainPanel := style.Panel
	if p.focusedPane == "" || p.focusedPane == "sidebar" {
		sidebarPanel = style.PanelFocus
	}
	if p.focusedPane == "main" {
		mainPanel = style.PanelFocus
	}
	sidebarStyle := sidebarPanel.Width(sidebarWidth)
	mainStyle := mainPanel.Width(mainWidth)
	if p.height > 0 {
		// Height pads short content so both panels align; MaxHeight is the hard
		// backstop guaranteeing neither panel (e.g. a long sidebar) exceeds the
		// tab budget, since lipgloss Height pads but does not truncate overflow.
		sidebarStyle = sidebarStyle.Height(innerH).MaxHeight(p.height)
		mainStyle = mainStyle.Height(innerH).MaxHeight(p.height)
	}
	sidebarStr := sidebarStyle.Render(sidebar.String())
	mainStr := mainStyle.Render(main.String())
	return lipgloss.JoinHorizontal(lipgloss.Top, sidebarStr, mainStr)
}

// sequencedBadge renders a compact one-line badge for a sequenced project's
// sidebar row: "⚙ paused", "⚙ ✓ done", or "⚙ P2/4 2✓ 1⚠" (active phase
// ordinal/total + satisfied/blocked counts within the active phase). Returns
// "⚙ seq" when the status hasn't been fetched yet.
func sequencedBadge(st *rpc.SeqStatusView) string {
	gear := style.DimText.Render("⚙")
	if st == nil {
		return gear + " " + style.DimText.Render("seq")
	}
	if st.Status == "paused" {
		return gear + " " + style.Danger.Render("paused")
	}
	if st.Complete {
		return gear + " " + style.Done.Render("✓ done")
	}
	ordinal := 0
	var active *rpc.SeqPhaseView
	for i := range st.Phases {
		if st.Phases[i].Number == st.ActivePhase {
			ordinal = i + 1
			active = &st.Phases[i]
			break
		}
	}
	satisfied, blocked := 0, 0
	if active != nil {
		for _, tk := range active.Tasks {
			if tk.GateState == "satisfied" {
				satisfied++
			}
		}
		blocked = len(active.Blocked)
	}
	badge := gear + " " + style.DimText.Render(fmt.Sprintf("P%d/%d", ordinal, len(st.Phases)))
	if satisfied > 0 {
		badge += " " + style.Done.Render(fmt.Sprintf("%d✓", satisfied))
	}
	if blocked > 0 {
		badge += " " + style.Danger.Render(fmt.Sprintf("%d⚠", blocked))
	}
	return badge
}

func partitionRuns(runs []RunSummary) (running, needsAttn, recentDone []RunSummary) {
	seenRunning := map[string]bool{}
	seenAttn := map[string]bool{}
	// done-bucket: keep the run with the greatest EndedAt per task (tie-break:
	// greater ID wins, matching store.ListRecentDoneTasks ORDER BY ended_at DESC,
	// id DESC). Using a map here avoids depending on input order.
	bestDone := map[string]RunSummary{}

	for _, r := range runs {
		switch r.Status {
		case "running":
			if r.TaskID == "" || !seenRunning[r.TaskID] {
				running = append(running, r)
				if r.TaskID != "" {
					seenRunning[r.TaskID] = true
				}
			}
		case "needs_attention", "error":
			if r.TaskID == "" || !seenAttn[r.TaskID] {
				needsAttn = append(needsAttn, r)
				if r.TaskID != "" {
					seenAttn[r.TaskID] = true
				}
			}
		case "done":
			if r.TaskID == "" {
				// un-hydrated: always pass through without collapsing
				recentDone = append(recentDone, r)
				continue
			}
			if cur, exists := bestDone[r.TaskID]; !exists ||
				r.EndedAt > cur.EndedAt ||
				(r.EndedAt == cur.EndedAt && r.ID > cur.ID) {
				bestDone[r.TaskID] = r
			}
		}
	}

	// Collect deduplicated done rows and sort newest-first.
	for _, r := range bestDone {
		recentDone = append(recentDone, r)
	}
	sort.Slice(recentDone, func(i, j int) bool {
		if recentDone[i].EndedAt != recentDone[j].EndedAt {
			return recentDone[i].EndedAt > recentDone[j].EndedAt
		}
		return recentDone[i].ID > recentDone[j].ID
	})
	return
}

// buildItemLines renders the visual-order list into individual lines (section
// headers inserted at status/kind boundaries, ▸ marker at the cursor) and
// returns the line index of the cursor's row so the viewport can scroll to keep
// it visible. Returning lines rather than writing to a builder lets the caller
// window them to the available height.
func buildItemLines(items []visibleItem, cursor int, needsApproval map[string]bool) (lines []string, cursorLine int) {
	if len(items) == 0 {
		return []string{style.DimText.Render("(no runs or queued tasks)")}, 0
	}
	currentSection := ""
	for i, it := range items {
		section := sectionFor(it)
		if section != currentSection {
			if currentSection != "" {
				lines = append(lines, "")
			}
			lines = append(lines, style.Header.Render(section))
			currentSection = section
		}
		marker := style.CursorMarker(i == cursor)
		var line string
		switch it.Kind {
		case itemRun:
			r := it.Run
			// Lead with the task name (status-colored); the run ID + summary
			// follow as dim secondary detail. Falls back to the run ID when
			// the task title hasn't hydrated yet.
			label := r.TaskTitle
			if label == "" {
				label = r.ID
			}
			// Flag runs blocked on a pending approval (resolve in Approvals tab).
			if needsApproval[r.ID] {
				label = "⚠ " + label
			}
			meta := " " + style.DimText.Render(r.ID)
			if r.Summary != "" {
				meta += style.DimText.Render(" · " + r.Summary)
			}
			line = marker + style.ForStatus(r.Status).Render(label) + meta
		case itemTask:
			t := it.Task
			// Status-color the title for non-pending tasks (e.g. the
			// needs_attention lane) so it reads the same as run rows;
			// plain pending tasks stay default-colored.
			label := t.Title
			if t.Status != "" && t.Status != "pending" {
				label = style.ForStatus(t.Status).Render(t.Title)
			}
			// Roadmap order prefix ("[1.2] ") so the list shows phase ordering
			// at a glance; absent for non-roadmap tasks.
			orderPrefix := ""
			if t.Order != "" {
				orderPrefix = style.Key.Render("[" + t.Order + "] ")
			}
			chip := ""
			switch t.IntegrationState {
			case "integrating":
				chip = style.DimText.Render(" finishing…")
			case "pr_open":
				chip = style.Key.Render(fmt.Sprintf(" PR#%d", t.PRNumber))
			case "ci":
				chip = style.DimText.Render(" CI⏳")
			case "merged":
				chip = style.Done.Render(" ✓merged")
			case "blocked":
				chip = style.Danger.Render(" ✗CI")
			}
			line = marker + orderPrefix + label + chip + style.DimText.Render(" ["+t.Status+"]")
		}
		if i == cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, line)
	}
	return lines, cursorLine
}

// windowRows clips lines to at most `height` rows, scrolled so firstVisible is
// at the top, with "↑ N more" / "↓ N more" affordances when content is
// off-window. Guarantees the output is ≤ height so the panel can't overflow.
// (Local copy of the modals roadmap-viewer helper — kept here to avoid a
// cross-package dependency from tabs into modals.)
func windowRows(lines []string, height, firstVisible int) []string {
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

// scrollFirstForCursor returns the first-visible line index that keeps `cursor`
// on screen in a window of `height` rows (anchors the cursor at the window
// bottom once it scrolls past the fold).
func scrollFirstForCursor(cursor, total, height int) int {
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

func sectionFor(it visibleItem) string {
	if it.Kind == itemTask {
		if it.Task.Status == "needs_attention" {
			return "Needs attention (enter to re-run or delete)"
		}
		return "Queued tasks (enter to open)"
	}
	switch it.Run.Status {
	case "running":
		return "In progress"
	case "done":
		return "Recent done"
	}
	return "Other"
}
