package modals

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRoadmapViewerDecomposeErrorShownInline: a failed roadmap.decompose RPC
// surfaces inline in the viewer (and clears the decomposing spinner) instead of
// the root silently closing the modal — the deferred "errMsg plumbing" fix.
func TestRoadmapViewerDecomposeErrorShownInline(t *testing.T) {
	repo := writeFakeRoadmap(t, "big", bigRoadmap())
	m := NewRoadmapViewerModal("big", repo)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}}) // decomposing=true
	m.Update(RPCResultMsg{Kind: "roadmap_decompose_open", Err: errors.New("rpc timeout after 120s")})
	if m.decomposing {
		t.Error("decomposing spinner should clear when the (error) result arrives")
	}
	out := m.View(120, 40)
	if !strings.Contains(out, "rpc timeout after 120s") {
		t.Errorf("view should surface the decompose error inline; got:\n%s", out)
	}
	if !strings.Contains(out, "Phase") {
		t.Errorf("phase list must stay visible after a decompose error (not a fatal early-return); got:\n%s", out)
	}
}

// TestRoadmapViewerDecomposeEmptyShownInline: an empty proposal set (RPC ok but
// no subtasks) also surfaces a message rather than silently closing.
func TestRoadmapViewerDecomposeEmptyShownInline(t *testing.T) {
	repo := writeFakeRoadmap(t, "big", bigRoadmap())
	m := NewRoadmapViewerModal("big", repo)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	m.Update(RPCResultMsg{Kind: "roadmap_decompose_open", Err: nil, Data: map[string]any{}})
	if m.decomposing {
		t.Error("decomposing spinner should clear on an empty result")
	}
	if out := m.View(120, 40); !strings.Contains(out, "no proposals") {
		t.Errorf("empty decompose should surface a 'no proposals' message; got:\n%s", out)
	}
}

// TestRoadmapViewerDClearsPriorDecomposeError: re-pressing D after an error
// clears it and starts a fresh attempt.
func TestRoadmapViewerDClearsPriorDecomposeError(t *testing.T) {
	repo := writeFakeRoadmap(t, "big", bigRoadmap())
	m := NewRoadmapViewerModal("big", repo)
	m.Update(RPCResultMsg{Kind: "roadmap_decompose_open", Err: errors.New("boom")})
	if !strings.Contains(m.View(120, 40), "boom") {
		t.Fatal("precondition: error should be shown")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}}) // retry
	if out := m.View(120, 40); strings.Contains(out, "boom") {
		t.Errorf("re-pressing D should clear the prior error; got:\n%s", out)
	}
	if !m.decomposing {
		t.Error("D should start a fresh decompose (decomposing=true)")
	}
}

