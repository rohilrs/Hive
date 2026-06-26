// Package sequence holds the pure (I/O-free) logic for the sequenced task
// dispatcher: deriving an ordered roadmap-phase execution plan from a
// project's tasks and the roadmap's phase order. Kept free of store/daemon
// dependencies so it is exhaustively unit-testable.
package sequence

// Gate state constants (tasks.gate_state). The state machine that ADVANCES
// these lands in Phase 2b; this package only reads them to decide whether a
// task is "resolved".
const (
	GateNone          = "none"
	GateBuilt         = "built"
	GatePROpen        = "pr_open"
	GateAwaitingMerge = "awaiting_merge"
	GateSatisfied     = "satisfied"
	GateSkipped       = "skipped"
	// GateMergeFailed is a TERMINAL gate the merge queue sets when it gives up
	// on a task after repeated failed merge attempts (the circuit breaker) or
	// when a resolve run can't even be provisioned (e.g. the PR branch was
	// deleted post-merge). It is deliberately NOT in resolved(): a merge_failed
	// task stays UNRESOLVED so it keeps blocking its phase and surfaces as a
	// needs_attention blocker for a human to confirm + mark done. detectMerges
	// only queries awaiting_merge, so a merge_failed task is never re-picked.
	GateMergeFailed = "merge_failed"
)

// TaskView is the minimal task projection the derivation needs.
type TaskView struct {
	ID        string
	Title     string
	Phase     string // roadmap_phase metadata ("" if absent)
	Status    string // task status (pending|running|done|needs_attention|...)
	GateState string // gate_state column
}

// PhaseRollup is one roadmap phase with its tasks and rollup flags.
type PhaseRollup struct {
	Number   string
	Title    string
	Tasks    []TaskView
	Complete bool       // true if explicitly completed (completedPhases) OR has >=1 task and every task is resolved (satisfied|skipped)
	Blocked  []TaskView // tasks in needs_attention (surface as blockers)
}

// Plan is the derived sequenced execution plan.
type Plan struct {
	Phases      []PhaseRollup // roadmap document order
	ActivePhase string        // lowest incomplete phase number; "" if all complete
	Unsequenced []TaskView    // tasks with no/unknown roadmap_phase
	Complete    bool          // every phase complete (and >=1 phase exists)
}

// resolved reports whether a task no longer blocks its phase from completing.
// A task is resolved when its gate is satisfied/skipped OR it has reached the
// terminal "done" status. The status check matters for tasks that finish
// WITHOUT advancing the merge gate — e.g. an audit/plan-pipeline task that
// produces a doc but opens no PR, so its gate stays GateNone even though the
// work is complete. Without this, such a task would block its phase forever.
func resolved(t TaskView) bool {
	return t.GateState == GateSatisfied || t.GateState == GateSkipped || t.Status == "done"
}

// Derive builds the Plan. phaseOrder is the roadmap's phases in document
// order; phaseTitles maps phase number -> title; tasks is the project's tasks;
// completedPhases is the set of phase numbers that have been explicitly marked
// complete (e.g. via MarkPhaseComplete) — a phase in this set is considered
// complete regardless of its task count, allowing the dispatcher to advance
// past a shipped or zero-task phase.
//
// Active phase = the lowest phase (in phaseOrder) that is not complete. A
// phase with zero tasks is NOT complete (nothing has been decomposed yet), so
// it becomes the active phase and the dispatcher idles there rather than
// running a later phase — this enforces ordering under incremental decompose.
// Exception: if a zero-task phase is in completedPhases it IS complete and
// the active phase advances to the next one.
func Derive(phaseOrder []string, phaseTitles map[string]string, tasks []TaskView, completedPhases map[string]bool) Plan {
	known := make(map[string]bool, len(phaseOrder))
	byPhase := make(map[string][]TaskView, len(phaseOrder))
	for _, n := range phaseOrder {
		known[n] = true
	}
	var unseq []TaskView
	for _, t := range tasks {
		if t.Phase == "" || !known[t.Phase] {
			unseq = append(unseq, t)
			continue
		}
		byPhase[t.Phase] = append(byPhase[t.Phase], t)
	}

	plan := Plan{Unsequenced: unseq}
	plan.Complete = len(phaseOrder) > 0
	for _, n := range phaseOrder {
		pr := PhaseRollup{Number: n, Title: phaseTitles[n], Tasks: byPhase[n]}
		allResolved := true
		for _, t := range pr.Tasks {
			if !resolved(t) {
				allResolved = false
			}
			if t.Status == "needs_attention" && !resolved(t) {
				pr.Blocked = append(pr.Blocked, t)
			}
		}
		pr.Complete = completedPhases[n] || (len(pr.Tasks) > 0 && allResolved)
		if !pr.Complete {
			plan.Complete = false
			if plan.ActivePhase == "" {
				plan.ActivePhase = n
			}
		}
		plan.Phases = append(plan.Phases, pr)
	}
	return plan
}
