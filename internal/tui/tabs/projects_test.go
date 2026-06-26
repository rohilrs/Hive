package tabs

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// doneRunsForTasks builds n distinct done runs, newest-last (EndedAt
// increasing), one per task — so partitionRuns yields n recent-done items.
func doneRunsForTasks(n int) []RunSummary {
	rs := make([]RunSummary, n)
	for i := 0; i < n; i++ {
		rs[i] = RunSummary{
			ID:        fmt.Sprintf("run-%02d", i),
			TaskID:    fmt.Sprintf("task-%02d", i),
			TaskTitle: fmt.Sprintf("DoneTask%02d", i),
			Status:    "done",
			EndedAt:   int64(1000 + i), // higher i = newer
		}
	}
	return rs
}

// queuedTasks builds n pending tasks with distinct titles QueuedTaskNN.
func queuedTasks(n int) []TaskSummary {
	ts := make([]TaskSummary, n)
	for i := 0; i < n; i++ {
		ts[i] = TaskSummary{
			ID:     fmt.Sprintf("t%02d", i),
			Title:  fmt.Sprintf("QueuedTask%02d", i),
			Status: "pending",
		}
	}
	return ts
}

// TestProjectsNeedsAttentionTaskNotDuplicatedAsDoneRun: a needs_attention task
// whose runs are done (stuck-merge case) shows once in the needs-attention lane,
// not ALSO as a done row in recent-done.
func TestProjectsNeedsAttentionTaskNotDuplicatedAsDoneRun(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {{ID: "run-1", TaskID: "t1", Status: "done", EndedAt: 100}},
		},
		tasks: map[string][]TaskSummary{
			"p1": {{ID: "t1", Title: "Stuck on merge", Status: "needs_attention"}},
		},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	naCount, doneRun := 0, 0
	for _, it := range p.visibleItems() {
		if it.Kind == itemTask && it.ID == "t1" && it.Task.Status == "needs_attention" {
			naCount++
		}
		if it.Kind == itemRun && it.Run.TaskID == "t1" && it.Run.Status == "done" {
			doneRun++
		}
	}
	if naCount != 1 {
		t.Errorf("needs_attention task should appear once in its lane; got %d", naCount)
	}
	if doneRun != 0 {
		t.Errorf("a needs_attention task's done run should not also show in recent-done; got %d", doneRun)
	}
}

// TestProjectsWheelScrollsMainList: the mouse wheel scrolls the task list
// (moves the row cursor, which the viewport auto-follows) and focuses the main
// pane — so a long list is scrollable without knowing the j/k keys.
func TestProjectsWheelScrollsMainList(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": queuedTasks(20)},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 15})
	if p.rowCursor != 0 {
		t.Fatalf("cursor should start at 0; got %d", p.rowCursor)
	}
	p.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	if p.rowCursor != 1 {
		t.Errorf("wheel down: rowCursor=%d want 1", p.rowCursor)
	}
	if p.focusedPane != "main" {
		t.Errorf("wheel should focus the main pane; got %q", p.focusedPane)
	}
	for i := 0; i < 18; i++ {
		p.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	}
	if p.rowCursor != 19 {
		t.Errorf("wheel down should reach + clamp at the last item; rowCursor=%d want 19", p.rowCursor)
	}
	// One past the end clamps.
	p.Update(tea.MouseMsg{Type: tea.MouseWheelDown})
	if p.rowCursor != 19 {
		t.Errorf("wheel past end must clamp; rowCursor=%d want 19", p.rowCursor)
	}
	// The bottom item is now scrolled into view.
	if v := p.View(); !strings.Contains(v, "QueuedTask19") {
		t.Errorf("wheel-to-bottom item should be visible:\n%s", v)
	}
	p.Update(tea.MouseMsg{Type: tea.MouseWheelUp})
	if p.rowCursor != 18 {
		t.Errorf("wheel up: rowCursor=%d want 18", p.rowCursor)
	}
}

