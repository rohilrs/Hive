package tui

import (
	"testing"

	"github.com/rohilrs/Hive/internal/tui/tabs"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// TestInitialStateSeedsSequenceStatus verifies that an initialStateMsg
// carrying a populated SequenceStatus map lands in the snapshot cache
// (keyed by project ID) after Update.
func TestInitialStateSeedsSequenceStatus(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	msg := initialStateMsg{
		Projects: []rpc.ProjectView{
			{ID: "p1", Slug: "alpha", Name: "Alpha", DispatchMode: "sequenced"},
		},
		SequenceStatus: map[string]*rpc.SeqStatusView{
			"p1": {Slug: "alpha", Status: "active", Policy: "human_merge", ActivePhase: "1"},
		},
	}
	m.Update(msg)

	got := m.snapshot.SequenceStatus["p1"]
	if got == nil {
		t.Fatalf("snapshot.SequenceStatus[p1] = nil, want a view")
	}
	if got.Slug != "alpha" || got.Status != "active" || got.ActivePhase != "1" {
		t.Fatalf("unexpected view: %+v", got)
	}
	// The project's DispatchMode should also have been mirrored into the
	// snapshot's ProjectView.
	if p := m.snapshot.Projects["p1"]; p == nil || p.DispatchMode != "sequenced" {
		t.Fatalf("project p1 DispatchMode not seeded: %+v", p)
	}
}

// TestSequenceStatusMsgFoldsIntoSnapshot verifies the top-level
// sequenceStatusMsg case writes the refreshed view into the cache.
func TestSequenceStatusMsgFoldsIntoSnapshot(t *testing.T) {
	m := NewModel(nil, NewSnapshot(), []tabs.TabModel{stubTab{name: "A"}})
	m.Update(sequenceStatusMsg{
		ProjectID: "p2",
		View:      &rpc.SeqStatusView{Slug: "beta", Status: "paused"},
	})
	got := m.snapshot.SequenceStatus["p2"]
	if got == nil || got.Status != "paused" || got.Slug != "beta" {
		t.Fatalf("sequenceStatusMsg not folded into snapshot: %+v", got)
	}
}

// TestSeqEventProjectResolvesSlug checks the event→slug resolver returns
// the right slug for a known project and ok=false otherwise.
func TestSeqEventProjectResolvesSlug(t *testing.T) {
	s := NewSnapshot()
	s.Projects["p1"] = &ProjectView{ID: "p1", Slug: "alpha"}
	m := NewModel(nil, s, []tabs.TabModel{stubTab{name: "A"}})

	t.Run("known project", func(t *testing.T) {
		ev := rpc.EventMessage{
			Type: rpc.EventSequenceGateChanged,
			Data: map[string]any{"project_id": "p1", "task_id": "t1", "gate_state": "satisfied"},
		}
		pid, slug, ok := m.seqEventProject(ev)
		if !ok {
			t.Fatalf("ok=false for known project")
		}
		if pid != "p1" || slug != "alpha" {
			t.Fatalf("got pid=%q slug=%q, want p1/alpha", pid, slug)
		}
	})

	t.Run("unknown project", func(t *testing.T) {
		ev := rpc.EventMessage{
			Type: rpc.EventSequenceGateChanged,
			Data: map[string]any{"project_id": "nope"},
		}
		if _, _, ok := m.seqEventProject(ev); ok {
			t.Fatalf("ok=true for unknown project, want false")
		}
	})

	t.Run("missing project_id", func(t *testing.T) {
		ev := rpc.EventMessage{Type: rpc.EventSequenceUpdated, Data: map[string]any{}}
		if _, _, ok := m.seqEventProject(ev); ok {
			t.Fatalf("ok=true for missing project_id, want false")
		}
	})
}