// writeFakeRoadmap writes a roadmap markdown file at the canonical path
// `<repoDir>/docs/superpowers/roadmaps/<slug>.md`. Returns the repo path.
func writeFakeRoadmap(t *testing.T, slug, body string) string {
	t.Helper()
	dir := t.TempDir()
	roadDir := filepath.Join(dir, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(roadDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(roadDir, slug+".md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write roadmap: %v", err)
	}
	return dir
}

func TestRoadmapViewerLoadsAndParsesFromDisk(t *testing.T) {
	body := `# Roadmap for hive

## Phase 1: bootstrap the project
goal: stand up the daemon

- task one
- task two

## Phase 2: ship the TUI
goal: render the snapshot

Spec: [overview](./docs/superpowers/specs/overview.md)
`
	repo := writeFakeRoadmap(t, "hive", body)
	m := NewRoadmapViewerModal("hive", repo)
	if m.err != "" {
		t.Fatalf("err=%q want empty", m.err)
	}
	if m.roadmap == nil {
		t.Fatal("roadmap nil after successful parse")
	}
	if len(m.roadmap.Phases) != 2 {
		t.Fatalf("phases=%d want 2", len(m.roadmap.Phases))
	}
	if m.roadmap.Phases[0].Number != "1" || m.roadmap.Phases[1].Number != "2" {
		t.Errorf("phase numbers = %q,%q want 1,2",
			m.roadmap.Phases[0].Number, m.roadmap.Phases[1].Number)
	}
	// Title in modal header contains the slug.
	if !strings.Contains(m.Title(), "hive") {
		t.Errorf("title %q missing slug", m.Title())
	}
}

func TestRoadmapViewerHandlesMissingFile(t *testing.T) {
	repo := t.TempDir()
	m := NewRoadmapViewerModal("nonexistent", repo)
	if m.roadmap != nil {
		t.Errorf("roadmap should be nil for missing file")
	}
	// A working-tree miss no longer errors immediately — it defers to the
	// daemon's branch-aware fetch (NeedsContent) and shows a loading line.
	if !m.NeedsContent() {
		t.Errorf("a working-tree miss should request a branch-aware fetch")
	}
	if m.err != "" {
		t.Errorf("err should be empty while loading; got %q", m.err)
	}
	if view := m.View(80, 24); !strings.Contains(view, "loading") || !strings.Contains(view, "esc") {
		t.Errorf("loading view missing 'loading'/'esc'; got:\n%s", view)
	}

	// When the fetch fails, the error renders inline and esc still works.
	m.SetError("no roadmap for nonexistent")
	if m.NeedsContent() {
		t.Errorf("NeedsContent should be false after SetError")
	}
	view := m.View(80, 24)
	if !strings.Contains(view, "no roadmap") || !strings.Contains(view, "esc") {
		t.Errorf("error view missing inline error/esc; got:\n%s", view)
	}
}

func TestRoadmapViewerSetContentParses(t *testing.T) {
	// A working-tree miss followed by a successful branch-aware fetch must parse.
	m := NewRoadmapViewerModal("p", t.TempDir())
	if !m.NeedsContent() {
		t.Fatal("precondition: should need content")
	}
	m.SetContent("# r\n\n## Phase 1: First\n\nbody\n")
	if m.NeedsContent() || m.err != "" {
		t.Fatalf("after SetContent: loading=%v err=%q", m.NeedsContent(), m.err)
	}
	if m.roadmap == nil || len(m.roadmap.Phases) != 1 {
		t.Fatalf("SetContent should parse one phase; got %+v", m.roadmap)
	}
}

func TestRoadmapViewerHandlesParseError(t *testing.T) {
	// roadmap.Parse rejects a doc with no `## Phase N: ...` headings.
	repo := writeFakeRoadmap(t, "broken", "this file has no phase headings whatsoever\n")
	m := NewRoadmapViewerModal("broken", repo)
	if m.roadmap != nil {
		t.Errorf("roadmap should be nil after parse error")
	}
	if !strings.Contains(m.err, "parse") {
		t.Errorf("err=%q want to contain 'parse'", m.err)
	}
}

func TestRoadmapViewerCursorNavigation(t *testing.T) {
	body := `## Phase 1: first
body one

## Phase 2: second
body two

## Phase 3: third
body three
`
	repo := writeFakeRoadmap(t, "hive", body)
	m := NewRoadmapViewerModal("hive", repo)
	if m.phaseCursor != 0 {
		t.Fatalf("initial cursor=%d want 0", m.phaseCursor)
	}

	// j moves down (one step).
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.phaseCursor != 1 {
		t.Errorf("after j, cursor=%d want 1", m.phaseCursor)
	}

	// down arrow also moves down.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.phaseCursor != 2 {
		t.Errorf("after down, cursor=%d want 2", m.phaseCursor)
	}

	// At bottom — further down is clamped.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.phaseCursor != 2 {
		t.Errorf("cursor should clamp at 2; got %d", m.phaseCursor)
	}

	// k moves up.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.phaseCursor != 1 {
		t.Errorf("after k, cursor=%d want 1", m.phaseCursor)
	}

	// up arrow also moves up; clamp at 0.
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.phaseCursor != 0 {
		t.Errorf("after up, cursor=%d want 0", m.phaseCursor)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.phaseCursor != 0 {
		t.Errorf("cursor should clamp at 0; got %d", m.phaseCursor)
	}
}