// TestProjectsRecentDoneCappedPerProject: the task list shows at most
// maxRecentDonePerProject done rows, keeping the newest and dropping the rest.
func TestProjectsRecentDoneCappedPerProject(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs:     map[string][]RunSummary{"p1": doneRunsForTasks(8)},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	doneCount := 0
	for _, it := range p.visibleItems() {
		if it.Kind == itemRun && it.Run.Status == "done" {
			doneCount++
		}
	}
	if doneCount != maxRecentDonePerProject {
		t.Errorf("recent-done shown=%d want exactly %d (8 available)", doneCount, maxRecentDonePerProject)
	}

	view := p.View()
	if !strings.Contains(view, "DoneTask07") {
		t.Errorf("newest done task DoneTask07 should be kept:\n%s", view)
	}
	if strings.Contains(view, "DoneTask00") {
		t.Errorf("oldest done task DoneTask00 should be capped out:\n%s", view)
	}
}

// TestProjectsViewClipsToHeight: a long task list must never render more rows
// than the tab's height budget (else it pushes the root tab bar off-screen).
func TestProjectsViewClipsToHeight(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": queuedTasks(30)},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 15})

	view := p.View()
	lines := strings.Count(view, "\n") + 1
	if lines > 15 {
		t.Errorf("view rendered %d lines, exceeds height budget 15:\n%s", lines, view)
	}
}

// TestProjectsViewClipsToTinyHeight: at degenerate heights the floored
// arithmetic (innerH≥3, listBudget≥1) can't keep the raw content under budget —
// the MaxHeight backstop is the sole safeguard. Lock that in so a refactor that
// drops MaxHeight can't silently regress into overflow.
func TestProjectsViewClipsToTinyHeight(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A", FeatureBranch: "feat", TargetBranch: "main"}},
		tasks:    map[string][]TaskSummary{"p1": queuedTasks(30)},
	}
	p := NewProjects(snap)
	for _, h := range []int{3, 4, 5, 6, 7} {
		p.Update(tea.WindowSizeMsg{Width: 100, Height: h})
		view := p.View()
		if lines := strings.Count(view, "\n") + 1; lines > h {
			t.Errorf("height=%d rendered %d lines, exceeds budget:\n%s", h, lines, view)
		}
	}
}

// TestProjectsViewClipsToHeightWithLongTitles: titles wider than the panel must
// be truncated, not soft-wrapped — wrapping inflates the rendered panel past its
// height budget (the bottom border + lower rows get clipped). Real conv-rework
// titles are long; the short-title clip test never exercised this.
func TestProjectsViewClipsToHeightWithLongTitles(t *testing.T) {
	long := strings.Repeat("a very long task title that far exceeds the panel content width ", 3)
	tasks := make([]TaskSummary, 20)
	for i := range tasks {
		tasks[i] = TaskSummary{ID: fmt.Sprintf("t%02d", i), Title: fmt.Sprintf("%02d %s", i, long), Status: "pending"}
	}
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": tasks},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	view := p.View()
	if lines := strings.Count(view, "\n") + 1; lines > 20 {
		t.Errorf("rendered %d lines > height 20", lines)
	}
	// The real symptom: long titles soft-wrap, inflating the MAIN panel past
	// its height budget, so MaxHeight clips its bottom rows — including the
	// main panel's bottom border. (The sidebar's border survives, which is why
	// a naive Contains("╰") is fooled.) The main panel is the right column, so
	// its bottom-right corner "╯" must be the last visible glyph of the last
	// row. Per-row truncation prevents the wrap.
	rows := strings.Split(view, "\n")
	last := strings.TrimRight(stripANSI(rows[len(rows)-1]), " ")
	if !strings.HasSuffix(last, "╯") {
		t.Errorf("main panel bottom border clipped (long titles wrapped + overflowed); last row = %q", last)
	}
}

// TestProjectsViewportScrollsToCursorWithAffordance: the list windows around
// the cursor (auto-scroll keeps it visible) with ↑/↓ "more" affordances.
func TestProjectsViewportScrollsToCursorWithAffordance(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": queuedTasks(30)},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 15})

	// Cursor at top: first item visible, down-affordance present.
	top := p.View()
	if !strings.Contains(top, "QueuedTask00") {
		t.Errorf("cursor-at-top should show first item:\n%s", top)
	}
	if !strings.Contains(top, "↓") {
		t.Errorf("long list should show a down-scroll affordance:\n%s", top)
	}

	// Cursor at the end: last item scrolls into view, up-affordance present.
	p.focusedPane = "main"
	p.rowCursor = 29
	end := p.View()
	if !strings.Contains(end, "QueuedTask29") {
		t.Errorf("cursor-at-end should auto-scroll last item into view:\n%s", end)
	}
	if !strings.Contains(end, "↑") {
		t.Errorf("scrolled list should show an up-scroll affordance:\n%s", end)
	}
}

