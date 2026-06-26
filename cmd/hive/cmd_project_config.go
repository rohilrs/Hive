package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

// newProjectConfigCmd returns the `hive project config <slug>` command.
//
// With --auto-dispatch=true|false the command edits the per-project TOML at
// <HiveDir>/projects/<slug>/config.toml. With no flag the command prints the
// current file contents (or a sentinel when absent).
//
// This is the operator-friendly companion to the T1 scheduler change: the
// scheduler reads the same path on every dispatch tick to resolve per-project
// auto_dispatch overrides. Avoids manual `cat`/`vim` of the TOML file.
func newProjectConfigCmd() *cobra.Command {
	var autoDispatch string
	cmd := &cobra.Command{
		Use:   "config <slug>",
		Short: "Read or edit a project's per-project config overrides",
		Long: `Read or edit ~/.hive/projects/<slug>/config.toml.

With --auto-dispatch=true|false, sets [scheduler] auto_dispatch for
this project and writes the file back atomically. The per-project
value overrides the global [scheduler] auto_dispatch setting for
tasks belonging to this project.

With no flag, prints the current file contents (or "no per-project
config" when absent -- global defaults apply).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectConfig(cmd, args[0], autoDispatch)
		},
	}
	cmd.Flags().StringVar(&autoDispatch, "auto-dispatch", "", "set [scheduler] auto_dispatch (true|false)")
	return cmd
}

func runProjectConfig(cmd *cobra.Command, slug, autoDispatch string) error {
	hiveDir, err := resolveHiveDir()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(hiveDir, "projects", slug, "config.toml")

	// No-flag mode: print current state. Sentinel on absent file so the
	// operator knows "global defaults apply" rather than mistaking the
	// empty output for an empty config.
	if autoDispatch == "" {
		body, err := os.ReadFile(cfgPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(cmd.OutOrStdout(), "(no per-project config at %s; using global defaults)\n", cfgPath)
				return nil
			}
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s", body)
		return nil
	}

	// Validate flag value up-front so we don't half-create directories
	// and bail before the rename.
	var newVal bool
	switch autoDispatch {
	case "true":
		newVal = true
	case "false":
		newVal = false
	default:
		return fmt.Errorf("--auto-dispatch must be true or false, got %q", autoDispatch)
	}

	// Read existing file, mutate in-memory, atomic write back. Preserves
	// any other fields the user may have hand-edited under [scheduler] or
	// other sections.
	existing := map[string]any{}
	body, err := os.ReadFile(cfgPath)
	if err == nil {
		if _, derr := toml.Decode(string(body), &existing); derr != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, derr)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	sched, _ := existing["scheduler"].(map[string]any)
	if sched == nil {
		sched = map[string]any{}
	}
	sched["auto_dispatch"] = newVal
	existing["scheduler"] = sched

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	tmpPath := cfgPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(existing); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated %s\n", cfgPath)
	return nil
}

// resolveHiveDir returns the daemon's state directory. Honors the
// HIVE_HOME env var (used by tests and validation smokes); else
// defaults to $HOME/.hive.
//
// Mirrors the cwd-resolution pattern from hiveDirForDoctor /
// hiveDirForInit, but with HIVE_HOME support so tests can redirect
// without trampling the user's real ~/.hive.
func resolveHiveDir() (string, error) {
	if h := os.Getenv("HIVE_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hive"), nil
}
