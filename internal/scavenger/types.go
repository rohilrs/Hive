// Package scavenger is Hive's client for the Scavenger AST dependency
// graph + session memory engine. v1 (Phase 2a) manages scavenger's
// daemon lifecycle and discovers per-project plugin directories so the
// claudecode adapter can wire workers to scavenger's MCP bridge and
// hooks. Phase 2b will add direct MCP queries for the predictor.
package scavenger

import "time"

// Config configures a Client.
type Config struct {
	// Binary is the scavenger CLI path/name. Tests use a fake.
	Binary string

	// IndexTimeout bounds a single `scavenger index` invocation.
	// Zero means no timeout.
	IndexTimeout time.Duration

	// MaxConcurrentDaemons caps simultaneously-running per-worktree
	// daemons. 0 = unlimited.
	MaxConcurrentDaemons int

	// SocketWaitTimeout bounds StartDaemon's best-effort poll for the
	// daemon.sock to appear. 0 = default (10s). Tests set this small.
	SocketWaitTimeout time.Duration
}
