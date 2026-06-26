package modals

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNewTaskPrefilledSlug(t *testing.T) {
	m := NewNewTask("hive").(*NewTask)
	if m.slug.Value() != "hive" {
		t.Errorf("slug not prefilled: %q", m.slug.Value())
	}
	if m.focusIdx != 1 {
		t.Errorf("focusIdx=%d want 1 (title field)", m.focusIdx)
	}
}

func TestNewTaskRequiresSlugAndTitle(t *testing.T) {
	m := NewNewTask("").(*NewTask)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !strings.Contains(m.errMsg, "required") {
		t.Errorf("errMsg=%q want 'required'", m.errMsg)
	}
}

func TestNewTaskSubmitsValidForm(t *testing.T) {
	m := NewNewTask("hive").(*NewTask)
	m.title.SetValue("implement foo")
	m.body.SetValue("line one\nline two")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("submit should emit cmd")
	}
	req := cmd().(SubmitRequest)
	if req.Params["project_slug"] != "hive" {
		t.Errorf("slug=%v", req.Params["project_slug"])
	}
	if req.Params["title"] != "implement foo" {
		t.Errorf("title=%v", req.Params["title"])
	}
	if req.Params["body"] != "line one\nline two" {
		t.Errorf("body should preserve newlines; got %q", req.Params["body"])
	}
}

func TestNewTaskDefaultsPipelineToBuild(t *testing.T) {
	m := NewNewTask("hive").(*NewTask)
	m.title.SetValue("do a thing")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if cmd == nil {
		t.Fatal("submit should emit cmd")
	}
	req := cmd().(SubmitRequest)
	if req.Params["pipeline"] != "build" {
		t.Errorf("pipeline=%v want build (default)", req.Params["pipeline"])
	}
}

func TestNewTaskSubmitsChosenPipeline(t *testing.T) {
	m := NewNewTask("hive").(*NewTask)
	m.title.SetValue("do a thing")
	m.pipeline.SetValue("plan")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	req := cmd().(SubmitRequest)
	if req.Params["pipeline"] != "plan" {
		t.Errorf("pipeline=%v want plan", req.Params["pipeline"])
	}
}
