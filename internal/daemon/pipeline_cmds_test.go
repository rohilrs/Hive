package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/store"
)

func TestRunCommandsForProject(t *testing.T) {
	d := newTestDaemon(t)
	dir := filepath.Join(d.cfg.HiveDir, "projects", "tsapp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[pipelines.build]\ntest_command = \"npm test\"\nvalidate_command = \"npm run build\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := d.runCommandsForProject("tsapp")
	if rc == nil {
		t.Fatal("runCommandsForProject returned nil")
	}
	if rc.Test != "npm test" || rc.Validate != "npm run build" {
		t.Errorf("per-project build cmds = (%q,%q), want npm", rc.Test, rc.Validate)
	}
	rcDefault := d.runCommandsForProject("nope")
	if rcDefault == nil {
		t.Fatal("nil for project without override")
	}
	if rcDefault.Test == "npm test" {
		t.Errorf("project without override picked up another project's command")
	}
}

func TestRunCommandsForProject_RepoLayerApplies(t *testing.T) {
	d := newTestDaemon(t)
	repo := "/repo/acme-ci"
	slug := "acme"
	if err := d.store.InsertProject(context.Background(), &store.Project{
		ID: slug, Slug: slug, Name: "A", Status: "active", RepoPath: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	// Write a repo-layer config (under the dir effectiveConfigForProject reads:
	// d.cfg.HiveDir) with a distinctive build test_command.
	key := config.RepoKey(repo)
	p := filepath.Join(d.cfg.HiveDir, "repos", key, "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("[pipelines.build]\ntest_command = \"repo-pnpm-test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rc := d.runCommandsForProject(slug)
	if rc.Test != "repo-pnpm-test" {
		t.Errorf("repo-layer test_command must surface: %q", rc.Test)
	}

	// A project-layer value still wins over the repo layer.
	pp := filepath.Join(d.cfg.HiveDir, "projects", slug, "config.toml")
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte("[pipelines.build]\ntest_command = \"project-test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc2 := d.runCommandsForProject(slug); rc2.Test != "project-test" {
		t.Errorf("project layer must override repo: %q", rc2.Test)
	}
}
