package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/scavenger"
)

// TestAdapterUsesHiveOwnedPluginDirWhenRunDirSet verifies that when
// scavenger is enabled AND RunDir is set on the StageRequest, the
// claude subprocess gets --plugin-dir pointing at the Hive-owned
// patched copy (NOT the original scavenger plugin dir).
func TestAdapterUsesHiveOwnedPluginDirWhenRunDirSet(t *testing.T) {
	scavClient := scavenger.NewClient(scavenger.Config{Binary: "scavenger"})

	fakeClaude := buildFakeClaude(t)
	stageDir := t.TempDir()
	runDir := t.TempDir()
	cwd := t.TempDir()
	// Per-run InstallPlugin output lives in the worktree (cwd).
	fixtureScavengerPlugin(t, filepath.Join(cwd, ".scavenger"))

	a := New(Config{
		Binary:           fakeClaude,
		HiveBinary:       "hive",
		RealHome:         makeFakeRealHome(t),
		Scavenger:        scavClient,
		ScavengerEnabled: true,
		ScavengerBinary:  "scavenger",
	})

	_, _ = a.RunStage(context.Background(), adapter.StageRequest{
		RunID:      "r-test",
		StageName:  "implement",
		Iter:       0,
		Model:      "sonnet",
		UserPrompt: "noop",
		Timeout:    5 * time.Second,
		Cwd:        cwd,
		StageDir:   stageDir,
		RunDir:     runDir,
	})

	argv := readFakeClaudeArgv(t, cwd)

	wantPluginDir := filepath.Join(runDir, "scavenger-plugin")
	if !argvHasFlagWithValue(argv, "--plugin-dir", wantPluginDir) {
		t.Errorf("--plugin-dir not pointing at hive-owned copy\nargv=%v\nwant=%s", argv, wantPluginDir)
	}

	originalSrc := filepath.Join(cwd, ".scavenger", "claude-plugin")
	if argvHasFlagWithValue(argv, "--plugin-dir", originalSrc) {
		t.Errorf("--plugin-dir still pointing at source (should be hive-owned copy): %s", originalSrc)
	}

	hooksPath := filepath.Join(wantPluginDir, "hooks", "hooks.json")
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read patched hooks.json: %v", err)
	}
	var doc struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc.Hooks["SessionEnd"]; present {
		t.Errorf("SessionEnd present in patched hooks.json")
	}
}

func TestAdapterFallsBackToSourceWhenRunDirEmpty(t *testing.T) {
	scavClient := scavenger.NewClient(scavenger.Config{Binary: "scavenger"})

	fakeClaude := buildFakeClaude(t)
	cwd := t.TempDir()
	fixtureScavengerPlugin(t, filepath.Join(cwd, ".scavenger"))
	a := New(Config{
		Binary:           fakeClaude,
		HiveBinary:       "hive",
		RealHome:         makeFakeRealHome(t),
		Scavenger:        scavClient,
		ScavengerEnabled: true,
		ScavengerBinary:  "scavenger",
	})

	_, _ = a.RunStage(context.Background(), adapter.StageRequest{
		RunID:      "r-test",
		StageName:  "implement",
		UserPrompt: "noop",
		Timeout:    5 * time.Second,
		Cwd:        cwd,
		StageDir:   t.TempDir(),
		// RunDir intentionally empty
	})
	argv := readFakeClaudeArgv(t, cwd)

	wantSrc := filepath.Join(cwd, ".scavenger", "claude-plugin")
	if !argvHasFlagWithValue(argv, "--plugin-dir", wantSrc) {
		t.Errorf("fallback should point --plugin-dir at source\nargv=%v\nwant=%s", argv, wantSrc)
	}
}

func argvHasFlagWithValue(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

func readFakeClaudeArgv(t *testing.T, cwd string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".fake-claude-argv.json"))
	if err != nil {
		t.Fatalf("read fake-claude argv: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatalf("unmarshal argv: %v", err)
	}
	return argv
}
