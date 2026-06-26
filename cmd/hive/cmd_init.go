package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/config"
)

// initCmd is registered in main.go alongside the other subcommands.
var initCmd = newInitCmd()

func newInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize ~/.hive with a default config.toml + run env checks.",
		Long: `hive init creates ~/.hive/config.toml from a template, then
runs environment checks (claude, git, gh, ANTHROPIC_API_KEY).
Refuses to overwrite an existing config.toml unless --force is set.

Does NOT start the daemon — inspect/edit the config before running
hive daemon.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			hiveDir := hiveDirForInit()
			if err := runInit(hiveDir, force); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "\nhive init — initialized %s\n\n", hiveDir)
			fmt.Fprintf(os.Stdout, "  %s config.toml written to %s\n", envGlyph(envOK), filepath.Join(hiveDir, "config.toml"))
			checks := runEnvChecks()
			hardErr := false
			for _, c := range checks {
				fmt.Fprintf(os.Stdout, "  %s %s\n", envGlyph(c.Status), c.Message)
				if c.Hint != "" {
					fmt.Fprintf(os.Stdout, "    — %s\n", c.Hint)
				}
				if c.Status == envError {
					hardErr = true
				}
			}
			fmt.Fprintln(os.Stdout)
			fmt.Fprintln(os.Stdout, "Next steps:")
			fmt.Fprintln(os.Stdout, "  1. hive daemon                          # start the daemon in the foreground")
			fmt.Fprintln(os.Stdout, "  2. hive project add hive ~/code/hive    # register your first project")
			fmt.Fprintln(os.Stdout, "  3. hive task add hive \"your first task\"")
			fmt.Fprintln(os.Stdout, "  4. hive tui                             # open the TUI")
			fmt.Fprintln(os.Stdout)
			fmt.Fprintln(os.Stdout, "Inspect or edit ~/.hive/config.toml before starting the daemon.")
			fmt.Fprintln(os.Stdout, "Run `hive doctor` any time to audit daemon + state health.")
			if hardErr {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing config.toml")
	return cmd
}

// hiveDirForInit mirrors hiveDirForDoctor — there's no HIVE_DIR env
// var today; ~/.hive is the only state location.
func hiveDirForInit() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hive")
}

// runInit writes the default config to hiveDir/config.toml. Returns
// an error if the file exists without --force, or if mkdir/write
// fails. Extracted so cmd_init_test.go can drive it against TempDir.
func runInit(hiveDir string, force bool) error {
	configPath := filepath.Join(hiveDir, "config.toml")
	if _, err := os.Stat(configPath); err == nil {
		if !force {
			return fmt.Errorf("config.toml already exists at %s; use --force to overwrite (or edit it directly)", configPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", configPath, err)
	}
	if err := os.MkdirAll(hiveDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", hiveDir, err)
	}
	if err := os.WriteFile(configPath, []byte(config.DefaultConfigTOML), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// envStatus is the per-check verdict for hive init's environment
// section. Distinct from doctor.Status — init has no subsystem
// grouping, and "info" is a real terminal state for not-required
// tools like gh.
type envStatus string

const (
	envOK    envStatus = "ok"
	envWarn  envStatus = "warn"
	envError envStatus = "error"
	envInfo  envStatus = "info"
)

type envCheck struct {
	Name    string
	Status  envStatus
	Message string
	Hint    string
}

// runEnvChecks runs the env-validation sweep. Returns a slice of
// envCheck records — ok/warn/error/info per resource. Used by the
// init command + the test suite.
func runEnvChecks() []envCheck {
	var out []envCheck

	// git is required (worktrees, pipelines).
	if path, err := exec.LookPath("git"); err == nil {
		out = append(out, envCheck{Name: "git", Status: envOK, Message: "git found at " + path})
	} else {
		out = append(out, envCheck{Name: "git", Status: envError, Message: "git not found on PATH",
			Hint: "install git (required to manage worktrees + pipelines)"})
	}

	// claude is needed to run any pipeline (warn only — user can install later).
	if path, err := exec.LookPath("claude"); err == nil {
		out = append(out, envCheck{Name: "claude", Status: envOK, Message: "claude found at " + path})
	} else {
		out = append(out, envCheck{Name: "claude", Status: envWarn, Message: "claude not found on PATH",
			Hint: "install Claude Code (required to run pipelines; the daemon starts without it but every run will fail)"})
	}

	// gh is optional (only for GitHub source).
	if path, err := exec.LookPath("gh"); err == nil {
		out = append(out, envCheck{Name: "gh", Status: envOK, Message: "gh found at " + path})
	} else {
		out = append(out, envCheck{Name: "gh", Status: envInfo, Message: "gh not found on PATH (only needed for GitHub source binding)"})
	}

	// ANTHROPIC_API_KEY is info-only (only needed for chat.provider=api).
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		out = append(out, envCheck{Name: "anthropic_api_key", Status: envOK, Message: "ANTHROPIC_API_KEY is set"})
	} else {
		out = append(out, envCheck{Name: "anthropic_api_key", Status: envInfo,
			Message: `ANTHROPIC_API_KEY not set (only needed if you change chat.provider to "api")`})
	}

	return out
}

// envGlyph maps envStatus to a Unicode glyph (TTY) or ASCII label
// (piped). Separate from doctor's glyph(doctor.Status) because init
// has its own status enum that includes "info" as a distinct terminal
// state. Reuses useASCII() from cmd_doctor.go for TTY detection.
func envGlyph(s envStatus) string {
	if useASCII() {
		switch s {
		case envOK:
			return "[OK]  "
		case envWarn:
			return "[WARN]"
		case envError:
			return "[ERR] "
		case envInfo:
			return "[--]  "
		}
		return "[?]   "
	}
	switch s {
	case envOK:
		return "✓"
	case envWarn:
		return "⚠"
	case envError:
		return "✗"
	case envInfo:
		return "ℹ"
	}
	return "?"
}
