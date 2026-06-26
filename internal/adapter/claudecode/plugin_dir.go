package claudecode

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WritePluginDir copies the scavenger Claude Code plugin tree rooted at
// srcDir into <destDir>/scavenger-plugin/, then patches hooks/hooks.json
// to remove the SessionEnd hook entry. The returned path is the new
// plugin root, suitable for passing to `claude --plugin-dir`.
//
// The SessionEnd strip is what makes the scavenger daemon survive across
// Hive stages (each stage being one Claude Code session). Hive's
// scavenger lifecycle client (Phase 2a) handles daemon teardown at run
// boundary instead.
//
// Idempotent: re-calling with the same args overwrites the destination
// with deterministic content.
func WritePluginDir(srcDir, destDir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(srcDir)
	if err != nil {
		return "", fmt.Errorf("scavenger plugin source: %w", err)
	}
	srcDir = resolved
	dest := filepath.Join(destDir, "scavenger-plugin")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("mkdir plugin dest: %w", err)
	}

	if err := filepath.Walk(srcDir, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, srcPath)
		if err != nil {
			return err
		}
		destPath := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}
		return copyFile(srcPath, destPath, info.Mode())
	}); err != nil {
		return "", fmt.Errorf("copy plugin tree: %w", err)
	}

	if err := stripSessionEndHook(filepath.Join(dest, "hooks", "hooks.json")); err != nil {
		return "", fmt.Errorf("patch hooks.json: %w", err)
	}
	return dest, nil
}

func copyFile(src, dest string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func stripSessionEndHook(hooksPath string) error {
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		// Missing hooks.json is not an error — plugin may not register
		// hooks. In practice scavenger always does, so this branch is
		// defensive only.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("unmarshal hooks.json: %w", err)
	}
	if hooksRaw, present := doc["hooks"]; present {
		hooks, ok := hooksRaw.(map[string]any)
		if !ok {
			return fmt.Errorf("hooks.json: 'hooks' field is %T, want object", hooksRaw)
		}
		delete(hooks, "SessionEnd")
	}
	patched, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal hooks.json: %w", err)
	}
	return os.WriteFile(hooksPath, patched, 0o644)
}
