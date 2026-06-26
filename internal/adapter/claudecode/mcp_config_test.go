package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteMCPConfig(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:    dir,
		HiveBinary: "/abs/path/to/hive",
		NotifySock: "/tmp/run-x/stage-0/verdict.sock",
		RunID:      "run-x",
		StageName:  "review",
		ToolName:   "hive_submit_review_verdict",
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	entry := servers["hive-stage"].(map[string]any)
	if entry["command"] != "/abs/path/to/hive" {
		t.Errorf("command=%v", entry["command"])
	}
	if filepath.Base(path) != "mcp.json" {
		t.Errorf("path=%s", path)
	}
}

func TestWriteMCPConfigIncludesScavenger(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:          dir,
		HiveBinary:       "/abs/path/to/hive",
		NotifySock:       "/tmp/run-x/stage-0/verdict.sock",
		RunID:            "run-x",
		StageName:        "review",
		ToolName:         "hive_submit_review_verdict",
		ScavengerEnabled: true,
		ScavengerBinary:  "/usr/local/bin/scavenger",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["hive-stage"]; !ok {
		t.Error("missing hive-stage entry")
	}
	scav, ok := servers["scavenger"].(map[string]any)
	if !ok {
		t.Fatal("missing scavenger entry")
	}
	if scav["command"] != "/usr/local/bin/scavenger" {
		t.Errorf("scavenger.command=%v", scav["command"])
	}
	args, _ := scav["args"].([]any)
	if len(args) != 1 || args[0] != "mcp-bridge" {
		t.Errorf("scavenger.args=%v want [mcp-bridge]", args)
	}
}

func TestWriteMCPConfigDefaultsScavengerBinary(t *testing.T) {
	dir := t.TempDir()
	path, _ := WriteMCPConfig(MCPConfigRequest{
		DestDir:          dir,
		HiveBinary:       "/abs/path/to/hive",
		NotifySock:       "/tmp/v.sock",
		RunID:            "r",
		StageName:        "review",
		ToolName:         "hive_submit_review_verdict",
		ScavengerEnabled: true,
		// ScavengerBinary intentionally empty -> default to "scavenger"
	})
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	scav := servers["scavenger"].(map[string]any)
	if scav["command"] != "scavenger" {
		t.Errorf("default scavenger.command=%v want \"scavenger\"", scav["command"])
	}
}

func TestWriteMCPConfigOmitsScavengerWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	path, _ := WriteMCPConfig(MCPConfigRequest{
		DestDir:    dir,
		HiveBinary: "/abs/path/to/hive",
		NotifySock: "/tmp/verdict.sock",
		RunID:      "r",
		StageName:  "review",
		ToolName:   "hive_submit_review_verdict",
		// ScavengerEnabled deliberately omitted (false)
	})
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if _, has := servers["scavenger"]; has {
		t.Error("scavenger entry should be absent when disabled")
	}
}

// Implement stage: no verdict tool (no NotifySock), but scavenger enabled.
// Must still produce a valid mcp.json with only the scavenger entry so the
// worker has scavenger's MCP tools (get_capsule, etc.) available.
func TestWriteMCPConfigScavengerOnlyNoVerdict(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:          dir,
		ScavengerEnabled: true,
		// no NotifySock / HiveBinary — implement-stage shape
	})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected mcp.json path, got empty")
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if _, has := servers["hive-stage"]; has {
		t.Error("hive-stage entry should be absent when NotifySock empty")
	}
	if _, has := servers["scavenger"]; !has {
		t.Error("scavenger entry should be present")
	}
}

// Neither verdict tool nor scavenger enabled -> no mcp.json written.
func TestWriteMCPConfigReturnsEmptyWhenNothingRequested(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{DestDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestWriteMCPConfigAddsPermServerWhenApprovalsEnabled(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:          dir,
		HiveBinary:       "/usr/bin/hive",
		RunID:            "run-1",
		StageName:        "implement",
		ApprovalsEnabled: true,
		// no NotifySock, no scavenger
	})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected mcp.json path when approvals enabled")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.MCPServers["hive_perm"]; !ok {
		t.Errorf("missing hive_perm server entry: %s", raw)
	}
}

func TestWriteMCPConfigDocTool(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:      dir,
		HiveBinary:   "/usr/bin/hive",
		RunID:        "run-1",
		StageName:    "document",
		DocToolName:  "hive_submit_documentation",
		DaemonSocket: "/tmp/d.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected a config path when DocToolName is set")
	}
	raw, _ := os.ReadFile(path)
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	srv, ok := cfg.MCPServers["hive_docs"]
	if !ok {
		t.Fatalf("hive_docs server missing; got %+v", cfg.MCPServers)
	}
	joined := strings.Join(srv.Args, " ")
	for _, want := range []string{"mcp-stage-server", "--doc-tool hive_submit_documentation", "--daemon-sock /tmp/d.sock", "--run-id run-1", "--stage document"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got %q", want, joined)
		}
	}
}

