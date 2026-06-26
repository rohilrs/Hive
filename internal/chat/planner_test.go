package chat

import (
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/anthropic"
)

func TestPlannerSystemPromptIncludesKeyDirectives(t *testing.T) {
	p := PlannerSystemPrompt("my-app", "/tmp/repo")
	for _, marker := range []string{
		"Hive Roadmap Planner",
		"hive_list_specs",
		"one question at a time",
		"my-app",
		"/tmp/repo",
	} {
		if !strings.Contains(p, marker) {
			t.Errorf("system prompt missing marker %q", marker)
		}
	}
}

func TestNewPlannerRegistryAdvertisesPlannerTools(t *testing.T) {
	reg := NewPlannerRegistry("/tmp/repo", nil, "", nil) // nil readRegistry → builds fresh
	for _, name := range []string{"hive_list_specs", "hive_read_doc", "hive_save_roadmap", "hive_save_spec"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("planner registry missing tool %q", name)
		}
	}
	save, ok := reg.Get("hive_save_roadmap")
	if !ok || !save.Mutating {
		t.Errorf("hive_save_roadmap should be Mutating=true")
	}
	spec, ok := reg.Get("hive_save_spec")
	if !ok || !spec.Mutating {
		t.Errorf("hive_save_spec should be Mutating=true")
	}
	read, ok := reg.Get("hive_list_specs")
	if !ok || read.Mutating {
		t.Errorf("hive_list_specs should be Mutating=false")
	}
}

func TestNewPlannerRegistryComposesExistingChatReads(t *testing.T) {
	base := NewRegistry()
	base.Register(Tool{Def: anthropic.ToolDef{Name: "hive_list_tasks"}, Mutating: false, Handler: nil})
	reg := NewPlannerRegistry("/tmp/repo", base, "", nil)
	if _, ok := reg.Get("hive_list_tasks"); !ok {
		t.Fatal("planner registry should inherit hive_list_tasks from base")
	}
}

func TestPlannerPromptMentionsExistingWork(t *testing.T) {
	p := PlannerSystemPrompt("demo", "/repo")
	if !strings.Contains(p, "Existing") || !strings.Contains(p, "> Existing:") {
		t.Errorf("planner prompt should instruct existing-work annotation:\n%s", p)
	}
}
