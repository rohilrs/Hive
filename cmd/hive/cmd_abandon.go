package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var abandonCmd = &cobra.Command{
	Use:   "abandon <run-id>",
	Short: "Abandon a run (cancel its in-flight pipeline, mark abandoned)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		result, err := rpcCall(rpc.MethodAbandon, map[string]any{"run_id": args[0]})
		if err != nil {
			return err
		}
		fmt.Printf("Abandoned run %s (cancelled in-flight worker: %v)\n", args[0], result["cancelled"])
		return nil
	},
}
