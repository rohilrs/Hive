package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

var envOverrides = map[string]func(*Config, string) error{
	"HIVE_CONCURRENCY_MAX_WORKERS":            func(c *Config, v string) error { return setInt(&c.Concurrency.MaxWorkers, "HIVE_CONCURRENCY_MAX_WORKERS", v) },
	"HIVE_CONCURRENCY_PER_REPO_CAP":           func(c *Config, v string) error { return setInt(&c.Concurrency.PerRepoCap, "HIVE_CONCURRENCY_PER_REPO_CAP", v) },
	"HIVE_COSTS_CAP_PER_RUN_USD":              func(c *Config, v string) error { return setFloat(&c.Costs.CapPerRunUSD, "HIVE_COSTS_CAP_PER_RUN_USD", v) },
	"HIVE_COSTS_CAP_PER_DAY_USD":              func(c *Config, v string) error { return setFloat(&c.Costs.CapPerDayUSD, "HIVE_COSTS_CAP_PER_DAY_USD", v) },
	"HIVE_PIPELINES_BUILD_MAX_ITERATIONS":     func(c *Config, v string) error { return setInt(&c.Pipelines.Build.MaxIterations, "HIVE_PIPELINES_BUILD_MAX_ITERATIONS", v) },
	"HIVE_PIPELINES_BUILD_CONFLICT_HOP_DEPTH": func(c *Config, v string) error { return setInt(&c.Pipelines.Build.ConflictHopDepth, "HIVE_PIPELINES_BUILD_CONFLICT_HOP_DEPTH", v) },
	"HIVE_PREDICTOR_FORCE_ENABLE":             func(c *Config, v string) error { return setBool(&c.Predictor.ForceEnable, "HIVE_PREDICTOR_FORCE_ENABLE", v) },
	"HIVE_STALL_DETECTION_NOTIFY_ON_STALL":    func(c *Config, v string) error { return setBool(&c.StallDetection.NotifyOnStall, "HIVE_STALL_DETECTION_NOTIFY_ON_STALL", v) },
	"HIVE_CLAUDE_CLI_BINARY":                  func(c *Config, v string) error { c.ClaudeCLI.Binary = v; return nil },
	"HIVE_SCAVENGER_PLUGIN_DIR":               func(c *Config, v string) error { c.Scavenger.PluginDir = v; return nil },
}

func applyEnvOverrides(cfg *Config) error {
	var errs []error
	for key, setter := range envOverrides {
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			if err := setter(cfg, v); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func setInt(dst *int, key, v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s=%q: invalid int: %w", key, v, err)
	}
	*dst = n
	return nil
}

func setFloat(dst *float64, key, v string) error {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("%s=%q: invalid float: %w", key, v, err)
	}
	*dst = f
	return nil
}

func setBool(dst *bool, key, v string) error {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%s=%q: invalid bool: %w", key, v, err)
	}
	*dst = b
	return nil
}
