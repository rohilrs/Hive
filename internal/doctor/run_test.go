package doctor

import (
	"context"
	"testing"
)

// stubRPCClient lets tests inject a "daemon up / daemon down" path
// without spinning a real daemon. Tasks 6-8's tests also use this
// stub (when they need a fixture client) so leave it package-level.
type stubRPCClient struct {
	statusErr        error
	health           HealthSnapshot
	healthErr        error
	sourcesStatus    map[string]SourceStatusEntry
	sourcesStatusErr error
}

func (s *stubRPCClient) Status(ctx context.Context) error { return s.statusErr }
func (s *stubRPCClient) Health(ctx context.Context) (HealthSnapshot, error) {
	return s.health, s.healthErr
}
func (s *stubRPCClient) SourcesStatus(ctx context.Context) (map[string]SourceStatusEntry, error) {
	return s.sourcesStatus, s.sourcesStatusErr
}

func TestRunReturnsReportWithAllSubsystemsCovered(t *testing.T) {
	hiveDir := t.TempDir()
	rep := Run(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	seen := map[string]bool{}
	for _, c := range rep.Checks {
		seen[c.Subsystem] = true
	}
	wantSubsystems := []string{"daemon", "store", "worktrees", "sources", "mcp", "config", "approvals"}
	for _, sub := range wantSubsystems {
		if !seen[sub] {
			t.Errorf("missing checks for subsystem %q", sub)
		}
	}
}

func TestRunComputesSummary(t *testing.T) {
	hiveDir := t.TempDir()
	rep := Run(context.Background(), hiveDir, &stubRPCClient{statusErr: errSocketDown})
	if rep.Summary.OK+rep.Summary.Warnings+rep.Summary.Errors+rep.Summary.Skipped != len(rep.Checks) {
		t.Errorf("summary counts don't sum to len(checks)=%d: %+v", len(rep.Checks), rep.Summary)
	}
}
