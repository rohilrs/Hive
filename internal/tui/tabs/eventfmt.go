package tabs

import (
	"fmt"
	"strings"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// FormatEventDetails distills an event's Data into a readable one-liner
// (shared by the Events firehose + the run drill-in). Replaces the old
// raw k=v dumps — and crucially formats tool.decision, which the firehose
// previously rendered blank.
func FormatEventDetails(ev TimedEvent) string {
	d := ev.Data
	s := func(k string) string { v, _ := d[k].(string); return v }
	switch ev.Type {
	case rpc.EventToolDecision:
		// "approve  Bash  go build ./..."
		return strings.TrimSpace(fmt.Sprintf("%-7s %-10s %s", s("decision"), s("tool_name"), s("arg")))
	case rpc.EventStageStarted:
		return strings.TrimSpace(fmt.Sprintf("%s iter %v · %s", s("name"), d["iter"], s("model")))
	case rpc.EventStageEnded:
		v := s("verdict")
		if v == "" {
			v = "—"
		}
		return fmt.Sprintf("%s iter %v · %s", s("name"), d["iter"], v)
	case rpc.EventRunStarted:
		return s("task_title")
	case rpc.EventRunEnded:
		return strings.TrimSpace(s("status") + " · " + s("summary"))
	case rpc.EventApprovalRequested:
		return "pending: " + s("tool_name")
	case rpc.EventApprovalResolved:
		return s("decision")
	case rpc.EventStallDetected:
		return fmt.Sprintf("L%v %s", d["layer"], s("tool"))
	case rpc.EventTaskCreated, rpc.EventTaskUpdated:
		return s("title")
	case rpc.EventDocumentationSubmitted:
		// files_changed arrives as []any after JSON round-trip.
		n := 0
		if fc, ok := d["files_changed"].([]any); ok {
			n = len(fc)
		}
		summary := s("summary")
		if summary == "" {
			summary = "(no summary)"
		}
		return fmt.Sprintf("documented: %s (%d file(s))", truncate(summary, 60), n)
	}
	return compactGeneric(d)
}

// compactGeneric is the fallback for unrecognized event types: a few
// well-known keys as k=v.
func compactGeneric(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	var bits []string
	for _, k := range []string{"run_id", "name", "status", "verdict", "tool_name", "decision"} {
		if v, ok := d[k]; ok {
			bits = append(bits, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(bits, " ")
}
