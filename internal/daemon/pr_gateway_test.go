package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGhOpenPRArgs(t *testing.T) {
	// Assert OpenPR returns the URL gh prints on success, using a fake `gh` on PATH.
	dir := t.TempDir()
	fakeGh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho https://github.com/o/r/pull/42\n"
	if err := os.WriteFile(fakeGh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	url, err := ghPRGateway{}.OpenPR(context.Background(), t.TempDir(),
		"spec/conversation-rework", "chat-test-harness", "title", "body", false)
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if url != "https://github.com/o/r/pull/42" {
		t.Errorf("url=%q", url)
	}
}
