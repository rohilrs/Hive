package main

import (
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/adapter/claudecode"
	"github.com/rohilrs/Hive/internal/config"
)

// TestBuildPlannerCCAgentUsesPlannerSystemPromptAndTools asserts the
// composition-root CC planner builder wires:
//   - the planner system prompt (so the model knows it's planning and which
//     project + cwd it's planning for)
//   - the PlannerToolNames palette (NOT the regular ChatToolNames)
//   - the chat-tools MCP server mode "plan" so the subprocess advertises
//     PlannerToolNames in its tools/list response
//
// Phase 8.A T6b.
func TestBuildPlannerCCAgentUsesPlannerSystemPromptAndTools(t *testing.T) {
	cfg := config.Default()
	hiveBin := "/tmp/hive"
	realHome := t.TempDir()
	hiveDir := t.TempDir()

	a := buildPlannerCCAgent(cfg, hiveBin, realHome, hiveDir, "myapp", "/repo/myapp", nil)
	cc, ok := a.(*claudecode.ChatAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *claudecode.ChatAgent", a)
	}

	prompt := cc.SystemPromptForTest()
	if !strings.Contains(prompt, "Hive Roadmap Planner") {
		t.Errorf("planner system prompt missing 'Hive Roadmap Planner' header; got: %s", prompt)
	}
	if !strings.Contains(prompt, "myapp") {
		t.Errorf("planner system prompt missing project slug 'myapp'; got: %s", prompt)
	}
	if !strings.Contains(prompt, "/repo/myapp") {
		t.Errorf("planner system prompt missing cwd '/repo/myapp'; got: %s", prompt)
	}

	tools := cc.AllowedToolsForTest()
	wantTools := map[string]bool{
		"hive_list_specs":    true,
		"hive_read_doc":      true,
		"hive_save_roadmap":  true,
		"hive_save_spec":     true,
		"hive_search_code":   true,
		"hive_query_capsule": true,
	}
	if len(tools) != len(wantTools) {
		t.Errorf("planner agent advertises %d tools, want %d: got %v", len(tools), len(wantTools), tools)
	}
	for _, name := range tools {
		if !wantTools[name] {
			t.Errorf("planner agent advertises unexpected tool %q (not in PlannerToolNames)", name)
		}
		delete(wantTools, name)
	}
	for name := range wantTools {
		t.Errorf("planner agent missing expected tool %q", name)
	}

	if mode := cc.MCPModeForTest(); mode != "plan" {
		t.Errorf("planner agent mcpMode=%q, want \"plan\"", mode)
	}
}

// TestChatToolsForModeChatVsPlan covers the mode dispatcher used by
// runChatToolsServer (Phase 8.A T6b). Default ("" or "chat") returns the
// regular chat tools; "plan" returns the planner tools.
func TestChatToolsForModeChatVsPlan(t *testing.T) {
	chatNames := chatToolsForMode("")
	if len(chatNames) != len(claudecode.ChatToolNames) {
		t.Errorf("chat mode count=%d, want %d", len(chatNames), len(claudecode.ChatToolNames))
	}
	chatNames2 := chatToolsForMode("chat")
	if len(chatNames2) != len(claudecode.ChatToolNames) {
		t.Errorf("explicit chat mode count=%d, want %d", len(chatNames2), len(claudecode.ChatToolNames))
	}
	planNames := chatToolsForMode("plan")
	if len(planNames) != len(claudecode.PlannerToolNames) {
		t.Errorf("plan mode count=%d, want %d", len(planNames), len(claudecode.PlannerToolNames))
	}
	// Spot-check: plan mode includes the expected planner tools.
	want := map[string]bool{
		"hive_list_specs": true, "hive_read_doc": true,
		"hive_save_roadmap": true, "hive_save_spec": true,
		"hive_search_code": true, "hive_query_capsule": true,
	}
	for _, n := range planNames {
		if !want[n] {
			t.Errorf("plan mode advertises unexpected tool %q", n)
		}
	}
	// Unknown mode degrades to chat.
	unknownNames := chatToolsForMode("garbage")
	if len(unknownNames) != len(claudecode.ChatToolNames) {
		t.Errorf("unknown mode should degrade to chat tools; got %d, want %d", len(unknownNames), len(claudecode.ChatToolNames))
	}
}
