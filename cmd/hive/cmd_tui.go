package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/internal/tui"
	"github.com/rohilrs/Hive/internal/tui/tabs"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Launch the Hive TUI control surface",
	Long: `Launches a full-screen Bubbletea TUI that subscribes to the daemon's event
stream and renders Projects + Active tabs with drill-in for individual runs.

Keys: q quit · tab/1/2 switch tabs · ↑↓ select · enter drill-in · esc back

The TUI auto-reconnects if the daemon goes down; a banner appears while
disconnected.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		sock := daemonSocketPath()
		client := tui.NewClient(sock)
		snapshot := tui.NewSnapshot()

		tabModels := []tabs.TabModel{
			tabs.NewProjects(snapshot),
			tabs.NewActive(snapshot),
			tabs.NewApprovals(snapshot),
			tabs.NewCosts(),
			tabs.NewEvents(snapshot),
			tabs.NewChat(),
		}

		root := tui.NewModel(client, snapshot, tabModels)
		p := tea.NewProgram(root, tea.WithAltScreen(), tea.WithMouseCellMotion())
		client.Bind(p)
		go client.Run()
		defer client.Close()

		if _, err := p.Run(); err != nil {
			return fmt.Errorf("tui: %w", err)
		}
		return nil
	},
}
