package tabs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestCostsRendersLoadingWhenEmpty(t *testing.T) {
	c := NewCosts()
	view := c.View()
	if !strings.Contains(view, "Loading costs") {
		t.Errorf("expected loading message; got %q", view)
	}
}

func TestCostsRendersBuckets(t *testing.T) {
	c := NewCosts()
	c.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	c.Update(CostSummaryUpdate{Summary: rpc.CostSummaryView{
		GeneratedAt: 1234567890,
		Daily:       []rpc.CostBucket{{Key: "2026-05-24", TotalUSD: 0.42, Count: 3}},
		Models:      []rpc.CostBucket{{Key: "sonnet", TotalUSD: 0.30, Count: 2}, {Key: "haiku", TotalUSD: 0.12, Count: 1}},
		Pipelines:   []rpc.CostBucket{{Key: "build", TotalUSD: 0.42, Count: 3}},
		Projects:    []rpc.CostBucket{{Key: "hive", TotalUSD: 0.42, Count: 3}},
	}})
	view := c.View()
	if !strings.Contains(view, "sonnet") {
		t.Errorf("missing 'sonnet': %q", view)
	}
	if !strings.Contains(view, "2026-05-24") {
		t.Errorf("missing daily key: %q", view)
	}
	if !strings.Contains(view, "hive") {
		t.Errorf("missing project bucket: %q", view)
	}
}

func TestCostsRKeyEmitsRefresh(t *testing.T) {
	c := NewCosts()
	_, cmd := c.Update(tea.KeyMsg{Runes: []rune{'r'}, Type: tea.KeyRunes})
	if cmd == nil {
		t.Fatal("r should emit refresh cmd")
	}
	msg := cmd()
	if _, ok := msg.(CostRefreshRequest); !ok {
		t.Errorf("got msg %T want CostRefreshRequest", msg)
	}
}