type fakeProjSnap struct {
	projects []ProjectSummary
	runs     map[string][]RunSummary
	tasks    map[string][]TaskSummary
	seq      map[string]*rpc.SeqStatusView
}

func (f *fakeProjSnap) AllProjects() []ProjectSummary         { return f.projects }
func (f *fakeProjSnap) RunsForProject(id string) []RunSummary { return f.runs[id] }
func (f *fakeProjSnap) TasksForProject(id string) []TaskSummary {
	return f.tasks[id]
}
func (f *fakeProjSnap) PendingApprovalRunIDs() map[string]bool { return nil }
func (f *fakeProjSnap) SequenceStatusFor(id string) *rpc.SeqStatusView {
	if f.seq == nil {
		return nil
	}
	return f.seq[id]
}

func TestProjectsViewRendersAllProjects(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive"},
			{ID: "p2", Slug: "test", Name: "Test"},
		},
		runs: map[string][]RunSummary{
			"p1": {{ID: "r1", Status: "running", Summary: "in-progress"}},
		},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := p.View()
	if !strings.Contains(view, "Hive") {
		t.Errorf("view missing 'Hive': %q", view)
	}
	if !strings.Contains(view, "in-progress") {
		t.Errorf("view missing run summary: %q", view)
	}
}

func TestProjectsTaskListShowsOrderPrefix(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "hive", Name: "Hive"}},
		tasks: map[string][]TaskSummary{
			"p1": {
				{ID: "t1", Title: "Stand up replay harness", Status: "pending", Order: "1.2"},
				{ID: "t2", Title: "Ad-hoc task", Status: "pending"}, // no Order → no prefix
			},
		},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := p.View()
	if !strings.Contains(view, "[1.2]") {
		t.Errorf("ordered task should show [1.2] prefix:\n%s", view)
	}
	if !strings.Contains(view, "Stand up replay harness") {
		t.Errorf("view missing ordered task title:\n%s", view)
	}
	// The unordered task must NOT gain a bracketed order prefix before its title.
	if strings.Contains(view, "[] Ad-hoc") || strings.Contains(view, "[.] Ad-hoc") {
		t.Errorf("unordered task should have no order prefix:\n%s", view)
	}
}

func TestProjectsSelectionMovesWithArrow(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "a", Name: "A"},
			{ID: "p2", Slug: "b", Name: "B"},
		},
	}
	p := NewProjects(snap)
	if p.selected != 0 {
		t.Fatal("start selected != 0")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.selected != 1 {
		t.Errorf("after down selected=%d want 1", p.selected)
	}
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.selected != 0 {
		t.Errorf("after up selected=%d want 0", p.selected)
	}
}

func TestProjectsEmptyMessage(t *testing.T) {
	p := NewProjects(&fakeProjSnap{})
	view := p.View()
	if !strings.Contains(view, "No projects") {
		t.Errorf("empty view missing helper text: %q", view)
	}
}

func TestProjectsEnterEmitsDrillIn(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {{ID: "r1", Status: "running"}},
		},
	}
	p := NewProjects(snap)
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter should return a cmd")
	}
	msg := cmd()
	req, ok := msg.(DrillInRequest)
	if !ok {
		t.Fatalf("got msg type %T want DrillInRequest", msg)
	}
	if req.RunID != "r1" {
		t.Errorf("RunID=%q want r1", req.RunID)
	}
}

func TestProjectsNKeyEmitsNewTaskRequest(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "hive", Name: "Hive"}},
	}
	p := NewProjects(snap)
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'n'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("n should emit cmd")
	}
	req, ok := cmd().(TabOpenModalRequest)
	if !ok {
		t.Fatalf("got %T", cmd())
	}
	if req.Kind != "new_task" {
		t.Errorf("Kind=%q", req.Kind)
	}
	if req.InitialState["project_slug"] != "hive" {
		t.Errorf("missing slug context")
	}
}

