package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fixtureScavengerPlugin builds a minimal scavenger plugin tree mirroring
// the real layout (~/.scavenger-state/<project>/claude-plugin/) in dir.
// Returns the plugin root path.
func fixtureScavengerPlugin(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, "claude-plugin")
	if err := os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"description":"AST dep graph","name":"scavenger","version":"0.2.0"}`
	if err := os.WriteFile(filepath.Join(root, ".claude-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := `{
  "description": "Scavenger hooks",
  "hooks": {
    "PostToolUse": [{"hooks":[{"command":"scavenger hook post-tool-use","type":"command"}],"matcher":"Write|Edit|MultiEdit"}],
    "PreToolUse":  [{"hooks":[{"command":"scavenger hook pre-tool-use","type":"command"}],"matcher":"Read"}],
    "SessionStart":[{"hooks":[{"command":"scavenger hook session-start","type":"command"}]}],
    "SessionEnd":  [{"hooks":[{"command":"scavenger hook session-end","type":"command"}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(root, "hooks", "hooks.json"), []byte(hooks), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWritePluginDirCopiesTreeAndStripsSessionEnd(t *testing.T) {
	src := fixtureScavengerPlugin(t, t.TempDir())
	runDir := t.TempDir()

	got, err := WritePluginDir(src, runDir)
	if err != nil {
		t.Fatalf("WritePluginDir: %v", err)
	}
	wantPath := filepath.Join(runDir, "scavenger-plugin")
	if got != wantPath {
		t.Errorf("returned path=%q want=%q", got, wantPath)
	}

	// plugin.json copied byte-identical.
	manifest, err := os.ReadFile(filepath.Join(got, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != `{"description":"AST dep graph","name":"scavenger","version":"0.2.0"}` {
		t.Errorf("plugin.json content unexpected: %s", manifest)
	}

	// hooks.json copied, but with SessionEnd stripped.
	raw, err := os.ReadFile(filepath.Join(got, "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hooks struct {
		Description string                     `json:"description"`
		Hooks       map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &hooks); err != nil {
		t.Fatalf("hooks.json unmarshal: %v", err)
	}
	if _, present := hooks.Hooks["SessionEnd"]; present {
		t.Errorf("SessionEnd should be stripped, but is present in %v", hooks.Hooks)
	}
	for _, want := range []string{"PostToolUse", "PreToolUse", "SessionStart"} {
		if _, present := hooks.Hooks[want]; !present {
			t.Errorf("hook %q should be preserved but is missing", want)
		}
	}
	if hooks.Description != "Scavenger hooks" {
		t.Errorf("description=%q want preserved", hooks.Description)
	}
}

func TestWritePluginDirIdempotent(t *testing.T) {
	src := fixtureScavengerPlugin(t, t.TempDir())
	runDir := t.TempDir()

	first, err := WritePluginDir(src, runDir)
	if err != nil {
		t.Fatalf("first WritePluginDir: %v", err)
	}
	second, err := WritePluginDir(src, runDir)
	if err != nil {
		t.Fatalf("second WritePluginDir: %v", err)
	}
	if first != second {
		t.Errorf("path drift across calls: %q vs %q", first, second)
	}

	// hooks.json content stable across calls.
	a, _ := os.ReadFile(filepath.Join(first, "hooks", "hooks.json"))
	b, _ := os.ReadFile(filepath.Join(second, "hooks", "hooks.json"))
	if string(a) != string(b) {
		t.Errorf("hooks.json differs across idempotent calls")
	}
}

func TestWritePluginDirMissingSourceReturnsError(t *testing.T) {
	runDir := t.TempDir()
	_, err := WritePluginDir(filepath.Join(t.TempDir(), "does-not-exist"), runDir)
	if err == nil {
		t.Errorf("expected error for missing source, got nil")
	}
}
