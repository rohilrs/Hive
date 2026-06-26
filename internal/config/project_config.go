package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// SetProjectScheduler merges the given keys into the [scheduler] table of a
// project's per-project config at <hiveDir>/projects/<slug>/config.toml,
// preserving all other keys/sections. Creates the file and parent dir if
// absent. Atomic write (.tmp + rename). Mirrors the TUI's setProjectAutoDispatch
// safety but is daemon-usable (no internal/tui dependency).
func SetProjectScheduler(hiveDir, slug string, keys map[string]any) error {
	cfgPath := filepath.Join(hiveDir, "projects", slug, "config.toml")
	doc := map[string]any{}
	if body, err := os.ReadFile(cfgPath); err == nil {
		if _, derr := toml.Decode(string(body), &doc); derr != nil {
			return derr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	sched, _ := doc["scheduler"].(map[string]any)
	if sched == nil {
		sched = map[string]any{}
	}
	for k, v := range keys {
		sched[k] = v
	}
	doc["scheduler"] = sched

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(doc); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, cfgPath)
}

// SetProjectIntegration merges keys into the [integration] table of the
// project's per-project config overlay, preserving other keys. Mirror of
// SetProjectScheduler.
func SetProjectIntegration(hiveDir, slug string, keys map[string]any) error {
	cfgPath := filepath.Join(hiveDir, "projects", slug, "config.toml")
	doc := map[string]any{}
	if body, err := os.ReadFile(cfgPath); err == nil {
		if _, derr := toml.Decode(string(body), &doc); derr != nil {
			return derr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	integ, _ := doc["integration"].(map[string]any)
	if integ == nil {
		integ = map[string]any{}
	}
	for k, v := range keys {
		integ[k] = v
	}
	doc["integration"] = integ

	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(doc); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, cfgPath)
}
