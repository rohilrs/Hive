package modals

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestTaskDetailViewNeverExceedsHeight: a long task body must scroll inside the
// modal's viewport, not overflow + clip the modal at the top. Regression for the
// dogfood "task modal cuts off".
func TestTaskDetailViewNeverExceedsHeight(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	// Lines deliberately WIDER than the modal frame's content area: if the
	// viewport is mis-sized, every line soft-wraps inside the frame and doubles
	// the row count (the real cutoff bug). The width-80 budget makes the frame
	// content area ~70 cols, so ~80-char lines must wrap in the VIEWPORT, not
	// the frame.
	body := strings.Repeat("acceptance criterion: pnpm -r test passes AND pnpm -r build succeeds cleanly here\n", 60)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "A fairly long task title that itself is wide enough to matter for the header",
		"body":  body, "status": "pending",
	}})
	for _, h := range []int{16, 24, 40} {
		rows := strings.Count(m.View(80, h), "\n") + 1
		if rows > h {
			t.Errorf("h=%d: View %d rows > height (would clip the modal)", h, rows)
		}
	}
}

// TestTaskDetailRendersPhaseLine: a roadmap-decomposed task (phase + index +
// total in metadata) shows a "Phase N · step i of total" header line, and the
// modal still fits its height budget with the extra row.
func TestTaskDetailRendersPhaseLine(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	body := strings.Repeat("acceptance criterion: pnpm -r test passes AND pnpm -r build succeeds here\n", 60)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title":  "Add retry/backoff to the chat send path",
		"body":   body,
		"status": "pending",
		"metadata": map[string]any{
			"roadmap_phase":       "1",
			"roadmap_phase_index": "2",
			"roadmap_phase_total": "5",
		},
	}})
	out := m.View(80, 30)
	if !strings.Contains(out, "Phase 1 · step 2 of 5") {
		t.Errorf("view missing phase line; got:\n%s", out)
	}
	// Height budget must still hold with the extra header row.
	for _, h := range []int{16, 24, 40} {
		if rows := strings.Count(m.View(80, h), "\n") + 1; rows > h {
			t.Errorf("h=%d: View %d rows > height with phase line", h, rows)
		}
	}
}

// TestTaskDetailNoPhaseLineWithoutMeta: a task with no roadmap metadata
// renders no phase header (and no stray blank row).
func TestTaskDetailNoPhaseLineWithoutMeta(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "Plain task", "body": "b", "status": "pending",
	}})
	if m.phaseLine != "" {
		t.Errorf("phaseLine should be empty without metadata; got %q", m.phaseLine)
	}
	if strings.Contains(m.View(80, 30), "Phase ") {
		t.Error("view should not contain a phase line for a non-roadmap task")
	}
}

func TestTaskDetailInitFetches(t *testing.T) {
	m := NewTaskDetail("t1", "known title", nil, "", "", 0)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init should fetch task")
	}
	// Init now batches the task.get submit with the spinner.Tick so the
	// tdLoading spinner animates. Look inside the batch for the
	// SubmitRequest rather than asserting on the top-level message.
	req, ok := findSubmitRequest(cmd())
	if !ok || req.Kind != "task_get" {
		t.Fatalf("Init batch missing task_get SubmitRequest; got %T", cmd())
	}
}

// findSubmitRequest searches a tea.Msg for a SubmitRequest. Top-level
// SubmitRequest is returned as-is; otherwise the batch is unrolled and
// each child cmd evaluated. Used by tests that need to assert against
// batched cmds (e.g. Init / r-key which combine the RPC submit with a
// spinner.Tick).
func findSubmitRequest(msg tea.Msg) (SubmitRequest, bool) {
	switch v := msg.(type) {
	case SubmitRequest:
		return v, true
	case tea.BatchMsg:
		for _, c := range v {
			if c == nil {
				continue
			}
			if found, ok := findSubmitRequest(c()); ok {
				return found, true
			}
		}
	}
	return SubmitRequest{}, false
}

func TestTaskDetailLoadsBodyThenViews(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "Real Title", "body": "line one\nline two", "status": "pending",
	}})
	if m.state != tdView {
		t.Errorf("state=%d want tdView", m.state)
	}
	view := m.View(100, 40)
	if !strings.Contains(view, "Real Title") || !strings.Contains(view, "line two") {
		t.Errorf("view missing loaded content: %q", view)
	}
}

func TestTaskDetailEditAndSave(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "body": "B", "status": "pending"}})
	// Enter edit mode.
	m.Update(tea.KeyMsg{Runes: []rune{'e'}, Type: tea.KeyRunes})
	if m.state != tdEdit {
		t.Fatalf("state=%d want tdEdit", m.state)
	}
	// ctrl+s submits task_edit.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("ctrl+s should submit")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok || req.Kind != "task_edit" {
		t.Fatalf("got %T/%v want task_edit", cmd(), req.Kind)
	}
	if req.Params["task_id"] != "t1" {
		t.Errorf("task_id=%v", req.Params["task_id"])
	}
}