func TestRoadmapViewerCapitalDEmitsDecomposeOpen(t *testing.T) {
	body := `## Phase 1: alpha
body

## Phase 2: beta
body
Spec: [s](./docs/superpowers/specs/x.md)
`
	repo := writeFakeRoadmap(t, "hive", body)
	m := NewRoadmapViewerModal("hive", repo)

	// Move cursor to phase 2 so the test catches a hard-coded index-0 bug.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.phaseCursor != 1 {
		t.Fatalf("setup: cursor=%d want 1", m.phaseCursor)
	}

	// Capital D — the decompose trigger.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	if cmd == nil {
		t.Fatal("D should emit a cmd")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("got msg %T want SubmitRequest", cmd())
	}
	if req.Kind != "roadmap_decompose_open" {
		t.Errorf("Kind=%q want roadmap_decompose_open", req.Kind)
	}
	if req.Params["project_slug"] != "hive" {
		t.Errorf("project_slug=%v want hive", req.Params["project_slug"])
	}
	if req.Params["phase"] != "2" {
		t.Errorf("phase=%v want 2 (cursor moved to second phase)", req.Params["phase"])
	}
	rp, _ := req.Params["roadmap_path"].(string)
	if !strings.HasSuffix(rp, filepath.Join("docs", "superpowers", "roadmaps", "hive.md")) {
		t.Errorf("roadmap_path=%q missing canonical suffix", rp)
	}
	if req.Params["phase_title"] != "beta" {
		t.Errorf("phase_title=%v want beta", req.Params["phase_title"])
	}
	specPaths, _ := req.Params["spec_paths"].([]string)
	if len(specPaths) == 0 {
		t.Errorf("spec_paths empty; want at least one")
	}
}

func TestRoadmapViewerCapitalLEmitsSyncLinear(t *testing.T) {
	body := "## Phase 1: alpha\nbody\n"
	repo := writeFakeRoadmap(t, "hive", body)
	m := NewRoadmapViewerModal("hive", repo)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	if cmd == nil {
		t.Fatal("L should emit a roadmap_sync_linear request")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok || req.Kind != "roadmap_sync_linear" {
		t.Fatalf("got %T/%v want SubmitRequest/roadmap_sync_linear", cmd(), req.Kind)
	}
	if req.Params["project_slug"] != "hive" {
		t.Errorf("project_slug=%v want hive", req.Params["project_slug"])
	}
	if !m.syncing {
		t.Error("syncing should flip true after L")
	}
	// A successful result reports the milestone count and clears the spinner.
	m.Update(RPCResultMsg{Kind: "roadmap_sync_linear", Data: map[string]any{"milestones": float64(3)}})
	if m.syncing {
		t.Error("syncing should reset on result")
	}
	if !strings.Contains(m.syncMsg, "3 milestone") {
		t.Errorf("syncMsg=%q want it to report 3 milestones", m.syncMsg)
	}
}

func TestRoadmapViewerSyncLinearZeroMilestonesHint(t *testing.T) {
	repo := writeFakeRoadmap(t, "hive", "## Phase 1: alpha\nbody\n")
	m := NewRoadmapViewerModal("hive", repo)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	// 0 milestones → the daemon no-op'd; hint at the likely cause (no write-back).
	m.Update(RPCResultMsg{Kind: "roadmap_sync_linear", Data: map[string]any{"milestones": float64(0)}})
	if !strings.Contains(m.syncMsg, "write-back") {
		t.Errorf("syncMsg=%q want it to mention write-back for a 0-milestone sync", m.syncMsg)
	}
}

func TestRoadmapViewerCapitalLNoOpWhenLoadFailed(t *testing.T) {
	repo := t.TempDir() // missing file → roadmap nil
	m := NewRoadmapViewerModal("nope", repo)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	if cmd != nil {
		t.Errorf("L on load-failed modal should be a no-op; got cmd %v", cmd())
	}
}

func TestRoadmapViewerCapitalDNoOpWhenLoadFailed(t *testing.T) {
	// When parse failed (no phases), D must not panic / emit anything.
	repo := t.TempDir() // missing file
	m := NewRoadmapViewerModal("nope", repo)
	if m.roadmap != nil {
		t.Fatalf("setup: roadmap should be nil")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	if cmd != nil {
		t.Errorf("D on load-failed modal should be a no-op; got cmd %v", cmd())
	}
}

func TestRoadmapViewerEscEmitsClose(t *testing.T) {
	body := `## Phase 1: a
body
`
	repo := writeFakeRoadmap(t, "hive", body)
	m := NewRoadmapViewerModal("hive", repo)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc emitted %T want CloseMsg", cmd())
	}
}

// bigRoadmap builds a roadmap with many phases + long bodies to exercise
// scrolling in both panes.
func bigRoadmap() string {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, "## Phase %d — phase %d with a deliberately long title that should truncate cleanly in the narrow left pane\n", i, i)
		for j := 0; j < 30; j++ {
			fmt.Fprintf(&b, "body line %d of phase %d, long enough that the right pane needs to scroll\n", j, i)
		}
		b.WriteString("\nSpec: [docs/superpowers/specs/p.md](docs/superpowers/specs/p.md)\n\n")
	}
	return b.String()
}

