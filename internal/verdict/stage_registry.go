package verdict

import "sync"

// StageRegistry maps (runID, stage) to the active *Listener for that
// stage. The daemon's HTTP MCP route looks up the Listener by run+stage
// path params and calls Submit on it directly — bypassing the per-stage
// UDS socket entirely for HTTP transport.
//
// Adapters register a Listener at stage start and Remove it at stage
// end. Lookups by an unknown (run, stage) return ok=false so the HTTP
// route can return a clean -32000 ("stage not active") error.
type StageRegistry struct {
	mu   sync.Mutex
	live map[string]*Listener
}

// NewStageRegistry returns an empty registry.
func NewStageRegistry() *StageRegistry {
	return &StageRegistry{live: map[string]*Listener{}}
}

// Register associates a Listener with the (runID, stage) pair. Replaces
// any prior entry (a re-registration is treated as a re-start).
func (r *StageRegistry) Register(runID, stage string, l *Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[stageKey(runID, stage)] = l
}

// Get returns the Listener for (runID, stage), if registered.
func (r *StageRegistry) Get(runID, stage string) (*Listener, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.live[stageKey(runID, stage)]
	return l, ok
}

// Remove deletes the (runID, stage) entry. Idempotent.
func (r *StageRegistry) Remove(runID, stage string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, stageKey(runID, stage))
}

func stageKey(runID, stage string) string { return runID + "\x00" + stage }
