package doctor

import (
	"context"
	"time"
)

// checkFn returns one or more Checks for a subsystem.
type checkFn func(ctx context.Context, hiveDir string, client RPCClient) []Check

// Run executes every subsystem check with a 2s per-check timeout and
// assembles the Report. client may be nil; doctor handles that as
// "daemon definitely down."
func Run(ctx context.Context, hiveDir string, client RPCClient) Report {
	checks := []checkFn{
		runDaemonChecks,
		runStoreChecks,
		runWorktreesChecks,
		runSourcesChecks,
		runMCPChecks,
		runConfigChecks,
		runApprovalsChecks,
	}
	rep := Report{}
	for _, fn := range checks {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		results := fn(cctx, hiveDir, client)
		cancel()
		rep.Checks = append(rep.Checks, results...)
	}
	rep.Summary = rep.computeSummary()
	return rep
}
