// Package doctor runs read-only health, drift, and config checks
// across Hive's subsystems (daemon, store, worktrees, sources, mcp,
// config, approvals) and aggregates them into a Report consumed by
// the hive doctor CLI.
package doctor

import (
	"context"
	"errors"
)

// Status is the per-check verdict.
type Status string

const (
	StatusOK    Status = "ok"
	StatusWarn  Status = "warn"
	StatusError Status = "error"
	StatusSkip  Status = "skip"
)

// Check is one audit finding. Composite checks (e.g. orphan worktrees)
// emit a single Check summarizing N findings; the Hint carries the
// per-finding detail.
type Check struct {
	Name      string `json:"name"`
	Subsystem string `json:"subsystem"`
	Status    Status `json:"status"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
}

// Summary is the aggregated count per status across all checks.
type Summary struct {
	OK       int `json:"ok"`
	Warnings int `json:"warnings"`
	Errors   int `json:"errors"`
	Skipped  int `json:"skipped"`
}

// Report is the full doctor output.
type Report struct {
	Checks  []Check `json:"checks"`
	Summary Summary `json:"summary"`
}

// computeSummary tallies the report's checks into a Summary.
func (r *Report) computeSummary() Summary {
	var sum Summary
	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			sum.OK++
		case StatusWarn:
			sum.Warnings++
		case StatusError:
			sum.Errors++
		case StatusSkip:
			sum.Skipped++
		}
	}
	return sum
}

// errSocketDown is the sentinel returned by RPCClient.Status when the
// daemon UDS is unreachable. Doctor's daemon-dependent checks compare
// against it to decide between "skip" and "error".
var errSocketDown = errors.New("daemon socket unreachable")

// RPCClient is the narrow interface doctor uses to talk to the daemon.
// In production it's a thin wrapper around the existing daemon UDS
// client; tests inject stubs.
type RPCClient interface {
	Status(ctx context.Context) error
	Health(ctx context.Context) (HealthSnapshot, error)
	SourcesStatus(ctx context.Context) (map[string]SourceStatusEntry, error)
}

// HealthSnapshot mirrors the daemon-side daemon.HealthSnapshot
// returned by the daemon.health RPC. Duplicated here to keep
// internal/doctor free of an internal/daemon import. Field names +
// json tags MUST match the daemon-side struct exactly.
type HealthSnapshot struct {
	ActiveRuns        int    `json:"active_runs"`
	PendingApprovals  int    `json:"pending_approvals"`
	MCPHTTPListenerOK bool   `json:"mcp_http_listener_ok"`
	MCPHTTPAddr       string `json:"mcp_http_addr"`
	SchemaVersionDB   int    `json:"schema_version_db"`
	UptimeSeconds     int64  `json:"uptime_seconds"`
	LastTickUnix      int64  `json:"last_tick_unix"`
}

// SourceStatusEntry mirrors the daemon-side daemon.SourceStatusEntry
// returned by the sources.status RPC. Duplicated here to keep
// internal/doctor free of an internal/daemon import. Field names +
// json tags MUST match the daemon-side struct exactly.
type SourceStatusEntry struct {
	LastSyncUnix int64  `json:"last_sync_unix"`
	Inserted     int    `json:"inserted"`
	Updated      int    `json:"updated"`
	Closed       int    `json:"closed"`
	Error        string `json:"error,omitempty"`
}
