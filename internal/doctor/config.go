package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// doctorChatConfig + doctorRootConfig are the minimal TOML decode
// shapes doctor needs from <hiveDir>/config.toml. Kept local rather
// than importing the daemon's config package so doctor stays
// dependency-light.
type doctorChatConfig struct {
	Provider string `toml:"provider"`
}

type doctorRootConfig struct {
	Chat doctorChatConfig `toml:"chat"`
}

// runConfigChecks validates ~/.hive/config.toml and per-project
// overrides. Four emitted checks:
//   - config.global_parses:     <hiveDir>/config.toml parses (or absent → ok).
//   - config.chat_provider:     [chat] provider is "api" or "claude-code".
//   - config.api_key:           ANTHROPIC_API_KEY present when provider="api".
//   - config.project_overrides: <hiveDir>/projects/*/config.toml each parses.
//
// Daemon-down-safe: pure filesystem reads. The empty provider defaults
// to "claude-code" so the api_key check is skipped rather than
// erroring when no config file exists.
func runConfigChecks(ctx context.Context, hiveDir string, client RPCClient) []Check {
	cfgPath := filepath.Join(hiveDir, "config.toml")
	var cfg doctorRootConfig
	var globalCheck Check

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		globalCheck = Check{Name: "config.global_parses", Subsystem: "config", Status: StatusOK, Message: "no config.toml (defaults will apply)"}
		// Treat provider as the default "claude-code" for downstream checks.
		cfg.Chat.Provider = "claude-code"
	} else {
		if _, err := toml.DecodeFile(cfgPath, &cfg); err != nil {
			return []Check{
				{Name: "config.global_parses", Subsystem: "config", Status: StatusError, Message: "parse " + cfgPath + ": " + err.Error(), Hint: "fix or remove the file"},
				skipCheck("config.chat_provider", "config", "skipped — config parse failed"),
				skipCheck("config.api_key", "config", "skipped — config parse failed"),
				skipCheck("config.project_overrides", "config", "skipped — global config parse failed"),
			}
		}
		globalCheck = Check{Name: "config.global_parses", Subsystem: "config", Status: StatusOK, Message: "ok"}
	}

	checks := []Check{globalCheck}

	// chat_provider
	provider := cfg.Chat.Provider
	if provider == "" {
		provider = "claude-code" // default
	}
	switch provider {
	case "api", "claude-code":
		checks = append(checks, Check{Name: "config.chat_provider", Subsystem: "config", Status: StatusOK, Message: provider})
	default:
		checks = append(checks, Check{
			Name: "config.chat_provider", Subsystem: "config", Status: StatusError,
			Message: "unknown chat.provider=" + provider,
			Hint:    "valid: \"api\" or \"claude-code\"",
		})
	}

	// api_key
	if provider == "api" {
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			checks = append(checks, Check{
				Name: "config.api_key", Subsystem: "config", Status: StatusError,
				Message: "ANTHROPIC_API_KEY not set (required for chat.provider=api)",
				Hint:    "export ANTHROPIC_API_KEY=... before starting the daemon",
			})
		} else {
			checks = append(checks, Check{Name: "config.api_key", Subsystem: "config", Status: StatusOK, Message: "present"})
		}
	} else {
		checks = append(checks, Check{Name: "config.api_key", Subsystem: "config", Status: StatusSkip, Message: "skipped (provider=" + provider + ")"})
	}

	// project_overrides
	projRoot := filepath.Join(hiveDir, "projects")
	projEntries, perr := os.ReadDir(projRoot)
	if os.IsNotExist(perr) {
		checks = append(checks, Check{Name: "config.project_overrides", Subsystem: "config", Status: StatusOK, Message: "no per-project configs"})
		return checks
	}
	if perr != nil {
		checks = append(checks, Check{Name: "config.project_overrides", Subsystem: "config", Status: StatusWarn, Message: "readdir " + projRoot + ": " + perr.Error()})
		return checks
	}
	var failures []string
	var checked int
	for _, e := range projEntries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(projRoot, e.Name(), "config.toml")
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		checked++
		var v map[string]any
		if _, err := toml.DecodeFile(p, &v); err != nil {
			failures = append(failures, p+": "+err.Error())
		}
	}
	if len(failures) == 0 {
		checks = append(checks, Check{Name: "config.project_overrides", Subsystem: "config", Status: StatusOK, Message: "all parse ok (" + strconv.Itoa(checked) + " files)"})
	} else {
		checks = append(checks, Check{
			Name: "config.project_overrides", Subsystem: "config", Status: StatusWarn,
			Message: strconv.Itoa(len(failures)) + " project config(s) failed to parse",
			Hint:    "files:\n  " + strings.Join(failures, "\n  "),
		})
	}
	return checks
}
