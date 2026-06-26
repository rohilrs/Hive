package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/rohilrs/Hive/pkg/rpc"
)

var syncStatusFlag bool

func init() {
	syncCmd.Flags().BoolVar(&syncStatusFlag, "status", false, "show last-sync time + counts per source (no sync)")
}

var syncCmd = &cobra.Command{
	Use:   "sync [source]",
	Short: "Pull bound sources into tasks now (github|linear|inbox; all if omitted)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if syncStatusFlag {
			raw, err := rpcCallRaw(rpc.MethodSourcesStatus, map[string]any{})
			if err != nil {
				return err
			}
			var st map[string]struct {
				LastSyncUnix int64  `json:"last_sync_unix"`
				Inserted     int    `json:"inserted"`
				Updated      int    `json:"updated"`
				Closed       int    `json:"closed"`
				Error        string `json:"error"`
			}
			_ = json.Unmarshal(raw, &st)
			if len(st) == 0 {
				fmt.Println("No sources have synced yet.")
				return nil
			}
			for name, s := range st {
				when := "never"
				if s.LastSyncUnix > 0 {
					when = time.Unix(s.LastSyncUnix, 0).Format("2006-01-02 15:04:05")
				}
				line := fmt.Sprintf("%-8s last=%s  +%d ~%d -%d", name, when, s.Inserted, s.Updated, s.Closed)
				if s.Error != "" {
					line += "  ERROR: " + s.Error
				}
				fmt.Println(line)
			}
			return nil
		}
		params := map[string]any{}
		if len(args) == 1 {
			params["source"] = args[0]
		}
		raw, err := rpcCallRaw(rpc.MethodSourcesSync, params)
		if err != nil {
			return err
		}
		var rep struct {
			PerSource map[string]struct {
				Inserted, Updated, Closed int
				Error                     string
			} `json:"per_source"`
		}
		_ = json.Unmarshal(raw, &rep)
		if len(rep.PerSource) == 0 {
			fmt.Println("No bound sources to sync.")
			return nil
		}
		anyErr := false
		for name, r := range rep.PerSource {
			if r.Error != "" {
				anyErr = true
				fmt.Printf("%-8s ERROR: %s\n", name, r.Error)
				continue
			}
			fmt.Printf("%-8s +%d new, ~%d updated, -%d closed\n", name, r.Inserted, r.Updated, r.Closed)
		}
		if anyErr {
			return fmt.Errorf("one or more sources failed")
		}
		return nil
	},
}
