package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var (
	sourcesBindRepo      string
	sourcesBindLabels    []string
	sourcesBindTeams     []string
	sourcesBindProjects  []string
	sourcesBindWriteBack bool
	sourcesBindWBTeam    string
	sourcesBindWBProject string
)

var sourcesCmd = &cobra.Command{Use: "sources", Short: "Manage per-project task sources"}

var sourcesBindCmd = &cobra.Command{
	Use:   "bind <slug> <source>",
	Short: "Bind a source (github|linear|inbox) to a project",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		slug, source := args[0], args[1]
		binding := map[string]any{}
		switch source {
		case "github":
			if sourcesBindRepo != "" {
				binding["repo"] = sourcesBindRepo
			}
			if len(sourcesBindLabels) > 0 {
				binding["labels"] = sourcesBindLabels
			}
		case "linear":
			if len(sourcesBindTeams) > 0 {
				binding["teams"] = sourcesBindTeams
			}
			if len(sourcesBindProjects) > 0 {
				binding["projects"] = sourcesBindProjects
			}
			if sourcesBindWriteBack {
				binding["write_back"] = true
			}
			if sourcesBindWBTeam != "" {
				binding["wb_team"] = sourcesBindWBTeam
			}
			if sourcesBindWBProject != "" {
				binding["wb_project"] = sourcesBindWBProject
			}
		case "inbox":
			// no binding fields
		default:
			return fmt.Errorf("unknown source %q (expected github|linear|inbox)", source)
		}
		if _, err := rpcCall(rpc.MethodSourcesBind, map[string]any{
			"slug": slug, "source": source, "binding": binding,
		}); err != nil {
			return err
		}
		fmt.Printf("Bound %s source to project %s\n", source, slug)
		return nil
	},
}

var sourcesListCmd = &cobra.Command{
	Use:   "list <slug>",
	Short: "List bound sources for a project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := rpcCallRaw(rpc.MethodSourcesList, map[string]any{"slug": args[0]})
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("decode sources: %w", err)
		}
		if len(m) == 0 {
			fmt.Printf("No sources bound to project %s\n", args[0])
			return nil
		}
		pretty, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(pretty))
		return nil
	},
}

var sourcesUnbindCmd = &cobra.Command{
	Use:   "unbind <slug> <source>",
	Short: "Remove a bound source from a project",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := rpcCall(rpc.MethodSourcesUnbind, map[string]any{
			"slug": args[0], "source": args[1],
		}); err != nil {
			return err
		}
		fmt.Printf("Unbound %s source from project %s\n", args[1], args[0])
		return nil
	},
}

func init() {
	sourcesBindCmd.Flags().StringVar(&sourcesBindRepo, "repo", "", "github repo (owner/name)")
	sourcesBindCmd.Flags().StringArrayVar(&sourcesBindLabels, "label", nil, "github issue label filter (repeatable)")
	sourcesBindCmd.Flags().StringArrayVar(&sourcesBindTeams, "team", nil, "linear team filter (repeatable)")
	sourcesBindCmd.Flags().StringArrayVar(&sourcesBindProjects, "project", nil, "linear project ID filter (repeatable; narrows ingestion to specific Linear projects within the bound teams)")
	sourcesBindCmd.Flags().BoolVar(&sourcesBindWriteBack, "write-back", false, "linear: mirror Hive-originated tasks to Linear + sync status (Phase 1)")
	sourcesBindCmd.Flags().StringVar(&sourcesBindWBTeam, "wb-team", "", "linear write-back target team key (required if >1 --team)")
	sourcesBindCmd.Flags().StringVar(&sourcesBindWBProject, "wb-project", "", "linear write-back target project id/slug (required if >1 --project)")
	sourcesCmd.AddCommand(sourcesBindCmd, sourcesListCmd, sourcesUnbindCmd)
}
