package doctor

import (
	"context"
	"testing"
)

func TestApprovalsQueueDepthZeroIsOK(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{health: HealthSnapshot{PendingApprovals: 0}}
	checks := runApprovalsChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "approvals.queue_depth")
	if c.Status != StatusOK {
		t.Errorf("zero pending: status=%s, want ok", c.Status)
	}
}

func TestApprovalsQueueDepthNonZeroStillOKWithMessage(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{health: HealthSnapshot{PendingApprovals: 3}}
	checks := runApprovalsChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "approvals.queue_depth")
	if c.Status != StatusOK {
		t.Errorf("3 pending: status=%s, want ok (info-only, not warn)", c.Status)
	}
	if c.Message == "" {
		t.Errorf("3 pending: empty message; want a count")
	}
}

func TestApprovalsSkipsWhenDaemonDown(t *testing.T) {
	hiveDir := t.TempDir()
	client := &stubRPCClient{statusErr: errSocketDown}
	checks := runApprovalsChecks(context.Background(), hiveDir, client)
	c := findCheck(t, checks, "approvals.queue_depth")
	if c.Status != StatusSkip {
		t.Errorf("daemon down: status=%s, want skip", c.Status)
	}
}
