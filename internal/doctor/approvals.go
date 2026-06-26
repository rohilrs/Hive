package doctor

import (
	"context"
	"fmt"
)

// runApprovalsChecks surfaces HealthSnapshot.PendingApprovals as an
// info-only ok with a count message. Pending approvals are an expected
// operational state (the daemon hands them to the operator); the doctor
// reports their presence so users notice when the queue is non-empty,
// but never warns or errors. Skips when the daemon is unreachable.
func runApprovalsChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	if client == nil {
		return []Check{skipCheck("approvals.queue_depth", "approvals", "skipped — daemon not running")}
	}
	if err := client.Status(ctx); err != nil {
		return []Check{skipCheck("approvals.queue_depth", "approvals", "skipped — daemon not running")}
	}
	health, hErr := client.Health(ctx)
	if hErr != nil {
		return []Check{skipCheck("approvals.queue_depth", "approvals", "daemon.health rpc failed: "+hErr.Error())}
	}
	n := health.PendingApprovals
	if n == 0 {
		return []Check{{Name: "approvals.queue_depth", Subsystem: "approvals", Status: StatusOK, Message: "0 pending"}}
	}
	return []Check{{
		Name: "approvals.queue_depth", Subsystem: "approvals", Status: StatusOK,
		Message: fmt.Sprintf("%d pending", n),
		Hint:    "resolve via `hive approvals resolve` or in the TUI",
	}}
}