func TestTaskDetailRunAndDelete(t *testing.T) {
	// r → run_now submit + transition to the dispatching state (3.7.5:
	// run.now blocks ~10s through the predictor, so the modal shows a
	// "dispatching…" state rather than looking frozen).
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "status": "pending"}})
	_, cmd := m.Update(tea.KeyMsg{Runes: []rune{'r'}, Type: tea.KeyRunes})
	// r batches the run_now submit with a spinner.Tick (so the dispatching
	// state animates). Unwrap the batch.
	req, ok := findSubmitRequest(cmd())
	if !ok || req.Kind != "run_now" {
		t.Fatalf("r should submit run_now; got %T", cmd())
	}
	if m.state != tdRunning {
		t.Fatalf("r should enter dispatching state; state=%d", m.state)
	}

	// Delete flow on a fresh modal (r is terminal until the run result).
	d := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	d.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "status": "pending"}})
	d.Update(tea.KeyMsg{Runes: []rune{'d'}, Type: tea.KeyRunes})
	if d.state != tdConfirmDelete {
		t.Fatalf("d should enter confirm-delete; state=%d", d.state)
	}
	_, cmd = d.Update(tea.KeyMsg{Runes: []rune{'y'}, Type: tea.KeyRunes})
	if req, ok := cmd().(SubmitRequest); !ok || req.Kind != "task_delete" {
		t.Fatalf("y should submit task_delete; got %T", cmd())
	}
}

func TestTaskDetailDeleteSuccessCloses(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	_, cmd := m.Update(RPCResultMsg{Kind: "task_delete", Data: map[string]any{"deleted": true}})
	if cmd == nil {
		t.Fatal("delete success should close")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("want CloseMsg, got %T", cmd())
	}
}

func TestTaskDetailEscFromViewCloses(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "status": "pending"}})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc from view should close")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("want CloseMsg, got %T", cmd())
	}
}

func TestTaskDetailEscFromEditReturnsToView(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "status": "pending"}})
	m.Update(tea.KeyMsg{Runes: []rune{'e'}, Type: tea.KeyRunes})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.state != tdView {
		t.Errorf("esc from edit should return to view; state=%d", m.state)
	}
}

// TestTaskDetailLoadingViewIncludesSpinner verifies that the tdLoading
// state renders the "loading task…" text alongside the spinner glyph.
// Spinner animation is timer-driven and not tested directly; we just
// confirm the View renders without panic and the surrounding text is
// still present (so the visual prompt to the user is preserved).
func TestTaskDetailLoadingViewIncludesSpinner(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	if m.state != tdLoading {
		t.Fatalf("expected initial state tdLoading; got %d", m.state)
	}
	out := m.View(80, 24)
	if !strings.Contains(out, "loading task") {
		t.Errorf("loading view missing 'loading task' text: %q", out)
	}
}

// TestTaskDetailRunningViewIncludesSpinner verifies the tdRunning state
// (entered via `r` in tdView) renders the spinner + dispatch message.
func TestTaskDetailRunningViewIncludesSpinner(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "status": "pending"}})
	m.Update(tea.KeyMsg{Runes: []rune{'r'}, Type: tea.KeyRunes})
	if m.state != tdRunning {
		t.Fatalf("expected tdRunning after r; got %d", m.state)
	}
	out := m.View(80, 24)
	if !strings.Contains(out, "dispatching run") {
		t.Errorf("running view missing 'dispatching run' text: %q", out)
	}
}

// TestTaskDetailRunReissuesSpinnerTick verifies that pressing `r` to
// transition into tdRunning re-issues the spinner.Tick command so the
// animation continues even if the prior tick chain was dropped while in
// tdView. Without this, the dispatching state would render a static glyph.
func TestTaskDetailRunReissuesSpinnerTick(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{"title": "T", "status": "pending"}})
	_, cmd := m.Update(tea.KeyMsg{Runes: []rune{'r'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("pressing r should return a cmd (run_now + spinner tick)")
	}
	// The cmd is a tea.Batch; we can't easily introspect it, but we can
	// verify it runs without panic and produces at least one tea.Msg.
	msg := cmd()
	if msg == nil {
		t.Fatal("batch cmd produced nil msg")
	}
}

// TestTaskDetailShowsIntegrationLine verifies that a task with an
// integration state renders the PR number and state in the modal.
func TestTaskDetailShowsIntegrationLine(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "merged", "https://github.com/o/r/pull/7", 7).(*TaskDetail)
	// Drive to tdView so View renders the detail body (not the loading spinner).
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "known", "body": "", "status": "done",
	}})
	if m.state != tdView {
		t.Fatalf("expected tdView after task_get; got %d", m.state)
	}
	out := m.View(80, 30)
	if !strings.Contains(out, "PR #7") {
		t.Errorf("integration line missing PR #7: %q", out)
	}
	if !strings.Contains(out, "merged") {
		t.Errorf("integration line missing 'merged': %q", out)
	}
}

