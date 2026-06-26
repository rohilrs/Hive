package modals

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/pkg/rpc"
)

func seqFixture() *rpc.SeqStatusView {
	return &rpc.SeqStatusView{
		Slug: "hive", Status: "active", Policy: "human_merge", Target: "staging",
		ActivePhase: "2",
		Phases: []rpc.SeqPhaseView{
			{Number: "1", Title: "One", Complete: true},
			{Number: "2", Title: "Engine", Tasks: []rpc.SeqTaskView{
				{ID: "t1", Title: "build core", Status: "done", GateState: "satisfied"},
				{ID: "t2", Title: "finish branch", Status: "needs_attention", GateState: "built"},
			}, Blocked: []rpc.SeqTaskView{{ID: "t2"}}},
		},
	}
}

func TestSequenceModalView(t *testing.T) {
	m := NewSequenceModal("hive", "p1", seqFixture())
	view := m.View(80, 24)
	for _, want := range []string{"Sequence — hive", "human_merge", "→staging", "Phase 2: Engine", "build core", "finish branch", "pause", "skip"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q; got:\n%s", want, view)
		}
	}
}

func TestSequenceModalActionsEmitRequests(t *testing.T) {
	cases := []struct {
		key, kind string
	}{
		{"p", "sequence_pause"},
		{"r", "sequence_resume"},
		{"a", "sequence_advance"},
	}
	for _, c := range cases {
		m := NewSequenceModal("hive", "p1", seqFixture())
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(c.key)})
		if cmd == nil {
			t.Fatalf("%s: no cmd", c.key)
		}
		req, ok := cmd().(SubmitRequest)
		if !ok || req.Kind != c.kind {
			t.Errorf("%s: got %T/%v want SubmitRequest/%s", c.key, cmd(), req.Kind, c.kind)
		}
		if req.Params["project_slug"] != "hive" {
			t.Errorf("%s: project_slug=%v want hive", c.key, req.Params["project_slug"])
		}
	}
}

func TestSequenceModalCompleteEmitsActivePhase(t *testing.T) {
	m := NewSequenceModal("hive", "p1", seqFixture())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if cmd == nil {
		t.Fatal("c should emit a sequence_complete request")
	}
	req, ok := cmd().(SubmitRequest)
	if !ok || req.Kind != "sequence_complete" {
		t.Fatalf("got %T/%v want SubmitRequest/sequence_complete", cmd(), req.Kind)
	}
	if req.Params["project_slug"] != "hive" {
		t.Errorf("project_slug=%v want hive", req.Params["project_slug"])
	}
	if req.Params["phase"] != "2" {
		t.Errorf("phase=%v want 2 (the active phase)", req.Params["phase"])
	}
}

func TestSequenceModalCompleteNoOpWhenAllComplete(t *testing.T) {
	v := seqFixture()
	v.Complete = true
	m := NewSequenceModal("hive", "p1", v)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	if cmd != nil {
		if _, ok := cmd().(SubmitRequest); ok {
			t.Error("c must NOT emit when all phases are already complete")
		}
	}
}

func TestSequenceModalSkipUsesCursorTask(t *testing.T) {
	m := NewSequenceModal("hive", "p1", seqFixture())
	// cursor starts at 0 (t1); move down to t2.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	req, ok := cmd().(SubmitRequest)
	if !ok || req.Kind != "sequence_skip" {
		t.Fatalf("got %T/%v want sequence_skip", cmd(), req.Kind)
	}
	if req.Params["task_id"] != "t2" {
		t.Errorf("task_id=%v want t2 (cursor task)", req.Params["task_id"])
	}
}

func TestSequenceModalCursorClamps(t *testing.T) {
	m := NewSequenceModal("hive", "p1", seqFixture())
	// 2 active tasks: k twice should not exceed index 1.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.cursor != 1 {
		t.Errorf("cursor=%d want 1 (clamped)", m.cursor)
	}
	// j back to 0, then j again clamps at 0.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.cursor != 0 {
		t.Errorf("cursor=%d want 0 (clamped)", m.cursor)
	}
}

func TestSequenceModalSetViewRefreshes(t *testing.T) {
	m := NewSequenceModal("hive", "p1", seqFixture())
	m.SetView(&rpc.SeqStatusView{Slug: "hive", Status: "paused", ActivePhase: "2",
		Phases: []rpc.SeqPhaseView{{Number: "2", Title: "Engine"}}})
	view := m.View(80, 24)
	if !strings.Contains(view, "paused") {
		t.Errorf("SetView should update rendered status; got:\n%s", view)
	}
}

func TestSequenceModalEscCloses(t *testing.T) {
	m := NewSequenceModal("hive", "p1", seqFixture())
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}

func TestSequenceModalNilView(t *testing.T) {
	m := NewSequenceModal("hive", "p1", nil)
	view := m.View(80, 24)
	if !strings.Contains(view, "loading") {
		t.Errorf("nil view should render a loading hint; got:\n%s", view)
	}
}
