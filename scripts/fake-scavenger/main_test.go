package main

// Compile-only guard: the fake must recognize the subcommands Hive's
// per-run lifecycle invokes. Real behavioral coverage lives in
// internal/scavenger/client_test.go (which builds and runs this binary).
// This test documents the expected subcommand set.
import "testing"

func TestKnownSubcommands(t *testing.T) {
	for _, sc := range []string{"daemon", "doctor", "index", "init", "capsule", "mcp-bridge"} {
		if sc == "" {
			t.Fatal("empty subcommand")
		}
	}
}
