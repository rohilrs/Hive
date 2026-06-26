package modals

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/internal/tui/style"
)

// repoConfigMaxBytes caps how much of the repo config we read into memory. The
// per-repo config.toml is a small hand-edited file (<8 KB in practice); the cap
// guards against a pathological file blocking the UI thread on construction.
const repoConfigMaxBytes = 256 * 1024

// RepoConfigModal is a READ-ONLY viewer of a project's per-repo config layer
// (~/.hive/repos/<key>/config.toml — the layer merged between global and
// per-project config, shared by every project on the same repo). It mirrors
// `hive repo config <slug>`: the TUI is view-only (editing the file stays a
// CLI/$EDITOR action, like the CLI's --path flow) so there's no accidental
// clobber of a hand-tuned, multi-project-shared file from a scrollable pane.
//
// The file is read synchronously on construction (a single capped os.ReadFile,
// same as the roadmap viewer) — the root resolves the path via repoConfigPath
// (config.RepoKey) and passes it in. Failure modes render inline + keep esc
// working so the operator is never trapped:
//
//   - empty path (project has no repo_path) → "project has no repo path…"
//   - file absent                            → "no per-repo config yet — run
//     `hive repo config <slug> --init`"
//   - read error                             → the error string
//
// Keys: ↑/k ↓/j scroll · esc close.
type RepoConfigModal struct {
	slug string
	path string

	// content is the raw config.toml body, split into lines for windowing.
	// nil when the file was absent/unreadable; notice carries the reason.
	lines []string

	// notice is the inline message shown instead of content (absent file,
	// no repo path, read error). Empty when content loaded.
	notice string

	scroll        int
	width, height int
}

// NewRepoConfigModal reads the repo config at path synchronously. path is the
// resolved ~/.hive/repos/<key>/config.toml (or "" when the project has no
// repo_path). The constructor MUST NOT block on the network — only a single
// capped os.ReadFile.
func NewRepoConfigModal(slug, path string) *RepoConfigModal {
	m := &RepoConfigModal{slug: slug, path: path}
	if path == "" {
		m.notice = "project " + slug + " has no repo path — set one with `hive project edit " + slug + " --repo <path>`"
		return m
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			m.notice = "no per-repo config yet — run `hive repo config " + slug + " --init` to create it"
		} else {
			m.notice = "read error: " + err.Error()
		}
		return m
	}
	if len(body) > repoConfigMaxBytes {
		body = body[:repoConfigMaxBytes]
	}
	// Trim a single trailing newline so the last line isn't a blank row.
	m.lines = strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	return m
}

func (m *RepoConfigModal) Title() string { return "Repo config — " + m.slug }

// Init returns nil — the read happens in the constructor (no async work).
func (m *RepoConfigModal) Init() tea.Cmd { return nil }

func (m *RepoConfigModal) Update(msg tea.Msg) (Modal, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return m, func() tea.Msg { return CloseMsg{} }
		case "up", "k":
			if m.scroll > 0 {
				m.scroll--
			}
			return m, nil
		case "down", "j":
			// Loosely bounded by the line count; View clamps precisely via
			// windowLines so it never scrolls past the end.
			if m.scroll < len(m.lines) {
				m.scroll++
			}
			return m, nil
		}
	}
	return m, nil
}

func (m *RepoConfigModal) View(width, height int) string {
	var b strings.Builder
	b.WriteString(style.ModalTitle.Render(m.Title()) + "\n")
	// Show the resolved path (dim) so the operator knows exactly which file this
	// is + where to point $EDITOR — mirrors `hive repo config <slug> --path`.
	if m.path != "" {
		b.WriteString(style.DimText.Render(m.path) + "\n")
	}
	b.WriteString("\n")

	if m.notice != "" {
		b.WriteString(style.Hint.Render(m.notice) + "\n\n")
		b.WriteString(style.Hint.Render(style.Key.Render("esc") + " close"))
		return m.frame(b.String(), width)
	}

	// Body: the config.toml lines, windowed to the available height so a long
	// file never overflows the modal. Budget: total height minus title (1) +
	// path (1) + the blank (1) + footer (1) + modal chrome (border 2 + padding
	// 2 = 4). Clamp to a usable minimum.
	bodyH := height - 8
	if bodyH < 3 {
		bodyH = 3
	}
	win := windowLines(m.lines, bodyH, m.scroll)
	b.WriteString(strings.Join(win, "\n") + "\n\n")

	b.WriteString(style.Hint.Render(
		style.Key.Render("↑↓") + " scroll · " +
			style.Key.Render("esc") + " close · " +
			style.DimText.Render("(read-only — edit via `hive repo config "+m.slug+" --path`)")))
	return m.frame(b.String(), width)
}

// frame wraps the modal body in the standard rounded-border + capped width
// (reuses the package frameWidth helper so a wide toml has room).
func (m *RepoConfigModal) frame(content string, width int) string {
	return style.Modal.Width(frameWidth(width)).Render(content)
}