// TestRoadmapViewerTwoPaneNeverExceedsHeight: the two-pane View must stay within
// its height budget at every cursor/focus/scroll position, so it can't overflow
// and clip the modal/tab bar. Regression for the dogfood "modal gets cut off".
func TestRoadmapViewerTwoPaneNeverExceedsHeight(t *testing.T) {
	repo := writeFakeRoadmap(t, "big", bigRoadmap())
	m := NewRoadmapViewerModal("big", repo)
	if m.err != "" {
		t.Fatalf("parse err: %s", m.err)
	}
	for _, wd := range []int{80, 120, 200} { // narrow, wide, ultrawide (capped at 140)
		for _, h := range []int{16, 24, 40} {
			for _, cur := range []int{0, 4, 7} {
				m.phaseCursor = cur
				for _, focusRight := range []bool{false, true} {
					m.focusRight = focusRight
					for _, sc := range []int{0, 8, 100} {
						m.detailScroll = sc
						rows := strings.Count(m.View(wd, h), "\n") + 1
						if rows > h {
							t.Errorf("w=%d h=%d cur=%d focusRight=%v scroll=%d: View %d rows > height", wd, h, cur, focusRight, sc, rows)
						}
					}
				}
			}
		}
	}
}

// TestRoadmapViewerTabFocusAndDetailScroll: Tab switches which pane ↑/↓ act on,
// the right pane scrolls the full body, and changing phase resets the scroll.
func TestRoadmapViewerTabFocusAndDetailScroll(t *testing.T) {
	repo := writeFakeRoadmap(t, "big", bigRoadmap())
	m := NewRoadmapViewerModal("big", repo)

	// Default focus = left: down moves the phase, not the body.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.phaseCursor != 1 || m.focusRight {
		t.Fatalf("left focus down should move phase; cursor=%d focusRight=%v", m.phaseCursor, m.focusRight)
	}

	// Tab focuses the right pane; down now scrolls the body, not the phase.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.focusRight {
		t.Fatal("tab should focus the right pane")
	}
	cur := m.phaseCursor
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.detailScroll != 1 || m.phaseCursor != cur {
		t.Errorf("right focus down should scroll body; detailScroll=%d cursor=%d (was %d)", m.detailScroll, m.phaseCursor, cur)
	}

	// Tab back to the left + move phase → detail scroll resets.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.detailScroll != 0 {
		t.Errorf("changing phase should reset detailScroll; got %d", m.detailScroll)
	}

	// The right pane shows the FULL body (no 8-line preview cap).
	if got := m.detailLineCount(); got < 30 {
		t.Errorf("detail line count = %d, want ≥30 (full body, not capped)", got)
	}
}

