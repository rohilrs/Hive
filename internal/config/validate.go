package config

import (
	"errors"
	"fmt"
)

func (c *Config) Validate() error {
	var errs []error

	if c.Concurrency.MaxWorkers <= 0 {
		errs = append(errs, errors.New("concurrency.max_workers must be > 0"))
	}
	if c.Concurrency.PerRepoCap <= 0 {
		errs = append(errs, errors.New("concurrency.per_repo_cap must be > 0"))
	}

	if c.Costs.CapPerRunUSD < 0 {
		errs = append(errs, errors.New("costs.cap_per_run_usd must be >= 0"))
	}
	if c.Costs.CapPerDayUSD < 0 {
		errs = append(errs, errors.New("costs.cap_per_day_usd must be >= 0"))
	}
	if p := c.Costs.AlertAtPct; p < 0 || p > 100 {
		errs = append(errs, fmt.Errorf("costs.alert_at_pct must be in [0, 100], got %d", p))
	}

	for name, depth := range map[string]int{
		"build":         c.Pipelines.Build.ConflictHopDepth,
		"debug":         c.Pipelines.Debug.ConflictHopDepth,
		"plan":          c.Pipelines.Plan.ConflictHopDepth,
		"finish_branch": c.Pipelines.FinishBranch.ConflictHopDepth,
	} {
		if depth < 0 || depth > 3 {
			errs = append(errs, fmt.Errorf(
				"pipelines.%s.conflict_hop_depth must be in [0, 3], got %d", name, depth))
		}
	}

	if want := c.Pipelines.Build.MaxIterations; want > 0 {
		if len(c.Models.WorkerLadder) < want {
			errs = append(errs, fmt.Errorf(
				"models.worker_ladder length %d must be >= pipelines.build.max_iterations (%d)",
				len(c.Models.WorkerLadder), want))
		}
		if len(c.Models.ReviewerLadder) < want {
			errs = append(errs, fmt.Errorf(
				"models.reviewer_ladder length %d must be >= pipelines.build.max_iterations (%d)",
				len(c.Models.ReviewerLadder), want))
		}
	}

	if t := c.Predictor.PrecisionKillThreshold; t < 0 || t > 1 {
		errs = append(errs, fmt.Errorf(
			"predictor.precision_kill_threshold must be in [0, 1], got %v", t))
	}

	if c.StallDetection.EventHeartbeatSeconds <= 0 {
		errs = append(errs, errors.New("stall_detection.event_heartbeat_seconds must be > 0"))
	}
	if c.StallDetection.ToolCallMaxMinutes <= 0 {
		errs = append(errs, errors.New("stall_detection.tool_call_max_minutes must be > 0"))
	}
	if t := c.StallDetection.LoopSimilarityThreshold; t < 0 || t > 1 {
		errs = append(errs, fmt.Errorf(
			"stall_detection.loop_similarity_threshold must be in [0, 1], got %v", t))
	}

	if m := c.Scheduler.DispatchMode; m != "" &&
		m != DispatchModeManual && m != DispatchModeAutoAll && m != DispatchModeSequenced {
		errs = append(errs, fmt.Errorf(
			"scheduler.dispatch_mode %q is not valid; must be one of %q, %q, %q",
			m, DispatchModeManual, DispatchModeAutoAll, DispatchModeSequenced))
	}

	return errors.Join(errs...)
}
