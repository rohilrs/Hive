package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectTOML seeds a per-project config file under
// HIVE_HOME/projects/<slug>/config.toml for the read/set tests.
// Mirrors what cmd_project_config writes.
func writeProjectTOML(t *testing.T, slug, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HIVE_HOME"), "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadProjectAutoDispatchAbsentReturnsNil(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	if got := readProjectAutoDispatch("nope"); got != nil {
		t.Errorf("expected nil for absent file, got %v", *got)
	}
}

func TestReadProjectAutoDispatchTrue(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive", "[scheduler]\nauto_dispatch = true\n")
	got := readProjectAutoDispatch("hive")
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if *got != true {
		t.Errorf("expected true, got %v", *got)
	}
}

func TestReadProjectAutoDispatchFalse(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive", "[scheduler]\nauto_dispatch = false\n")
	got := readProjectAutoDispatch("hive")
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if *got != false {
		t.Errorf("expected false, got %v", *got)
	}
}

func TestReadProjectAutoDispatchMissingSectionReturnsNil(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive", "[other]\nfoo = 1\n")
	if got := readProjectAutoDispatch("hive"); got != nil {
		t.Errorf("expected nil with no [scheduler] section, got %v", *got)
	}
}

func ptrBool(v bool) *bool { return &v }

func TestSetProjectAutoDispatchCreatesFile(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	if err := setProjectAutoDispatch("hive", ptrBool(true)); err != nil {
		t.Fatal(err)
	}
	got := readProjectAutoDispatch("hive")
	if got == nil || *got != true {
		t.Errorf("expected file to contain true, got %v", got)
	}
}

func TestSetProjectAutoDispatchOverwritesExisting(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive", "[scheduler]\nauto_dispatch = true\n")
	if err := setProjectAutoDispatch("hive", ptrBool(false)); err != nil {
		t.Fatal(err)
	}
	got := readProjectAutoDispatch("hive")
	if got == nil || *got != false {
		t.Errorf("expected overwrite to false, got %v", got)
	}
}

func TestSetProjectAutoDispatchClearRemovesKey(t *testing.T) {
	// nil clears the per-project override. With no other content in the
	// file, the whole file should be removed so the project really does
	// inherit global with no leftover TOML clutter.
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive", "[scheduler]\nauto_dispatch = true\n")
	if err := setProjectAutoDispatch("hive", nil); err != nil {
		t.Fatal(err)
	}
	if got := readProjectAutoDispatch("hive"); got != nil {
		t.Errorf("expected nil after clear, got %v", *got)
	}
	if _, err := os.Stat(projectConfigPath("hive")); !os.IsNotExist(err) {
		t.Errorf("expected file to be removed when clear leaves doc empty, stat err = %v", err)
	}
}

func TestSetProjectAutoDispatchClearPreservesOtherKeys(t *testing.T) {
	// Clear must not nuke unrelated [scheduler] keys or sibling
	// sections — only the auto_dispatch key goes away.
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive",
		"[scheduler]\nauto_dispatch = true\nsome_other_key = \"keep me\"\n[other]\nfoo = \"bar\"\n")
	if err := setProjectAutoDispatch("hive", nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(projectConfigPath("hive"))
	if err != nil {
		t.Fatalf("expected file preserved with other keys, err=%v", err)
	}
	if !strings.Contains(string(body), "keep me") {
		t.Errorf("clear lost some_other_key: %s", body)
	}
	if !strings.Contains(string(body), "foo") {
		t.Errorf("clear lost [other] section: %s", body)
	}
	if strings.Contains(string(body), "auto_dispatch") {
		t.Errorf("clear left auto_dispatch behind: %s", body)
	}
}

func TestSetProjectAutoDispatchPreservesOtherKeysOnWrite(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	writeProjectTOML(t, "hive",
		"[scheduler]\nauto_dispatch = true\nsome_other_key = \"keep me\"\n[other]\nfoo = \"bar\"\n")
	if err := setProjectAutoDispatch("hive", ptrBool(false)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(projectConfigPath("hive"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "keep me") {
		t.Errorf("write lost some_other_key: %s", body)
	}
	if !strings.Contains(string(body), "foo") {
		t.Errorf("write lost [other] section: %s", body)
	}
}

func TestSetProjectAutoDispatchNoLeftoverTmpFile(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	if err := setProjectAutoDispatch("hive", ptrBool(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(projectConfigPath("hive") + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected .tmp file to be renamed away; stat err = %v", err)
	}
}
