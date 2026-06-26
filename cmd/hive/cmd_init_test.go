package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/config"
)

func TestInitWritesConfigToFreshDir(t *testing.T) {
	dir := t.TempDir()
	if err := runInit(dir, false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.HasPrefix(string(got), "# ~/.hive/config.toml") {
		t.Errorf("template prefix missing; got first 60 chars: %q", string(got[:minInt(60, len(got))]))
	}
	if !strings.Contains(string(got), config.DefaultConfigTOML[:50]) {
		t.Errorf("config.toml content doesn't match DefaultConfigTOML")
	}
}

func TestInitRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	sentinel := "# SENTINEL — pre-existing content\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("pre-create config: %v", err)
	}
	err := runInit(dir, false)
	if err == nil {
		t.Fatal("expected error for existing config without --force")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if string(got) != sentinel {
		t.Errorf("config.toml was modified; want sentinel intact")
	}
}

func TestInitForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	sentinel := "# SENTINEL — pre-existing content\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(sentinel), 0o600); err != nil {
		t.Fatalf("pre-create config: %v", err)
	}
	if err := runInit(dir, true); err != nil {
		t.Fatalf("runInit --force: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(got), "SENTINEL") {
		t.Errorf("--force did not overwrite the sentinel")
	}
}

func TestInitConfigPermsAre600(t *testing.T) {
	dir := t.TempDir()
	if err := runInit(dir, false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm=%o, want 0600", perm)
	}
}

func TestRunEnvChecksGitMissingIsError(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	checks := runEnvChecks()
	gotErr := false
	for _, c := range checks {
		if c.Name == "git" && c.Status == envError {
			gotErr = true
		}
	}
	if !gotErr {
		t.Errorf("expected git=error when PATH excludes it; got %+v", checks)
	}
}

func TestRunEnvChecksClaudeMissingIsWarn(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	checks := runEnvChecks()
	gotWarn := false
	for _, c := range checks {
		if c.Name == "claude" && c.Status == envWarn {
			gotWarn = true
		}
	}
	if !gotWarn {
		t.Errorf("expected claude=warn when PATH excludes it; got %+v", checks)
	}
}

// minInt is a local helper for tests on Go versions before built-in min.
// Named to avoid colliding with any other "min" in package main.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