// TestRoadmapViewerDShowsDecomposing: pressing D flags the (multi-second)
// decompose RPC so the footer shows progress instead of looking dead.
func TestRoadmapViewerDShowsDecomposing(t *testing.T) {
	repo := writeFakeRoadmap(t, "big", bigRoadmap())
	m := NewRoadmapViewerModal("big", repo)
	if m.decomposing {
		t.Fatal("should not start in decomposing state")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	if !m.decomposing {
		t.Error("D should set decomposing=true")
	}
	if cmd == nil {
		t.Error("D should emit a decompose SubmitRequest cmd")
	}
	if !strings.Contains(m.View(100, 30), "decomposing") {
		t.Errorf("View footer should show the decomposing indicator")
	}
}

// TestFramedModalBodyKeepsBottomBorder is the regression for the modal scroll
// bug: the body inserts a blank line after the title AND before the footer, but
// the budget counted only one — so the body was 1 row too tall and MaxHeight
// clipped the bottom border once the field window filled on scroll. The
// rendered modal's last row must still carry the rounded bottom border.
func TestFramedModalBodyKeepsBottomBorder(t *testing.T) {
	fields := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		fields = append(fields, fmt.Sprintf("Field %d:", i), fmt.Sprintf("  value-%d", i))
	}
	footer := "an inline error message that wraps across the modal width\n\nctrl+s submit · tab next · esc cancel"
	title := "Edit project some-slug"

	for _, height := range []int{18, 22, 26, 30} {
		for _, focus := range []int{0, 10, 39} { // top, middle, bottom of the ring
			out := framedModalBody(title, fields, focus, footer, 60, height)
			lines := strings.Split(out, "\n")
			last := lines[len(lines)-1]
			// RoundedBorder bottom corners are ╰ … ╯ — both must be present.
			if !strings.Contains(last, "╰") || !strings.Contains(last, "╯") {
				t.Errorf("height=%d focus=%d: bottom border clipped (last row=%q):\n%s",
					height, focus, last, out)
			}
		}
	}
}

// TestFramedModalBodyScrollFollowsAndConstantHeight is the regression for the
// project-modal scroll bugs: (1) the focused field's VALUE line must stay on
// screen as you tab down (auto-scroll follows the cursor, not just the label),
// and (2) the modal height must stay CONSTANT while scrolling (no grow as the
// scroll hints appear).
func TestFramedModalBodyScrollFollowsAndConstantHeight(t *testing.T) {
	// 12 fields, each a 2-line block (label + value) — forces windowing at a
	// constrained height so scrolling is in play.
	var fields []formField
	for i := 0; i < 12; i++ {
		fields = append(fields, formField{
			slot:  i,
			label: fmt.Sprintf("Field %d:", i),
			value: fmt.Sprintf("VAL%d", i),
		})
	}
	footer := "ctrl+s submit · tab next · esc cancel"
	title := "Edit project x"
	const height = 18

	var firstHeight int
	for focus := 0; focus < 12; focus++ {
		lines, focusedLine := buildFieldLines(fields, focus)
		out := framedModalBody(title, lines, focusedLine, footer, 60, height)

		// (1) the focused field's VALUE must be visible (auto-scroll followed).
		if !strings.Contains(out, fmt.Sprintf("VAL%d", focus)) {
			t.Errorf("focus=%d: focused field's value VAL%d scrolled off-screen:\n%s", focus, focus, out)
		}
		// (2) height constant across all focus positions.
		h := strings.Count(out, "\n") + 1
		if focus == 0 {
			firstHeight = h
		} else if h != firstHeight {
			t.Errorf("focus=%d: modal height %d != %d (box grows/shrinks on scroll)", focus, h, firstHeight)
		}
		// bottom border intact.
		last := out[strings.LastIndex(out, "\n")+1:]
		if !strings.Contains(last, "╰") {
			t.Errorf("focus=%d: bottom border clipped (last row=%q)", focus, last)
		}
	}
}
