package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectConfigWritesAutoDispatchTrue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)

	var stdout bytes.Buffer
	cmd := newProjectConfigCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"myslug", "--auto-dispatch=true"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	cfgPath := filepath.Join(home, "projects", "myslug", "config.toml")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "auto_dispatch = true") {
		t.Errorf("config doesn't contain auto_dispatch = true; got:\n%s", body)
	}
	if !strings.Contains(string(body), "[scheduler]") {
		t.Errorf("config doesn't contain [scheduler] section; got:\n%s", body)
	}
}

func TestProjectConfigFlipsAutoDispatchFalse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	cfgDir := filepath.Join(home, "projects", "myslug")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("[scheduler]\nauto_dispatch = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newProjectConfigCmd()
	cmd.SetArgs([]string{"myslug", "--auto-dispatch=false"})
	cmd.SetOut(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(filepath.Join(cfgDir, "config.toml"))
	if !strings.Contains(string(body), "auto_dispatch = false") {
		t.Errorf("auto_dispatch not flipped; got:\n%s", body)
	}
}

func TestProjectConfigRequiresSlug(t *testing.T) {
	cmd := newProjectConfigCmd()
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error on missing slug arg")
	}
}

func TestProjectConfigReadsCurrentWhenNoFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	cfgDir := filepath.Join(home, "projects", "myslug")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("[scheduler]\nauto_dispatch = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	cmd := newProjectConfigCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"myslug"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "auto_dispatch = true") {
		t.Errorf("expected current config in stdout; got:\n%s", stdout.String())
	}
}

func TestProjectConfigSentinelOnMissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)

	var stdout bytes.Buffer
	cmd := newProjectConfigCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"ghost"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "no per-project config") {
		t.Errorf("expected sentinel message; got:\n%s", stdout.String())
	}
}
