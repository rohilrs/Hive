package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show daemon status",
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := rpcCall(rpc.MethodStatus, map[string]any{})
		if err != nil {
			return err
		}
		fmt.Printf("Adapter:       %v\n", result["adapter"])
		fmt.Printf("Pending tasks: %v\n", result["pending_tasks"])
		fmt.Printf("Running runs:  %v\n", result["running_runs"])
		if pa := result["pending_approvals"]; pa != nil {
			if n, ok := pa.(float64); ok && n > 0 {
				fmt.Printf("⚠ Pending approvals: %v  (resolve in `hive tui` Approvals tab)\n", int(n))
			}
		}
		for _, r := range coerceSlice(result["running"]) {
			fmt.Printf("  %s  task=%s  pipeline=%s  %s\n",
				strField(r, "id"),
				strField(r, "task_id"),
				strField(r, "pipeline"),
				formatStartedAgo(r["started_at"]),
			)
		}
		recent := coerceSlice(result["recent"])
		if len(recent) > 0 {
			fmt.Printf("\nRecent runs (last %d):\n", len(recent))
			for _, r := range recent {
				summary := strField(r, "summary")
				if summary == "" {
					summary = "(no summary)"
				}
				fmt.Printf("  %s  %-16s  %s  %s\n",
					strField(r, "id"),
					strField(r, "status"),
					formatEndedAgo(r["ended_at"]),
					summary,
				)
			}
		}
		return nil
	},
}

func coerceSlice(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func formatStartedAgo(v any) string {
	t, ok := unixFromAny(v)
	if !ok {
		return "started ?"
	}
	return "started " + humanizeAgo(time.Since(t)) + " ago"
}

func formatEndedAgo(v any) string {
	t, ok := unixFromAny(v)
	if !ok {
		return "ended ?"
	}
	return "ended " + humanizeAgo(time.Since(t)) + " ago"
}

func unixFromAny(v any) (time.Time, bool) {
	switch n := v.(type) {
	case float64:
		return time.Unix(int64(n), 0), true
	case int64:
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

func humanizeAgo(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
