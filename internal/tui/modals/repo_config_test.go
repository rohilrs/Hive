package modals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func writeFakeRepoConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fake repo config: %v", err)
	}
	return path
}

func TestRepoConfigModalRendersContents(t *testing.T) {
	body := "[integration]\n  merge_method = \"squash\"\n\n[pipelines.build]\n  test_command = \"pnpm test\"\n"
	path := writeFakeRepoConfig(t, body)
	m := NewRepoConfigModal("sidecar", path)
	if m.notice != "" {
		t.Fatalf("notice=%q want empty (file loaded)", m.notice)
	}
	view := m.View(100, 40)
	for _, want := range []string{"Repo config — sidecar", "merge_method", "pnpm test", path} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q; got:\n%s", want, view)
		}
	}
}

func TestRepoConfigModalMissingFileHint(t *testing.T) {
	// A path that doesn't exist → the --init hint, not an error.
	path := filepath.Join(t.TempDir(), "config.toml")
	m := NewRepoConfigModal("sidecar", path)
	if !strings.Contains(m.notice, "--init") {
		t.Errorf("notice=%q want the `hive repo config sidecar --init` hint", m.notice)
	}
	if !strings.Contains(m.View(80, 24), "--init") {
		t.Errorf("view should render the --init hint")
	}
}

func TestRepoConfigModalNoRepoPath(t *testing.T) {
	// Empty path (project has no repo_path) → a "no repo path" notice.
	m := NewRepoConfigModal("sidecar", "")
	if !strings.Contains(m.notice, "no repo path") {
		t.Errorf("notice=%q want a 'no repo path' message", m.notice)
	}
}

func TestRepoConfigModalEscCloses(t *testing.T) {
	m := NewRepoConfigModal("sidecar", writeFakeRepoConfig(t, "x = 1\n"))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should emit a cmd")
	}
	if _, ok := cmd().(CloseMsg); !ok {
		t.Errorf("esc should emit CloseMsg; got %T", cmd())
	}
}

func TestRepoConfigModalScrollClampsAtTop(t *testing.T) {
	m := NewRepoConfigModal("sidecar", writeFakeRepoConfig(t, "a = 1\nb = 2\n"))
	// up at the top is a no-op (no negative scroll).
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.scroll != 0 {
		t.Errorf("scroll=%d want 0 (clamped at top)", m.scroll)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.scroll != 1 {
		t.Errorf("scroll=%d want 1 after one down", m.scroll)
	}
}