func TestProjectsNeedsAttentionTaskOpensModal(t *testing.T) {
	// A task whose run was abandoned/failed sits in needs_attention and
	// must show as a task row (re-runnable / deletable), NOT as the
	// abandoned run row.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {{ID: "run-1", TaskID: "t1", Status: "abandoned"}},
		},
		tasks: map[string][]TaskSummary{
			"p1": {{ID: "t1", Title: "Broken task", Status: "needs_attention"}},
		},
	}
	p := NewProjects(snap)
	view := p.View()
	if !strings.Contains(view, "Needs attention") || !strings.Contains(view, "Broken task") {
		t.Errorf("needs-attention task should be visible; got:\n%s", view)
	}
	// Cursor 0 = the needs-attention task; Enter opens the detail modal.
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on needs-attention task should emit a cmd")
	}
	req, ok := cmd().(TabOpenModalRequest)
	if !ok || req.Kind != "task_detail" {
		t.Fatalf("expected task_detail modal; got %T / %v", cmd(), req.Kind)
	}
	if req.InitialState["task_id"] != "t1" {
		t.Errorf("task_id=%v want t1", req.InitialState["task_id"])
	}
}

func TestProjectsCapitalNKeyEmitsNewProjectRequest(t *testing.T) {
	p := NewProjects(&fakeProjSnap{})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'N'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("N should emit cmd")
	}
	req, ok := cmd().(TabOpenModalRequest)
	if !ok {
		t.Fatalf("got %T", cmd())
	}
	if req.Kind != "new_project" {
		t.Errorf("Kind=%q", req.Kind)
	}
}

func TestProjectsJKMovesRunCursor(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {
				{ID: "run-1", Status: "running"},
				{ID: "run-2", Status: "running"},
				{ID: "run-3", Status: "done"},
			},
		},
	}
	p := NewProjects(snap)
	if p.rowCursor != 0 {
		t.Fatal("rowCursor should start at 0")
	}
	// k = down (3.7.5: j=up, k=down).
	p.Update(tea.KeyMsg{Runes: []rune{'k'}, Type: tea.KeyRunes})
	if p.rowCursor != 1 {
		t.Errorf("k (down) should move rowCursor to 1; got %d", p.rowCursor)
	}
	p.Update(tea.KeyMsg{Runes: []rune{'k'}, Type: tea.KeyRunes})
	if p.rowCursor != 2 {
		t.Errorf("k again should move to 2; got %d", p.rowCursor)
	}
	p.Update(tea.KeyMsg{Runes: []rune{'k'}, Type: tea.KeyRunes})
	if p.rowCursor != 2 {
		t.Errorf("k at end shouldn't overflow; got %d", p.rowCursor)
	}
	// j = up.
	p.Update(tea.KeyMsg{Runes: []rune{'j'}, Type: tea.KeyRunes})
	if p.rowCursor != 1 {
		t.Errorf("j (up) should move back to 1; got %d", p.rowCursor)
	}
}

func TestPartitionRunsDedupsByTask(t *testing.T) {
	// Input is in ASCENDING id / older-first order — exactly how RunsForProject
	// delivers runs (sorted by ID ASC). The old first-seen logic would keep
	// run-100 (the build run); the correct logic must keep run-200 (finish-branch,
	// max EndedAt) — matching what store.ListRecentDoneTasks picks (ORDER BY
	// ended_at DESC, id DESC).
	runs := []RunSummary{
		{ID: "run-100", TaskID: "t1", Status: "done", Pipeline: "build", Summary: "approved + tests", EndedAt: 100},
		{ID: "run-200", TaskID: "t1", Status: "done", Pipeline: "finish-branch", Summary: "branch finished", EndedAt: 200},
	}
	_, _, recentDone := partitionRuns(runs)
	if len(recentDone) != 1 {
		t.Fatalf("recentDone len=%d, want 1 (one row per task)", len(recentDone))
	}
	if recentDone[0].ID != "run-200" {
		t.Errorf("kept %q (pipeline=%q), want run-200 (finish-branch, max EndedAt)",
			recentDone[0].ID, recentDone[0].Pipeline)
	}
	if recentDone[0].Pipeline != "finish-branch" {
		t.Errorf("pipeline=%q want finish-branch", recentDone[0].Pipeline)
	}
}