func TestWriteMCPConfigChatTools(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:      dir,
		HiveBinary:   "/usr/bin/hive",
		ChatTools:    true,
		DaemonSocket: "/tmp/d.sock",
		// no NotifySock / verdict / doc tool — chat-provider shape
	})
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected a config path when ChatTools is set")
	}
	raw, _ := os.ReadFile(path)
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	srv, ok := cfg.MCPServers["hive_chat"]
	if !ok {
		t.Fatalf("hive_chat server missing; got %+v", cfg.MCPServers)
	}
	if srv.Command != "/usr/bin/hive" {
		t.Errorf("hive_chat.command=%q want /usr/bin/hive", srv.Command)
	}
	joined := strings.Join(srv.Args, " ")
	for _, want := range []string{"mcp-stage-server", "--chat-tools", "--daemon-sock /tmp/d.sock"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got %q", want, joined)
		}
	}
}

func TestWriteMCPConfigChatToolsRequiresHiveBinary(t *testing.T) {
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:   t.TempDir(),
		ChatTools: true,
		// HiveBinary intentionally empty
	})
	if err == nil {
		t.Fatal("expected error when ChatTools set without HiveBinary")
	}
}

func TestWriteMCPConfigDocToolRequiresHiveBinary(t *testing.T) {
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:     t.TempDir(),
		DocToolName: "hive_submit_documentation",
		// HiveBinary intentionally empty
	})
	if err == nil {
		t.Fatal("expected error when DocToolName set without HiveBinary")
	}
}

func TestWriteMCPConfigChatHTTPMode(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:       dir,
		ChatTools:     true,
		UseHTTPFor:    map[string]bool{"chat": true},
		MCPBaseURL:    "http://127.0.0.1:54321",
		ChatSessionID: "sess-abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(raw), `"type": "http"`) {
		t.Errorf("HTTP mode mcp.json missing type:http: %s", raw)
	}
	if !strings.Contains(string(raw), `/mcp/chat/sess-abc`) {
		t.Errorf("mcp.json missing chat URL: %s", raw)
	}
	if strings.Contains(string(raw), "mcp-stage-server") {
		t.Errorf("HTTP mode should not emit stdio command: %s", raw)
	}
}

func TestWriteMCPConfigChatStdioMode(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:      dir,
		ChatTools:    true,
		HiveBinary:   "/usr/bin/hive",
		DaemonSocket: "/tmp/d.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(raw), `"type": "stdio"`) {
		t.Errorf("default mode should be stdio: %s", raw)
	}
	if !strings.Contains(string(raw), "mcp-stage-server") {
		t.Errorf("stdio mode missing the subprocess command: %s", raw)
	}
}

func TestWriteMCPConfigStageHTTPMode(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:    dir,
		HiveBinary: "/usr/bin/hive",
		NotifySock: "/tmp/v.sock",
		StageName:  "review",
		RunID:      "run-7",
		UseHTTPFor: map[string]bool{"stage": true},
		MCPBaseURL: "http://127.0.0.1:54321",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(raw), `/mcp/stage/run-7/review`) {
		t.Errorf("stage HTTP URL missing: %s", raw)
	}
	if strings.Contains(string(raw), "mcp-stage-server") {
		t.Errorf("HTTP mode should not emit stdio command: %s", raw)
	}
}

func TestWriteMCPConfigPermHTTPMode(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:          dir,
		ApprovalsEnabled: true,
		HiveBinary:       "/usr/bin/hive",
		DaemonSocket:     "/tmp/d.sock",
		StageName:        "implement",
		RunID:            "run-9",
		UseHTTPFor:       map[string]bool{"perm": true},
		MCPBaseURL:       "http://127.0.0.1:54321",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(raw), `/mcp/perm/run-9/implement`) {
		t.Errorf("perm HTTP URL missing: %s", raw)
	}
	if strings.Contains(string(raw), "mcp-stage-server") {
		t.Errorf("HTTP mode should not emit stdio command: %s", raw)
	}
}

func TestWriteMCPConfigHTTPRejectsMissingBaseURL(t *testing.T) {
	dir := t.TempDir()
	_, err := WriteMCPConfig(MCPConfigRequest{
		DestDir:       dir,
		ChatTools:     true,
		UseHTTPFor:    map[string]bool{"chat": true},
		ChatSessionID: "sess-abc",
		// MCPBaseURL deliberately omitted
	})
	if err == nil {
		t.Fatal("expected error when MCPBaseURL missing for HTTP mode")
	}
}
