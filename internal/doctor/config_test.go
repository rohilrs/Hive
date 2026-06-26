package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigGlobalParsesOKWithNoFile(t *testing.T) {
	hiveDir := t.TempDir()
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.global_parses")
	if c.Status != StatusOK {
		t.Errorf("no config file: status=%s, want ok", c.Status)
	}
}

func TestConfigGlobalParsesErrorOnInvalidToml(t *testing.T) {
	hiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hiveDir, "config.toml"), []byte("this is = not [ valid toml"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.global_parses")
	if c.Status != StatusError {
		t.Errorf("invalid toml: status=%s, want error", c.Status)
	}
}

func TestConfigChatProviderRecognized(t *testing.T) {
	hiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hiveDir, "config.toml"), []byte(`[chat]
provider = "claude-code"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.chat_provider")
	if c.Status != StatusOK {
		t.Errorf("known provider: status=%s, want ok", c.Status)
	}
}

func TestConfigChatProviderUnknownIsError(t *testing.T) {
	hiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hiveDir, "config.toml"), []byte(`[chat]
provider = "frobozz"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.chat_provider")
	if c.Status != StatusError {
		t.Errorf("unknown provider: status=%s, want error", c.Status)
	}
}

func TestConfigAPIKeyRequiredWhenProviderIsAPI(t *testing.T) {
	hiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hiveDir, "config.toml"), []byte(`[chat]
provider = "api"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.api_key")
	if c.Status != StatusError {
		t.Errorf("provider=api without key: status=%s, want error", c.Status)
	}
}

func TestConfigAPIKeySkippedWhenProviderIsClaudeCode(t *testing.T) {
	hiveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hiveDir, "config.toml"), []byte(`[chat]
provider = "claude-code"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.api_key")
	if c.Status != StatusSkip {
		t.Errorf("provider=claude-code: status=%s, want skip", c.Status)
	}
}

// TestConfigProjectOverridesParseFailIsWarn covers C4's positive
// failure path: a per-project config.toml that fails to parse should
// surface as a warn (not silently OK).
func TestConfigProjectOverridesParseFailIsWarn(t *testing.T) {
	hiveDir := t.TempDir()
	projDir := filepath.Join(hiveDir, "projects", "frobozz")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "config.toml"), []byte("garbage = [unclosed"), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	checks := runConfigChecks(context.Background(), hiveDir, &stubRPCClient{})
	c := findCheck(t, checks, "config.project_overrides")
	if c.Status != StatusWarn {
		t.Errorf("invalid project toml: status=%s, want warn; msg=%q", c.Status, c.Message)
	}
}