func TestProjectsEnterDrillsCursorRunByVisualOrder(t *testing.T) {
	// Visual order: needs_attention → running → done.
	// With [run-1 done, run-2 running] in input, visual is [run-2, run-1].
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {
				{ID: "run-1", Status: "done"},
				{ID: "run-2", Status: "running"},
			},
		},
	}
	p := NewProjects(snap)
	// Cursor 0 → first visible = run-2 (running).
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	req := cmd().(DrillInRequest)
	if req.RunID != "run-2" {
		t.Errorf("cursor=0 Enter should drill run-2 (running, top of visual list); got %q", req.RunID)
	}
	// k (down) → cursor=1 → run-1 (done).
	p.Update(tea.KeyMsg{Runes: []rune{'k'}, Type: tea.KeyRunes})
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	req = cmd().(DrillInRequest)
	if req.RunID != "run-1" {
		t.Errorf("cursor=1 Enter should drill run-1 (done, second); got %q", req.RunID)
	}
}

func TestProjectsTabPKeyEmitsPlannerOpen(t *testing.T) {
	// P on the selected project should emit TabPlannerOpenRequest with
	// the selected slug. Root then resets the chat tab + fires the
	// planner-mode chat.send.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive"},
			{ID: "p2", Slug: "other", Name: "Other"},
		},
	}
	p := NewProjects(snap)
	// Move selection to the second project so we know the keybind uses
	// the cursor, not always index 0.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'P'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("P should emit a cmd")
	}
	req, ok := cmd().(TabPlannerOpenRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabPlannerOpenRequest", cmd())
	}
	if req.ProjectSlug != "other" {
		t.Errorf("ProjectSlug=%q want other", req.ProjectSlug)
	}
}

func TestProjectsTabEKeyEmitsEditProject(t *testing.T) {
	// e on the selected project should emit TabEditProjectRequest carrying
	// the slug + current name + current repo_path so root can pre-fill the
	// Edit Project modal.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive", RepoPath: "/tmp/hive"},
			// A MANUAL project (no sequence.status) with a configured target
			// branch — the edit request must carry it so the modal pre-fills
			// the real target, not the "main" default.
			{ID: "p2", Slug: "other", Name: "Other", RepoPath: "/tmp/other",
				DispatchMode: "manual", TargetBranch: "chat-test-harness"},
		},
	}
	p := NewProjects(snap)
	// Move selection to the second project so the test catches a hard-coded
	// index-0 implementation.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'e'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("e should emit a cmd")
	}
	req, ok := cmd().(TabEditProjectRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabEditProjectRequest", cmd())
	}
	if req.Slug != "other" {
		t.Errorf("Slug=%q want other", req.Slug)
	}
	if req.Name != "Other" {
		t.Errorf("Name=%q want Other", req.Name)
	}
	if req.RepoPath != "/tmp/other" {
		t.Errorf("RepoPath=%q want /tmp/other", req.RepoPath)
	}
	// Regression: a manual project's configured target branch must be seeded
	// into the edit request (was only seeded from the sequenced cache, which is
	// nil for manual projects → modal defaulted to "main").
	if req.Target != "chat-test-harness" {
		t.Errorf("Target=%q want chat-test-harness (manual project target must pre-fill)", req.Target)
	}
}

func TestProjectsTabDKeyEmitsDeleteRequest(t *testing.T) {
	// d on the selected project should emit TabDeleteProjectRequest
	// carrying the slug + cascading task/run counts so the confirm modal
	// can show the destructive scope.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive"},
			{ID: "p2", Slug: "other", Name: "Other"},
		},
		tasks: map[string][]TaskSummary{
			"p2": {
				{ID: "t1", Title: "a", Status: "pending"},
				{ID: "t2", Title: "b", Status: "needs_attention"},
				{ID: "t3", Title: "c", Status: "pending"},
			},
		},
		runs: map[string][]RunSummary{
			"p2": {
				{ID: "r1", Status: "running"},
				{ID: "r2", Status: "done"},
			},
		},
	}
	p := NewProjects(snap)
	// Move to the second project so the test catches a hard-coded
	// index-0 implementation.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'d'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("d should emit a cmd")
	}
	req, ok := cmd().(TabDeleteProjectRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabDeleteProjectRequest", cmd())
	}
	if req.Slug != "other" {
		t.Errorf("Slug=%q want other", req.Slug)
	}
	if req.TaskCount != 3 {
		t.Errorf("TaskCount=%d want 3", req.TaskCount)
	}
	if req.RunCount != 2 {
		t.Errorf("RunCount=%d want 2", req.RunCount)
	}
}

