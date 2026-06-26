package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs <run-id>",
	Short: "Show JSONL logs + stderr for a run",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		runID := args[0]
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, ".hive", runID)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read logs dir %s: %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			stagePath := filepath.Join(dir, e.Name())
			stageEntries, _ := os.ReadDir(stagePath)
			for _, f := range stageEntries {
				if ext := filepath.Ext(f.Name()); ext != ".jsonl" && ext != ".log" {
					continue
				}
				fmt.Printf("== %s/%s ==\n", e.Name(), f.Name())
				fh, err := os.Open(filepath.Join(stagePath, f.Name()))
				if err != nil {
					continue
				}
				io.Copy(os.Stdout, fh)
				fh.Close()
			}
		}
		return nil
	},
}
