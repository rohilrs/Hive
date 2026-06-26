package style

import (
	"strings"
	"testing"
)

func TestNewTokensExist(t *testing.T) {
	// Compile-time presence check: each token below must be a valid
	// identifier exported from the package. The test enforces the
	// surface in a way `go vet` won't catch alone.
	_ = Modal
	_ = ModalTitle
	_ = Hint
	_ = InlineError
	_ = Key
	_ = Danger
}

func TestCursorMarkerActiveAndInactive(t *testing.T) {
	active := CursorMarker(true)
	if !strings.Contains(active, "▸") {
		t.Errorf("active marker should contain ▸, got %q", active)
	}
	inactive := CursorMarker(false)
	if strings.Contains(inactive, "▸") {
		t.Errorf("inactive marker should NOT contain ▸, got %q", inactive)
	}
	// Inactive marker should still be 2 visible cells wide so column
	// alignment doesn't shift between rows. lipgloss doesn't expose a
	// raw-width helper here; the literal "  " (2 spaces) is what we use.
	if inactive != "  " {
		t.Errorf("inactive marker should be exactly 2 spaces, got %q", inactive)
	}
}

func TestHintAndDimTextAreEquivalent(t *testing.T) {
	// Hint is meant as a more-readable name for DimText. Both styles
	// should render the same string identically until/unless one is
	// rebranded.
	in := "hello"
	if Hint.Render(in) != DimText.Render(in) {
		t.Errorf("Hint should render identically to DimText (alias semantics)")
	}
}

func TestScrollHintReturnsEmptyForZero(t *testing.T) {
	if got := ScrollHint("up", 0); got != "" {
		t.Errorf("ScrollHint(up, 0) should be empty, got %q", got)
	}
	if got := ScrollHint("down", 0); got != "" {
		t.Errorf("ScrollHint(down, 0) should be empty, got %q", got)
	}
	if got := ScrollHint("up", -3); got != "" {
		t.Errorf("ScrollHint(up, -3) should be empty (count<=0), got %q", got)
	}
}

func TestScrollHintUpAndDown(t *testing.T) {
	up := ScrollHint("up", 5)
	if !strings.Contains(up, "↑ 5 more") {
		t.Errorf("ScrollHint(up,5) should contain '↑ 5 more', got %q", up)
	}
	down := ScrollHint("down", 12)
	if !strings.Contains(down, "↓ 12 more") {
		t.Errorf("ScrollHint(down,12) should contain '↓ 12 more', got %q", down)
	}
}

func TestScrollHintIgnoresUnknownDirection(t *testing.T) {
	if got := ScrollHint("xyz", 5); got != "" {
		t.Errorf("ScrollHint(xyz,5) should be empty for unknown direction, got %q", got)
	}
	if got := ScrollHint("", 5); got != "" {
		t.Errorf("ScrollHint(\"\",5) should be empty for unknown direction, got %q", got)
	}
}

func TestKeyIsBold(t *testing.T) {
	// Lipgloss doesn't expose .GetBold() cleanly without inspecting the
	// rendered ANSI. Confirm by rendering and looking for the bold SGR.
	rendered := Key.Render("q")
	if !strings.Contains(rendered, "\x1b[") {
		t.Skip("ANSI output not emitted in this env; skipping")
	}
	if !strings.Contains(rendered, "1") { // SGR 1 = bold
		t.Errorf("Key should render with bold SGR; got %q", rendered)
	}
}
