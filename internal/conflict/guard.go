// Package conflict implements Hive's in-memory file-level conflict guard.
// When two runs would touch overlapping files (per the predictor's
// candidate list), the second is queued via the daemon scheduler. This
// package only owns the in-memory state; persistence (waiting_on, the
// pending+waiting_on store query, etc.) lives in internal/store.
//
// File-level granularity is coarse — two runs editing different
// symbols in the same file will serialize. Symbol-level granularity
// would need merge intelligence Hive doesn't have today; conservative-
// by-design per the Phase 2b spec.
package conflict

import "sync"

// Decision is the CheckAndReserve result. Proceed=true means the run
// reserved its files and can dispatch; Proceed=false means at least
// one file is reserved by another run, listed in WaitingOn.
type Decision struct {
	Proceed   bool
	WaitingOn []string
}

// Guard tracks file-set reservations per in-flight run.
type Guard struct {
	mu       sync.Mutex
	inFlight map[string]map[string]struct{} // runID -> fileSet
}

// NewGuard constructs an empty Guard.
func NewGuard() *Guard {
	return &Guard{
		inFlight: map[string]map[string]struct{}{},
	}
}

// CheckAndReserve atomically checks for file overlap with currently-
// reserved runs and, on no conflict, registers runID's file set.
// Returns the Decision. Runs with empty file lists always proceed
// (they can't conflict with anyone).
//
// Calling CheckAndReserve again for an already-reserved runID is
// allowed but a no-op on conflict (returns the same blockers as a
// fresh check would). On no-conflict it overwrites the reservation
// — useful for re-dispatch from pending+waiting_on after the original
// reservation was Released.
func (g *Guard) CheckAndReserve(runID string, files []string) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(files) == 0 {
		// No files to reserve, nothing to conflict with.
		g.inFlight[runID] = map[string]struct{}{}
		return Decision{Proceed: true}
	}

	blockers := map[string]struct{}{}
	for otherID, otherFiles := range g.inFlight {
		if otherID == runID {
			continue
		}
		for _, f := range files {
			if _, hit := otherFiles[f]; hit {
				blockers[otherID] = struct{}{}
				break
			}
		}
	}
	if len(blockers) > 0 {
		out := make([]string, 0, len(blockers))
		for id := range blockers {
			out = append(out, id)
		}
		return Decision{Proceed: false, WaitingOn: out}
	}

	set := make(map[string]struct{}, len(files))
	for _, f := range files {
		set[f] = struct{}{}
	}
	g.inFlight[runID] = set
	return Decision{Proceed: true}
}

// Release frees runID's reservation. No-op if runID isn't registered.
func (g *Guard) Release(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.inFlight, runID)
}
