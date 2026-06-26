package daemon

import "testing"

func TestDecomposeJobsStartAndDedup(t *testing.T) {
	r := newDecomposeJobs()

	// First registration for (demo,1): nothing running, so it registers d1.
	id := r.startOrExisting("d1", "demo", "1")
	if id != "d1" {
		t.Fatalf("first start returned %q, want d1", id)
	}
	// A second start for the same (slug, phase) while d1 is running must
	// return the existing id, not register a new job.
	if got := r.startOrExisting("d2", "demo", "1"); got != "d1" {
		t.Fatalf("dedup returned %q, want existing d1", got)
	}
	// A different phase is independent.
	if got := r.startOrExisting("d3", "demo", "2"); got != "d3" {
		t.Fatalf("different phase returned %q, want new d3", got)
	}
	// After d1 finishes, a new start for (demo,1) is a fresh job (no stale wait).
	r.finish("d1")
	if got := r.startOrExisting("d4", "demo", "1"); got != "d4" {
		t.Fatalf("post-finish start returned %q, want new d4", got)
	}
}
