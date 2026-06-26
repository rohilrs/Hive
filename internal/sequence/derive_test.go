package sequence

import "testing"

func tv(id, phase, status, gate string) TaskView {
	return TaskView{ID: id, Title: id, Phase: phase, Status: status, GateState: gate}
}

func TestDeriveActivePhaseLowestIncomplete(t *testing.T) {
	order := []string{"1", "2", "3"}
	titles := map[string]string{"1": "One", "2": "Two", "3": "Three"}
	tasks := []TaskView{
		tv("a", "1", "done", "satisfied"),
		tv("b", "1", "done", "satisfied"),
		tv("c", "2", "running", "none"),
		tv("d", "3", "pending", "none"),
	}
	p := Derive(order, titles, tasks, nil)
	if p.ActivePhase != "2" {
		t.Errorf("ActivePhase = %q, want 2", p.ActivePhase)
	}
	if p.Complete {
		t.Error("Complete = true, want false")
	}
	if len(p.Phases) != 3 {
		t.Fatalf("phases = %d, want 3", len(p.Phases))
	}
	if !p.Phases[0].Complete {
		t.Error("phase 1 should be complete")
	}
	if p.Phases[1].Complete {
		t.Error("phase 2 should not be complete")
	}
}

func TestDeriveSkippedCountsAsResolved(t *testing.T) {
	order := []string{"1"}
	p := Derive(order, map[string]string{"1": "One"}, []TaskView{
		tv("a", "1", "done", "satisfied"),
		tv("b", "1", "abandoned", "skipped"),
	}, nil)
	if !p.Complete || p.ActivePhase != "" {
		t.Errorf("expected complete with no active phase; got active=%q complete=%v", p.ActivePhase, p.Complete)
	}
}

// A terminal "done" task whose gate never advanced (GateNone — e.g. an
// audit/plan task that opened no PR) must count as resolved, or its phase would
// never complete even though all work is finished.
func TestDeriveDoneWithGateNoneCountsAsResolved(t *testing.T) {
	order := []string{"1"}
	p := Derive(order, map[string]string{"1": "One"}, []TaskView{
		tv("audit", "1", "done", "none"),
		tv("build", "1", "done", "satisfied"),
	}, nil)
	if !p.Complete || p.ActivePhase != "" {
		t.Errorf("expected phase complete despite the done/GateNone audit task; got active=%q complete=%v", p.ActivePhase, p.Complete)
	}
}

func TestDeriveBlockers(t *testing.T) {
	order := []string{"1"}
	p := Derive(order, map[string]string{"1": "One"}, []TaskView{
		tv("a", "1", "needs_attention", "none"),
		tv("b", "1", "pending", "none"),
	}, nil)
	if p.ActivePhase != "1" {
		t.Fatalf("active = %q, want 1", p.ActivePhase)
	}
	if len(p.Phases[0].Blocked) != 1 || p.Phases[0].Blocked[0].ID != "a" {
		t.Errorf("blocked = %+v, want [a]", p.Phases[0].Blocked)
	}
}

// TestDeriveMergeFailedBlocksPhase pins the circuit-breaker terminal gate's
// contract: a merge_failed task is NOT resolved(), so it keeps its phase
// incomplete (the phase stays the active phase) AND surfaces as a needs_attention
// blocker for a human to confirm + clear.
func TestDeriveMergeFailedBlocksPhase(t *testing.T) {
	// Sanity: the gate is explicitly NOT resolved.
	if resolved(tv("x", "1", "needs_attention", GateMergeFailed)) {
		t.Fatal("merge_failed must NOT be resolved()")
	}

	order := []string{"1", "2"}
	titles := map[string]string{"1": "One", "2": "Two"}
	p := Derive(order, titles, []TaskView{
		tv("a", "1", "done", "satisfied"),
		tv("b", "1", "needs_attention", GateMergeFailed), // merge gave up
		tv("c", "2", "pending", "none"),
	}, nil)

	if p.Phases[0].Complete {
		t.Error("phase 1 must NOT be complete with an unresolved merge_failed task")
	}
	if p.ActivePhase != "1" {
		t.Errorf("ActivePhase = %q, want 1 (merge_failed blocks the phase, no advance)", p.ActivePhase)
	}
	if p.Complete {
		t.Error("plan must not be Complete while a merge_failed task blocks a phase")
	}
	if len(p.Phases[0].Blocked) != 1 || p.Phases[0].Blocked[0].ID != "b" {
		t.Errorf("Blocked = %+v, want [b] (merge_failed surfaces as needs_attention blocker)", p.Phases[0].Blocked)
	}
}

func TestDeriveEmptyPhaseStillActive(t *testing.T) {
	// Phase 1 has tasks (all done); phase 2 has NO tasks yet (not decomposed).
	// Active phase must be 2 (lowest incomplete in roadmap order), idle — it
	// must NOT skip ahead to phase 3.
	order := []string{"1", "2", "3"}
	titles := map[string]string{"1": "One", "2": "Two", "3": "Three"}
	tasks := []TaskView{
		tv("a", "1", "done", "satisfied"),
		tv("d", "3", "pending", "none"),
	}
	p := Derive(order, titles, tasks, nil)
	if p.ActivePhase != "2" {
		t.Errorf("ActivePhase = %q, want 2 (empty phase blocks ordering)", p.ActivePhase)
	}
}

func TestDeriveSkippedBlockerNotInBlocked(t *testing.T) {
	order := []string{"1"}
	p := Derive(order, map[string]string{"1": "One"}, []TaskView{
		tv("a", "1", "needs_attention", "skipped"), // skipped-to-unblock
		tv("b", "1", "done", "satisfied"),
	}, nil)
	if !p.Complete {
		t.Error("want Complete=true when all tasks resolved")
	}
	if len(p.Phases[0].Blocked) != 0 {
		t.Errorf("Blocked = %+v, want empty (resolved task must not appear as blocker)", p.Phases[0].Blocked)
	}
}

func TestDeriveUnsequencedAndUnknownPhase(t *testing.T) {
	order := []string{"1"}
	p := Derive(order, map[string]string{"1": "One"}, []TaskView{
		tv("a", "1", "pending", "none"),
		tv("x", "", "pending", "none"),  // no roadmap_phase
		tv("y", "9", "pending", "none"), // phase not in roadmap
	}, nil)
	ids := map[string]bool{}
	for _, t2 := range p.Unsequenced {
		ids[t2.ID] = true
	}
	if !ids["x"] || !ids["y"] || len(p.Unsequenced) != 2 {
		t.Errorf("Unsequenced = %+v, want {x,y}", p.Unsequenced)
	}
}

func TestDeriveCompletedPhaseAdvances(t *testing.T) {
	order := []string{"1", "2"}
	titles := map[string]string{"1": "Foundation", "2": "Snapshot"}
	plan := Derive(order, titles, nil, map[string]bool{"1": true})
	if !plan.Phases[0].Complete {
		t.Error("phase 1 should be Complete when in completedPhases")
	}
	if plan.ActivePhase != "2" {
		t.Errorf("ActivePhase=%q, want 2 (1 completed → advance)", plan.ActivePhase)
	}
	plan2 := Derive(order, titles, nil, map[string]bool{"99": true})
	if plan2.ActivePhase != "1" {
		t.Errorf("ActivePhase=%q, want 1 (unknown completed ignored)", plan2.ActivePhase)
	}
}
