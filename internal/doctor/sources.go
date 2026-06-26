package doctor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// staleSourceThresholdSeconds is the age threshold above which a
// source's LastSyncUnix is considered stale. 600s = 10 minutes —
// generous enough for the default sync intervals (5-30m) without
// being so loose that a truly hung syncer takes hours to surface.
// Hardcoded for now; a config-driven per-source threshold is a
// future enhancement.
const staleSourceThresholdSeconds = 600

// runSourcesChecks queries the daemon's sources.status RPC for the
// in-memory per-source last-sync state and classifies SR1 (staleness)
// and SR2 (last_error). The store-only enumeration is no longer needed
// since the daemon owns the authoritative state.
func runSourcesChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	if client == nil {
		return []Check{
			skipCheck("sources.staleness", "sources", "skipped — daemon not running"),
			skipCheck("sources.last_error", "sources", "skipped — daemon not running"),
		}
	}
	if err := client.Status(ctx); err != nil {
		return []Check{
			skipCheck("sources.staleness", "sources", "skipped — daemon not running"),
			skipCheck("sources.last_error", "sources", "skipped — daemon not running"),
		}
	}

	statuses, err := client.SourcesStatus(ctx)
	if err != nil {
		return []Check{
			{Name: "sources.staleness", Subsystem: "sources", Status: StatusWarn,
				Message: "daemon.sources_status rpc failed: " + err.Error()},
			skipCheck("sources.last_error", "sources", "skipped — sources_status rpc failed"),
		}
	}

	if len(statuses) == 0 {
		// No source has synced yet. Normal state on a fresh daemon OR
		// a daemon with no bound sources. Either way: ok.
		return []Check{
			{Name: "sources.staleness", Subsystem: "sources", Status: StatusOK, Message: "no sources have synced yet (or none bound)"},
			{Name: "sources.last_error", Subsystem: "sources", Status: StatusOK, Message: "no sources have synced yet (or none bound)"},
		}
	}

	// SR1 staleness: any source with LastSyncUnix older than threshold.
	// LastSyncUnix==0 means "never synced" — not stale.
	now := time.Now().Unix()
	var stale []string
	for name, st := range statuses {
		if st.LastSyncUnix == 0 {
			continue
		}
		age := now - st.LastSyncUnix
		if age > staleSourceThresholdSeconds {
			stale = append(stale, fmt.Sprintf("%s: last synced %ds ago (threshold %ds)", name, age, staleSourceThresholdSeconds))
		}
	}
	sort.Strings(stale)

	// SR2 errors: any source with non-empty Error on most-recent sync.
	var errs []string
	for name, st := range statuses {
		if st.Error != "" {
			errs = append(errs, fmt.Sprintf("%s: %s", name, st.Error))
		}
	}
	sort.Strings(errs)

	var out []Check
	if len(stale) == 0 {
		out = append(out, Check{Name: "sources.staleness", Subsystem: "sources", Status: StatusOK,
			Message: fmt.Sprintf("all %d source(s) fresh", len(statuses))})
	} else {
		out = append(out, Check{Name: "sources.staleness", Subsystem: "sources", Status: StatusWarn,
			Message: fmt.Sprintf("%d source(s) stale (threshold %ds)", len(stale), staleSourceThresholdSeconds),
			Hint:    "check `hive sync --status`:\n  " + strings.Join(stale, "\n  ")})
	}
	if len(errs) == 0 {
		out = append(out, Check{Name: "sources.last_error", Subsystem: "sources", Status: StatusOK,
			Message: fmt.Sprintf("all %d source(s) error-free", len(statuses))})
	} else {
		out = append(out, Check{Name: "sources.last_error", Subsystem: "sources", Status: StatusWarn,
			Message: fmt.Sprintf("%d source(s) with last_error", len(errs)),
			Hint:    "errors:\n  " + strings.Join(errs, "\n  ")})
	}
	return out
}
