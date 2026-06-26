package daemon

import (
	"sync"
	"time"
)

// decomposeJob tracks one in-flight async decompose. Result/error are NOT
// stored here — they are delivered to clients via the event bus
// (decompose.proposed / decompose.failed). The registry exists to dedup
// concurrent starts of the same (slug, phase) and bound liveness.
type decomposeJob struct {
	id        string
	slug      string
	phase     string
	status    string // always "running" while registered — finish() deletes the entry rather than transitioning it
	startedAt time.Time
}

// decomposeJobs is the daemon's in-memory registry of running async
// decomposes. Mutex-guarded; lives for the daemon's lifetime. A daemon
// restart drops it (acceptable: decompose is propose-only and cheap to
// re-run).
type decomposeJobs struct {
	mu   sync.Mutex
	jobs map[string]*decomposeJob // id -> job
}

func newDecomposeJobs() *decomposeJobs {
	return &decomposeJobs{jobs: map[string]*decomposeJob{}}
}

// startOrExisting returns the id of an already-RUNNING job for (slug, phase)
// if one exists; otherwise it registers newID as a running job and returns it.
// Only running jobs dedup — terminal jobs have been removed, so there is no
// stale-id wait.
func (r *decomposeJobs) startOrExisting(newID, slug, phase string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.slug == slug && j.phase == phase && j.status == "running" {
			return j.id
		}
	}
	r.jobs[newID] = &decomposeJob{id: newID, slug: slug, phase: phase, status: "running", startedAt: time.Now()}
	return newID
}

// finish removes a job from the registry (its terminal event has been
// published).
func (r *decomposeJobs) finish(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, id)
}
