package store

import (
	"context"
	"testing"
)

func TestApprovalRulesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.InsertApprovalRule(ctx, ApprovalRule{
		Scope: "global", ToolName: "Bash", ArgMatcher: "git *", Decision: "allow", Source: "user",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("expected non-zero rule id")
	}
	rules, err := s.ListApprovalRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ToolName != "Bash" || rules[0].ArgMatcher != "git *" || rules[0].Decision != "allow" {
		t.Fatalf("unexpected rules: %+v", rules)
	}
}

func TestApprovalAuditRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.InsertApproval(ctx, ApprovalAudit{
		RunID: "run-1", Stage: "implement", ToolName: "Bash", ToolInputJSON: `{"command":"ls"}`,
		Decision: "allow", ResolvedBy: "rule:1", Reason: "matched allow rule",
		RequestedAt: 100, ResolvedAt: 101,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListApprovals(ctx, "run-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Decision != "allow" || got[0].ResolvedBy != "rule:1" {
		t.Fatalf("unexpected audit: %+v", got)
	}
	// Filter by a different run -> empty.
	other, err := s.ListApprovals(ctx, "run-2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("expected no rows for run-2; got %d", len(other))
	}
}