func TestProjectsTabRKeyEmitsRoadmapViewer(t *testing.T) {
	// R on the selected project should emit TabRoadmapViewerRequest
	// carrying slug + repo_path so root can construct the viewer
	// modal (which reads the on-disk roadmap synchronously).
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive", RepoPath: "/home/u/hive"},
			{ID: "p2", Slug: "other", Name: "Other", RepoPath: "/home/u/other"},
		},
	}
	p := NewProjects(snap)
	// Move selection to the second project so an index-0 hardcode is caught.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'R'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("R should emit a cmd")
	}
	req, ok := cmd().(TabRoadmapViewerRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabRoadmapViewerRequest", cmd())
	}
	if req.ProjectSlug != "other" {
		t.Errorf("ProjectSlug=%q want other", req.ProjectSlug)
	}
	if req.RepoPath != "/home/u/other" {
		t.Errorf("RepoPath=%q want /home/u/other", req.RepoPath)
	}
}

func TestProjectsTabSKeyEmitsSourcesRequest(t *testing.T) {
	// s on the selected project should emit TabSourcesRequest carrying
	// the slug so root can open the Sources modal + fire sources.list.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive"},
			{ID: "p2", Slug: "other", Name: "Other"},
		},
	}
	p := NewProjects(snap)
	// Move selection to the second project so the test catches a hard-coded
	// index-0 implementation.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'s'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("s should emit a cmd")
	}
	req, ok := cmd().(TabSourcesRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabSourcesRequest", cmd())
	}
	if req.ProjectSlug != "other" {
		t.Errorf("ProjectSlug=%q want other", req.ProjectSlug)
	}
}

func TestProjectsEnterOnTaskOpensDetail(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks: map[string][]TaskSummary{
			"p1": {{ID: "t1", Title: "pending one", Status: "pending"}},
		},
	}
	p := NewProjects(snap)
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on the only row (task) should emit cmd")
	}
	req, ok := cmd().(TabOpenModalRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabOpenModalRequest", cmd())
	}
	if req.Kind != "task_detail" {
		t.Errorf("Kind=%q want task_detail", req.Kind)
	}
	if req.InitialState["task_id"] != "t1" {
		t.Errorf("task_id=%v", req.InitialState["task_id"])
	}
}

func TestProjectsFocusSidebarOnArrowKey(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "a", Name: "A"},
			{ID: "p2", Slug: "b", Name: "B"},
		},
	}
	p := NewProjects(snap)
	// Default focus is "" (treated as sidebar); arrow keys should pin
	// it explicitly to "sidebar" so the next j/k flip is observable.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if p.focusedPane != "sidebar" {
		t.Errorf("after ↓ focusedPane=%q want sidebar", p.focusedPane)
	}
	// Flip to main, then arrow back — should snap back to sidebar.
	p.focusedPane = "main"
	p.Update(tea.KeyMsg{Type: tea.KeyUp})
	if p.focusedPane != "sidebar" {
		t.Errorf("after ↑ focusedPane=%q want sidebar", p.focusedPane)
	}
}

func TestProjectsFocusMainOnJ(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {
				{ID: "run-1", Status: "running"},
				{ID: "run-2", Status: "running"},
			},
		},
	}
	p := NewProjects(snap)
	p.Update(tea.KeyMsg{Runes: []rune{'j'}, Type: tea.KeyRunes})
	if p.focusedPane != "main" {
		t.Errorf("after j focusedPane=%q want main", p.focusedPane)
	}
	// k should also pin focus to main.
	p.focusedPane = "sidebar"
	p.Update(tea.KeyMsg{Runes: []rune{'k'}, Type: tea.KeyRunes})
	if p.focusedPane != "main" {
		t.Errorf("after k focusedPane=%q want main", p.focusedPane)
	}
}

func TestProjectsViewRendersBothFocusStates(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		runs: map[string][]RunSummary{
			"p1": {{ID: "r1", Status: "running"}},
		},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for _, focus := range []string{"", "sidebar", "main"} {
		p.focusedPane = focus
		view := p.View()
		if view == "" {
			t.Errorf("focusedPane=%q produced empty View()", focus)
		}
	}
}

