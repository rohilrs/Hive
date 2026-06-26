package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var documentCmd = &cobra.Command{
	Use:   "document <run-id>",
	Short: "Re-run the documenter stage for a run (fills in docs after a skip)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := rpcCall(rpc.MethodRunDocument, map[string]any{"run_id": args[0]})
		if err != nil {
			return err
		}
		if d, _ := result["dispatched"].(bool); d {
			fmt.Printf("Documenter re-run dispatched for run %s — watch the TUI or `hive events` for the outcome.\n", args[0])
		}
		return nil
	},
}
