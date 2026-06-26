package tabs

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/rohilrs/Hive/internal/tui/style"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// CostRefreshRequest is emitted when the Costs tab wants to refresh.
// Root model receives this and calls Client.FetchCostSummary.
type CostRefreshRequest struct{}

// CostSummaryUpdate carries a fresh summary into the tab. Root model
// forwards costSummaryMsg payloads to the Costs tab as this type.
type CostSummaryUpdate struct{ Summary rpc.CostSummaryView }

// Costs renders four cost rollups (daily / model / pipeline / project)
// and auto-refreshes every refreshInterval while focused.
type Costs struct {
	summary         rpc.CostSummaryView
	width, height   int
	refreshInterval time.Duration
}

// NewCosts constructs the tab. Refresh cadence defaults to 10s.
func NewCosts() *Costs {
	return &Costs{refreshInterval: 10 * time.Second}
}

func (c *Costs) Name() string    { return "Costs" }
func (c *Costs) KeyHelp() string { return "r refresh" }

// Init kicks off the first fetch + a refresh ticker.
func (c *Costs) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg { return CostRefreshRequest{} },
		c.refreshTick(),
	)
}

func (c *Costs) refreshTick() tea.Cmd {
	return tea.Tick(c.refreshInterval, func(_ time.Time) tea.Msg {
		return CostRefreshRequest{}
	})
}

func (c *Costs) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		c.width = msg.Width
		c.height = msg.Height
	case tea.KeyMsg:
		if msg.String() == "r" {
			return c, func() tea.Msg { return CostRefreshRequest{} }
		}
	case CostSummaryUpdate:
		c.summary = msg.Summary
		return c, c.refreshTick()
	}
	return c, nil
}

func (c *Costs) View() string {
	if c.summary.GeneratedAt == 0 {
		return style.Hint.Render("Loading costs… press " + style.Key.Render("r") + " to refresh.")
	}
	panelW := (c.width - 6) / 2
	if panelW < 30 {
		panelW = 30
	}

	daily := renderBucketPanel("Daily (last 14)", c.summary.Daily, panelW)
	models := renderBucketPanel("By model", c.summary.Models, panelW)
	pipelines := renderBucketPanel("By pipeline", c.summary.Pipelines, panelW)
	projects := renderBucketPanel("By project", c.summary.Projects, panelW)

	top := lipgloss.JoinHorizontal(lipgloss.Top, daily, models)
	bottom := lipgloss.JoinHorizontal(lipgloss.Top, pipelines, projects)

	footer := style.DimText.Render(fmt.Sprintf("generated %s — press r to refresh",
		time.Unix(c.summary.GeneratedAt, 0).Format("15:04:05")))
	return top + "\n" + bottom + "\n" + footer
}

func renderBucketPanel(title string, buckets []rpc.CostBucket, width int) string {
	var b strings.Builder
	b.WriteString(style.Header.Render(title) + "\n\n")
	if len(buckets) == 0 {
		b.WriteString(style.DimText.Render("(no data)") + "\n")
	} else {
		var total float64
		for _, bb := range buckets {
			total += bb.TotalUSD
		}
		b.WriteString(fmt.Sprintf("  %-20s  %-10s  %-6s\n", "KEY", "USD", "COUNT"))
		for _, bb := range buckets {
			key := bb.Key
			if len(key) > 20 {
				key = key[:19] + "…"
			}
			b.WriteString(fmt.Sprintf("  %-20s  $%-9.4f  %-6d\n", key, bb.TotalUSD, bb.Count))
		}
		b.WriteString("  " + style.DimText.Render(fmt.Sprintf("total: $%.4f", total)) + "\n")
	}
	return style.Panel.Width(width).Render(b.String())
}