func TestSequencedBadge(t *testing.T) {
	if got := sequencedBadge(nil); !strings.Contains(got, "seq") {
		t.Errorf("nil status badge=%q want contains 'seq'", got)
	}
	if got := sequencedBadge(&rpc.SeqStatusView{Status: "paused"}); !strings.Contains(got, "paused") {
		t.Errorf("paused badge=%q", got)
	}
	if got := sequencedBadge(&rpc.SeqStatusView{Complete: true}); !strings.Contains(got, "done") {
		t.Errorf("complete badge=%q want 'done'", got)
	}
	st := &rpc.SeqStatusView{
		Status:      "active",
		ActivePhase: "2",
		Phases: []rpc.SeqPhaseView{
			{Number: "1", Complete: true},
			{Number: "2", Tasks: []rpc.SeqTaskView{
				{GateState: "satisfied"}, {GateState: "satisfied"}, {GateState: "built"},
			}, Blocked: []rpc.SeqTaskView{{GateState: "built"}}},
		},
	}
	got := sequencedBadge(st)
	for _, want := range []string{"P2/2", "2✓", "1⚠"} {
		if !strings.Contains(got, want) {
			t.Errorf("active badge=%q missing %q", got, want)
		}
	}
}

func TestProjectsRendersSequencedBadge(t *testing.T) {
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "hive", Name: "Hive", DispatchMode: "sequenced"}},
		seq: map[string]*rpc.SeqStatusView{"p1": {
			Status: "active", ActivePhase: "1",
			Phases: []rpc.SeqPhaseView{{Number: "1", Tasks: []rpc.SeqTaskView{{GateState: "satisfied"}}}},
		}},
	}
	p := NewProjects(snap)
	p.width = 100
	view := p.View()
	if !strings.Contains(view, "P1/1") {
		t.Errorf("Projects view should show the sequenced badge; got:\n%s", view)
	}
}

func TestProjectsShowsIntegrationChipAndBranches(t *testing.T) {
	// A task with IntegrationState "pr_open" + PRNumber 7 should render "PR#7"
	// in the task row. A project with FeatureBranch/TargetBranch/AutoIntegrate
	// should render the branch line and auto badge in the header pane.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{
			ID:            "p1",
			Slug:          "myproj",
			Name:          "MyProj",
			FeatureBranch: "spec/x",
			TargetBranch:  "main",
			AutoIntegrate: true,
		}},
		tasks: map[string][]TaskSummary{
			"p1": {
				{ID: "t1", Title: "Do the thing", Status: "pending", IntegrationState: "pr_open", PRNumber: 7},
			},
		},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	view := p.View()

	// Task chip: PR#7 should appear somewhere in the view.
	if !strings.Contains(view, "PR#7") {
		t.Errorf("expected PR#7 chip in view; got:\n%s", view)
	}

	// Branch header: "spec/x → main" should appear.
	if !strings.Contains(view, "spec/x") {
		t.Errorf("expected feature branch 'spec/x' in view; got:\n%s", view)
	}
	if !strings.Contains(view, "main") {
		t.Errorf("expected target branch 'main' in view; got:\n%s", view)
	}

	// Auto-integrate badge: "auto" should appear.
	if !strings.Contains(view, "auto") {
		t.Errorf("expected auto-integrate badge in view; got:\n%s", view)
	}
}

func TestProjectsSKeyOpensSequenceOnlyForSequenced(t *testing.T) {
	// q on a sequenced project emits TabSequenceRequest; on a non-sequenced
	// project it's a no-op (nil cmd).
	snap := &fakeProjSnap{projects: []ProjectSummary{
		{ID: "p1", Slug: "seqp", Name: "Seqp", DispatchMode: "sequenced"},
		{ID: "p2", Slug: "plain", Name: "Plain", DispatchMode: "manual"},
	}}
	p := NewProjects(snap)
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")})
	if cmd == nil {
		t.Fatal("S on sequenced project should emit a cmd")
	}
	req, ok := cmd().(TabSequenceRequest)
	if !ok || req.Slug != "seqp" || req.ProjectID != "p1" {
		t.Errorf("got %T/%+v want TabSequenceRequest{seqp,p1}", cmd(), req)
	}
	// Move to the manual project: q is a no-op.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")})
	if cmd != nil {
		t.Errorf("S on non-sequenced project should be a no-op; got %T", cmd())
	}
}