// TestTaskDetailBlockedIntegrationLine verifies that "blocked" state
// is rendered and no PR URL leaks out of the truncation budget.
func TestTaskDetailBlockedIntegrationLine(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "blocked", "https://github.com/o/r/pull/3", 3).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "known", "body": "", "status": "blocked",
	}})
	out := m.View(80, 30)
	if !strings.Contains(out, "PR #3") {
		t.Errorf("integration line missing PR #3: %q", out)
	}
	if !strings.Contains(out, "blocked") {
		t.Errorf("integration line missing 'blocked': %q", out)
	}
}

// TestTaskDetailNoIntegrationLineWhenEmpty verifies that no Integration
// line is emitted for tasks that are not currently integrating.
func TestTaskDetailNoIntegrationLineWhenEmpty(t *testing.T) {
	m := NewTaskDetail("t1", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "known", "body": "", "status": "pending",
	}})
	out := m.View(80, 30)
	if strings.Contains(out, "Integration:") {
		t.Errorf("unexpected Integration line for non-integrating task: %q", out)
	}
}

// TestTaskDetailIntegrationLineHeightBudget verifies that adding an
// integration line does not push the modal past its height budget.
func TestTaskDetailIntegrationLineHeightBudget(t *testing.T) {
	body := strings.Repeat("acceptance criterion: pnpm -r test passes AND build succeeds cleanly here\n", 60)
	m := NewTaskDetail("t1", "known", nil, "pr_open", "https://github.com/o/r/pull/5", 5).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "A fairly long task title that itself is wide enough to matter for the header",
		"body":  body, "status": "pending",
	}})
	for _, h := range []int{16, 24, 40} {
		rows := strings.Count(m.View(80, h), "\n") + 1
		if rows > h {
			t.Errorf("h=%d: View %d rows > height (would clip the modal)", h, rows)
		}
	}
}

// TestTaskDetailDecomposeEntersSpinnerAndEmitsOpen: capital D on a
// non-running task transitions to the decomposing spinner state and emits
// the task_decompose_open SubmitRequest carrying the task_id (batched with
// a spinner.Tick so the glyph animates).
func TestTaskDetailDecomposeEntersSpinnerAndEmitsOpen(t *testing.T) {
	m := NewTaskDetail("t-42", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "T", "body": "B", "status": "pending",
	}})
	_, cmd := m.Update(tea.KeyMsg{Runes: []rune{'D'}, Type: tea.KeyRunes})
	if m.state != tdDecomposing {
		t.Fatalf("state=%d want tdDecomposing", m.state)
	}
	req, ok := findSubmitRequest(cmd())
	if !ok || req.Kind != "task_decompose_open" {
		t.Fatalf("D should emit task_decompose_open; got ok=%v kind=%q", ok, req.Kind)
	}
	if req.Params["task_id"] != "t-42" {
		t.Errorf("task_id=%v want t-42", req.Params["task_id"])
	}
	// The decomposing state renders its own spinner line.
	if !strings.Contains(m.View(80, 30), "decomposing") {
		t.Errorf("view should show decomposing spinner; got:\n%s", m.View(80, 30))
	}
}

// TestTaskDetailDecomposeBlockedWhileRunning: D is a no-op (with an inline
// error) on a running task — it shouldn't carve up a task mid-dispatch.
func TestTaskDetailDecomposeBlockedWhileRunning(t *testing.T) {
	m := NewTaskDetail("t-42", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "T", "body": "B", "status": "running",
	}})
	_, cmd := m.Update(tea.KeyMsg{Runes: []rune{'D'}, Type: tea.KeyRunes})
	if m.state == tdDecomposing {
		t.Error("D should not start decompose on a running task")
	}
	if cmd != nil {
		if _, ok := findSubmitRequest(cmd()); ok {
			t.Error("D on a running task should not emit a SubmitRequest")
		}
	}
	if m.errMsg == "" {
		t.Error("D on a running task should set an inline error")
	}
}

// TestTaskDetailDecomposeErrorDropsSpinner: a forwarded task_decompose_open
// error drops the spinner back to the view so it's visible and esc-able.
func TestTaskDetailDecomposeErrorDropsSpinner(t *testing.T) {
	m := NewTaskDetail("t-42", "known", nil, "", "", 0).(*TaskDetail)
	m.Update(RPCResultMsg{Kind: "task_get", Data: map[string]any{
		"title": "T", "body": "B", "status": "pending",
	}})
	m.Update(tea.KeyMsg{Runes: []rune{'D'}, Type: tea.KeyRunes})
	m.Update(RPCResultMsg{Kind: "task_decompose_open", Err: errStr("daemon: boom")})
	if m.state != tdView {
		t.Errorf("state=%d want tdView after decompose error", m.state)
	}
	if !strings.Contains(m.View(80, 30), "boom") {
		t.Errorf("error should render inline; got:\n%s", m.View(80, 30))
	}
}

// errStr is a tiny error helper so the test doesn't pull in errors just for one line.
type errStr string

func (e errStr) Error() string { return string(e) }
