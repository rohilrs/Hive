// Command hive is Hive's single binary: daemon, CLI, mcp-stage-server.
// TUI lives elsewhere; future phases may add subcommands.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "hive",
	Short:         "Hive — terminal-first Claude Code orchestrator",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func main() {
	rootCmd.AddCommand(
		daemonCmd,
		taskCmd,
		runCmd,
		resolveCmd,
		statusCmd,
		logsCmd,
		mcpStageServerCmd,
		predictCmd,
		eventsCmd,
		tuiCmd,
		approvalsCmd,
		abandonCmd,
		documentCmd,
		syncCmd,
		sourcesCmd,
		projectCmd,
		chatCmd,
		planCmd,
		doctorCmd,
		decomposeCmd,
		roadmapCmd,
		initCmd,
		sequenceCmd,
		cleanCmd,
		newRepoCmd(),
		newMergeCmd(),
	)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "hive:", err)
		os.Exit(1)
	}
}
