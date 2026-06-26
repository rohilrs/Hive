package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestMergeRetryCmdWiring(t *testing.T) {
	root := newMergeCmd()
	var retry *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "retry" {
			retry = c
		}
	}
	if retry == nil {
		t.Fatal("retry subcommand not registered under merge")
	}
	if retry.Args == nil {
		t.Error("retry should require exactly 1 arg")
	}
}
