package approval

import (
	"context"
	"testing"
)

func TestStubAllowsEverything(t *testing.T) {
	e := NewStub()
	cases := []ToolUseRequest{
		{RunID: "r1", Stage: "implement", ToolName: "Bash", ToolInput: map[string]any{"command": "rm -rf node_modules"}},
		{RunID: "r1", Stage: "implement", ToolName: "Write", ToolInput: map[string]any{"file_path": "/etc/passwd"}},
		{RunID: "r1", Stage: "review", ToolName: "Read"},
	}
	for _, req := range cases {
		d, err := e.Evaluate(context.Background(), req)
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", req.ToolName, err)
		}
		if d.Kind != DecisionApprove {
			t.Errorf("stub denied %s: %+v", req.ToolName, d)
		}
	}
}

type fakeRuleStore struct {
	rules []Rule
	err   error
}

func (f fakeRuleStore) ListApprovalRules(_ context.Context) ([]Rule, error) {
	return f.rules, f.err
}

func TestRealEngineEvaluate(t *testing.T) {
	defaults := []Rule{
		{Scope: "global", ToolName: "Read", Decision: "allow"},
		{Scope: "global", ToolName: "Bash", ArgMatcher: "git *", Decision: "allow"},
		{Scope: "stage:review", ToolName: "Grep", Decision: "allow"},
	}
	persisted := []Rule{
		{ID: 5, Scope: "global", ToolName: "Bash", ArgMatcher: "rm *", Decision: "deny"},
	}
	e := NewRealEngine(fakeRuleStore{rules: persisted}, defaults)
	ctx := context.Background()

	cases := []struct {
		name string
		req  ToolUseRequest
		want DecisionKind
	}{
		{"mcp tool always allowed", ToolUseRequest{ToolName: "mcp__hive-stage__hive_submit_review_verdict"}, DecisionApprove},
		{"default allow Read", ToolUseRequest{ToolName: "Read", ToolInput: map[string]any{"file_path": "/x"}}, DecisionApprove},
		{"bash git allowed by glob", ToolUseRequest{ToolName: "Bash", ToolInput: map[string]any{"command": "git status"}}, DecisionApprove},
		{"bash rm denied (explicit deny beats nothing)", ToolUseRequest{ToolName: "Bash", ToolInput: map[string]any{"command": "rm -rf /"}}, DecisionDeny},
		{"unknown bash fail-closed", ToolUseRequest{ToolName: "Bash", ToolInput: map[string]any{"command": "make all"}}, DecisionDeny},
		{"unknown tool fail-closed", ToolUseRequest{ToolName: "WebFetch"}, DecisionDeny},
		{"stage-scoped grep only in review", ToolUseRequest{ToolName: "Grep", Stage: "implement"}, DecisionDeny},
		{"stage-scoped grep allowed in review", ToolUseRequest{ToolName: "Grep", Stage: "review"}, DecisionApprove},
	}
	for _, c := range cases {
		d, err := e.Evaluate(ctx, c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if d.Kind != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, d.Kind, d.Reason, c.want)
		}
	}
}

func TestRealEngineFailsClosedOnStoreError(t *testing.T) {
	e := NewRealEngine(fakeRuleStore{err: context.DeadlineExceeded}, nil)
	d, err := e.Evaluate(context.Background(), ToolUseRequest{ToolName: "Bash", ToolInput: map[string]any{"command": "ls"}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DecisionDeny {
		t.Errorf("store error should fail closed; got %s", d.Kind)
	}
}