func TestProjectsCKeyEmitsResolveRequest(t *testing.T) {
	// C on a needs_attention task emits TabResolveTaskRequest with the task ID.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": {{ID: "t1", Title: "stuck", Status: "needs_attention"}}},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	if cmd == nil {
		t.Fatal("C on a needs_attention task should emit a cmd")
	}
	req, ok := cmd().(TabResolveTaskRequest)
	if !ok || req.TaskID != "t1" {
		t.Fatalf("got %T / %v want TabResolveTaskRequest{t1}", cmd(), req)
	}
}

func TestProjectsCKeyNoopOnNonStuckTask(t *testing.T) {
	// C on a pending (non-needs_attention) task must be a no-op — the resolver
	// only makes sense for stuck tasks. Mirror the S-on-non-sequenced guard.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": {{ID: "t1", Title: "pending one", Status: "pending"}}},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(TabResolveTaskRequest); ok {
			t.Errorf("C on a pending task should not emit TabResolveTaskRequest; got %T", msg)
		}
	}
}

func TestProjectsTabGKeyEmitsGraduateRequestWhenFeatureBranchSet(t *testing.T) {
	// G on a project WITH a feature branch should emit TabGraduateRequest
	// carrying slug + feature + target (mirrors the H health-check gating).
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive"},
			{ID: "p2", Slug: "other", Name: "Other",
				FeatureBranch: "feature/x", TargetBranch: "staging"},
		},
	}
	p := NewProjects(snap)
	// Move to the second project so an index-0 hardcode is caught.
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'G'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("G should emit a cmd when a feature branch is set")
	}
	req, ok := cmd().(TabGraduateRequest)
	if !ok {
		t.Fatalf("got msg %T; want TabGraduateRequest", cmd())
	}
	if req.Slug != "other" {
		t.Errorf("Slug=%q want other", req.Slug)
	}
	if req.Feature != "feature/x" {
		t.Errorf("Feature=%q want feature/x", req.Feature)
	}
	if req.Target != "staging" {
		t.Errorf("Target=%q want staging", req.Target)
	}
}

func TestProjectsTabGKeyNoOpWithoutFeatureBranch(t *testing.T) {
	// G on a project WITHOUT a feature branch is a no-op (nothing to graduate),
	// mirroring the H health-check gating — no cmd is emitted.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{
			{ID: "p1", Slug: "hive", Name: "Hive"}, // no FeatureBranch
		},
	}
	p := NewProjects(snap)
	_, cmd := p.Update(tea.KeyMsg{Runes: []rune{'G'}, Type: tea.KeyRunes})
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Fatalf("G should be a no-op without a feature branch; got %T", msg)
		}
	}
}

func TestProjectsMKeyEmitsMergeRetryRequest(t *testing.T) {
	// M on a needs_attention task emits TabMergeRetryRequest with the task ID.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": {{ID: "t1", Title: "merge-failed task", Status: "needs_attention"}}},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if cmd == nil {
		t.Fatal("M on a needs_attention task should emit a cmd")
	}
	req, ok := cmd().(TabMergeRetryRequest)
	if !ok || req.TaskID != "t1" {
		t.Fatalf("got %T want TabMergeRetryRequest{TaskID:t1}", cmd())
	}
}

func TestProjectsMKeyNoopOnNonStuckTask(t *testing.T) {
	// M on a pending (non-needs_attention) task must be a no-op — merge.retry
	// only makes sense for tasks parked at merge_failed (which surfaces as
	// needs_attention in the snapshot). Mirror the C-key guard.
	snap := &fakeProjSnap{
		projects: []ProjectSummary{{ID: "p1", Slug: "a", Name: "A"}},
		tasks:    map[string][]TaskSummary{"p1": {{ID: "t1", Title: "pending one", Status: "pending"}}},
	}
	p := NewProjects(snap)
	p.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}})
	if cmd != nil {
		msg := cmd()
		if _, ok := msg.(TabMergeRetryRequest); ok {
			t.Errorf("M on a pending task should not emit TabMergeRetryRequest; got %T", msg)
		}
	}
}
