package store

import "context"

// BoundSource is one (project, source) binding pair surfaced by
// ListBoundSources. The store knows the binding shape (Kind + Key +
// ProjectSlug); the live runtime state (LastSyncUnix, LastError,
// SyncIntervalMinutes) is held in-memory on the daemon's syncStatus
// and global config — those fields stay zero when populated from
// store alone, and callers (e.g. internal/doctor) should treat zero
// as "no data; skip staleness/error judgment".
type BoundSource struct {
	// Kind is the source name (e.g. "inbox", "github", "linear").
	Kind string
	// Key is the per-binding identifier (e.g. Linear team key, GitHub
	// repo). Encoded as a stable JSON string of the binding value so
	// the doctor can dedupe + display without re-decoding.
	Key string
	// ProjectSlug is the slug of the project this binding belongs to.
	ProjectSlug string
	// LastSyncUnix is the last successful sync time in unix seconds;
	// zero means "no data from store-only enumeration".
	LastSyncUnix int64
	// LastError is the most recent sync error message for this binding;
	// empty means "no error or no data".
	LastError string
	// SyncIntervalMinutes is the configured poll interval; zero means
	// "no data from store-only enumeration".
	SyncIntervalMinutes int
}

// ListBoundSources walks every project and emits one BoundSource per
// entry in projects.sources JSON. The store cannot populate live state
// (last_sync / last_error / sync_interval_minutes); those live in the
// daemon's in-memory syncStatus + global config. Doctor uses this helper
// as the canonical "what bindings exist on disk?" enumeration; staleness
// + error judgment falls back to "no data → ok".
func (s *Store) ListBoundSources(ctx context.Context) ([]BoundSource, error) {
	projects, err := s.ListProjects(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []BoundSource
	for _, p := range projects {
		for kind, val := range p.Sources {
			out = append(out, BoundSource{
				Kind:        kind,
				Key:         boundSourceKey(val),
				ProjectSlug: p.Slug,
			})
		}
	}
	return out, nil
}

// boundSourceKey renders a binding value as a short display string.
// projects.sources is opaque (map[string]any with per-source schemas),
// so we render strings as-is and other shapes as a placeholder.
func boundSourceKey(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]any); ok {
		// Best-effort: prefer common identifying keys.
		for _, k := range []string{"team", "repo", "key", "id", "slug", "name"} {
			if val, ok := m[k].(string); ok && val != "" {
				return val
			}
		}
	}
	return ""
}
