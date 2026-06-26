package tabs

import (
	"strings"
	"testing"

	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestFormatEventDetails(t *testing.T) {
	cases := []struct {
		ev   TimedEvent
		want string // substring that must appear
	}{
		{TimedEvent{Type: rpc.EventToolDecision, Data: map[string]any{"decision": "approve", "tool_name": "Bash", "arg": "go build ./..."}}, "go build ./..."},
		{TimedEvent{Type: rpc.EventStageEnded, Data: map[string]any{"name": "review", "iter": 0, "verdict": "APPROVE"}}, "APPROVE"},
		{TimedEvent{Type: rpc.EventRunEnded, Data: map[string]any{"status": "done", "summary": "approved on iter 0"}}, "approved on iter 0"},
		{TimedEvent{Type: rpc.EventRunStarted, Data: map[string]any{"task_title": "smoke test"}}, "smoke test"},
	}
	for _, c := range cases {
		got := FormatEventDetails(c.ev)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s details %q missing %q", c.ev.Type, got, c.want)
		}
	}
	// tool.decision must NOT be blank (the firehose regression).
	if FormatEventDetails(TimedEvent{Type: rpc.EventToolDecision, Data: map[string]any{"decision": "deny", "tool_name": "Bash", "arg": "make all"}}) == "" {
		t.Error("tool.decision details should not be blank")
	}
}
