package tui

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/rohilrs/Hive/internal/config"
)

// hiveDir resolves the daemon state directory. Mirrors cmd/hive's
// resolveHiveDir — HIVE_HOME env first, else $HOME/.hive. Kept
// private to the TUI package; the helper exists so tests can
// redirect via t.Setenv without trampling the user's real state.
func hiveDir() string {
	if h := os.Getenv("HIVE_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hive")
}

// projectConfigPath returns the path to a project's per-project TOML.
// Mirrors the path cmd_project_config writes to so the TUI reads
// exactly what the CLI helper wrote.
func projectConfigPath(slug string) string {
	return filepath.Join(hiveDir(), "projects", slug, "config.toml")
}

// repoConfigPath returns the path to a repo's per-repo config layer
// (~/.hive/repos/<RepoKey(repoPath)>/config.toml). Mirrors the path the
// `hive repo config` CLI resolves, using the SAME config.RepoKey so the TUI
// reads exactly what the CLI seeds. Returns "" for an empty repo path (the
// caller surfaces a "project has no repo path" state rather than a bogus path).
func repoConfigPath(repoPath string) string {
	key := config.RepoKey(repoPath)
	if key == "" {
		return ""
	}
	return filepath.Join(hiveDir(), "repos", key, "config.toml")
}

// readProjectAutoDispatch returns the explicit per-project setting or
// nil when the file/section/key is absent. Used to pre-fill the
// project create/edit modals' auto-dispatch tri-state field.
//
// Returns nil on any read or parse error — the field just defaults
// to "inherit" rather than blowing up the modal render.
func readProjectAutoDispatch(slug string) *bool {
	body, err := os.ReadFile(projectConfigPath(slug))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if _, derr := toml.Decode(string(body), &doc); derr != nil {
		return nil
	}
	sched, _ := doc["scheduler"].(map[string]any)
	if sched == nil {
		return nil
	}
	v, ok := sched["auto_dispatch"].(bool)
	if !ok {
		return nil
	}
	return &v
}

// setProjectAutoDispatch writes the project's auto_dispatch setting.
// nil clears the key (and removes the [scheduler] section if it
// becomes empty); *true / *false writes the explicit value. Atomic
// write (.tmp + rename) matches cmd_project_config's safety. Other
// TOML keys/sections are preserved across writes.
//
// On clear: if the file becomes entirely empty (no other sections,
// no other [scheduler] keys), the file is removed so the project
// genuinely "inherits global" with no leftover TOML clutter.
func setProjectAutoDispatch(slug string, v *bool) error {
	cfgPath := projectConfigPath(slug)
	existing := map[string]any{}
	if body, err := os.ReadFile(cfgPath); err == nil {
		if _, derr := toml.Decode(string(body), &existing); derr != nil {
			return derr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	sched, _ := existing["scheduler"].(map[string]any)
	if sched == nil {
		sched = map[string]any{}
	}
	if v == nil {
		delete(sched, "auto_dispatch")
		if len(sched) == 0 {
			delete(existing, "scheduler")
		} else {
			existing["scheduler"] = sched
		}
	} else {
		sched["auto_dispatch"] = *v
		existing["scheduler"] = sched
	}
	// If clear left the doc empty AND there's nothing else to keep,
	// remove the file entirely. Operator's "inherit" really means
	// "no per-project state".
	if v == nil && len(existing) == 0 {
		if err := os.Remove(cfgPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	tmpPath := cfgPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(existing); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, cfgPath)
}
