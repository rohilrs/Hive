package modals

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakePreview is a cleanup.run dry-run result wire shape (numbers as float64,
// items as []any of map[string]any) mirroring what the daemon returns through
// the TUI client's map[string]any decode path.
func fakePreview() map[string]any {
	return map[string]any{
		"runs":    float64(2),
		"bytes":   float64(5 * 1024 * 1024), // 5 MiB
		"dry_run": true,
		"kept":    float64(3),
		"items": []any{
			map[string]any{"run_id": "run-aaa", "reason": "terminal > keep_last"},
			map[string]any{"run_id": "run-bbb", "reason": "terminal > keep_last"},
		},
		"errors": []any{},
	}
}

func TestCleanModalInitialStateIsPreview(t *testing.T) {
	m := NewCleanModal()
	if m.state != "preview" {
		t.Fatalf("state=%q want preview", m.state)
	}
	if !strings.Contains(m.View(100, 40), "scanning") {
		t.Errorf("preview view should show a scanning line; got:\n%s", m.View(100, 40))
	}
}

func TestCleanModalPreviewRendersPlan(t *testing.T) {
	m := NewCleanModal()
	m.Update(RPCResultMsg{Kind: "cleanup_preview", Data: fakePreview()})
	if m.state != "confirm" {
		t.Fatalf("state=%q want confirm after preview result", m.state)
	}
	out := m.View(120, 40)
	// Humanized bytes (5.0 MiB) + run/kept counts + the per-item list.
	for _, want := range []string{"Would reclaim", "5.0 MiB", "run-aaa", "run-bbb", "terminal > keep_last"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirm view should contain %q; got:\n%s", want, out)
		}
	}
}

// TestCleanModalConfirmEmitsCleanupRun: enter/y from the confirm state emits
// the cleanup_run SubmitRequest (the real reclaim) and enters running state.
func TestCleanModalConfirmEmitsCleanupRun(t *testing.T) {
	m := NewCleanModal()
	m.Update(RPCResultMsg{Kind: "cleanup_preview", Data: fakePreview()})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.state != "running" {
		t.Errorf("state=%q want running after confirm", m.state)
	}
	if cmd == nil {
		t.Fatal("confirm should emit a command")
	}
	sr, ok := cmd().(SubmitRequest)
	if !ok {
		t.Fatalf("confirm should emit a SubmitRequest; got %T", cmd())
	}
	if sr.Kind != "cleanup_run" {
		t.Errorf("confirm Kind=%q want cleanup_run", sr.Kind)
	}
}

// TestCleanModalDoneShowsResult: a cleanup_done result renders the final
// "reclaimed" summary with humanized bytes.
func TestCleanModalDoneShowsResult(t *testing.T) {
	m := NewCleanModal()
	m.Update(RPCResultMsg{Kind: "cleanup_preview", Data: fakePreview()})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → running
	done := map[string]any{
		"runs":    float64(2),
		"bytes":   float64(5 * 1024 * 1024),
		"dry_run": false,
		"kept":    float64(3),
		"items":   []any{},
		"errors":  []any{},
	}
	m.Update(RPCResultMsg{Kind: "cleanup_done", Data: done})
	if m.state != "done" {
		t.Fatalf("state=%q want done", m.state)
	}
	out := m.View(120, 40)
	if !strings.Contains(out, "reclaimed 2 run(s)") || !strings.Contains(out, "5.0 MiB") {
		t.Errorf("done view should show the reclaim summary; got:\n%s", out)
	}
}

// TestCleanModalErrorsRenderInline: per-item errors from the daemon surface.
func TestCleanModalErrorsRenderInline(t *testing.T) {
	m := NewCleanModal()
	data := fakePreview()
	data["errors"] = []any{"rm run-aaa: permission denied"}
	m.Update(RPCResultMsg{Kind: "cleanup_preview", Data: data})
	if out := m.View(120, 40); !strings.Contains(out, "permission denied") {
		t.Errorf("error list should render inline; got:\n%s", out)
	}
}

// TestCleanModalPreviewErrorSurfaced: a transport error on the dry-run shows
// inline rather than leaving the modal blank.
func TestCleanModalPreviewErrorSurfaced(t *testing.T) {
	m := NewCleanModal()
	m.Update(RPCResultMsg{Kind: "cleanup_preview", Err: errors.New("dial daemon: no socket")})
	if out := m.View(100, 40); !strings.Contains(out, "no socket") {
		t.Errorf("preview error should surface inline; got:\n%s", out)
	}
}

func TestCleanModalEscCloses(t *testing.T) {
	m := NewCleanModal()
	m.Update(RPCResultMsg{Kind: "cleanup_preview", Data: fakePreview()})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a command")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := map[int64]string{
		0:               "0 B",
		512:             "512 B",
		1024:            "1.0 KiB",
		5 * 1024 * 1024: "5.0 MiB",
	}
	for in, want := range cases {
		if got := humanizeBytes(in); got != want {
			t.Errorf("humanizeBytes(%d)=%q want %q", in, got, want)
		}
	}
}
