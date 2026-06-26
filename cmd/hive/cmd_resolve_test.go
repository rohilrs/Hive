package main

import "testing"

// TestResolveCmdRequiresTaskArg verifies cobra rejects `hive resolve` when no
// task-id is supplied (Args = cobra.ExactArgs(1)).
func TestResolveCmdRequiresTaskArg(t *testing.T) {
	cmd := newResolveCmd()
	cmd.SetArgs([]string{})
	// Silence cobra's usage output to keep the test log clean.
	cmd.SetOut(discardWriter{})
	cmd.SetErr(discardWriter{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no task-id given")
	}
}
