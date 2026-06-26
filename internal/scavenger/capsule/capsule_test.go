package capsule

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildFakeScavenger compiles scripts/fake-scavenger to a tempdir and
// returns the path. Single binary per test process; sync.Once-cached.
func buildFakeScavenger(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fake-scavenger")
	cmd := exec.Command("go", "build", "-o", out, "../../../scripts/fake-scavenger")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake-scavenger: %v\n%s", err, b)
	}
	return out
}

func TestFetchReturnsParsedSections(t *testing.T) {
	bin := buildFakeScavenger(t)
	f := NewCLIFetcher(Config{Binary: bin})

	got, err := f.Fetch(context.Background(), Req{File: "x.go"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(got.Target, "BuildPipeline) Run") {
		t.Errorf("Target=%q want substring 'BuildPipeline) Run'", got.Target)
	}
	if !strings.Contains(got.Callees, "derefRepoPath") {
		t.Errorf("Callees=%q want substring 'derefRepoPath'", got.Callees)
	}
	if !strings.Contains(got.Body, "BuildPipeline) Run") {
		t.Errorf("Body=%q want substring 'BuildPipeline) Run'", got.Body)
	}
	if got.TokenEstimate == 0 {
		t.Errorf("TokenEstimate=0 want >0 for non-empty raw")
	}
	if got.Raw == "" {
		t.Errorf("Raw is empty")
	}
}

func TestFetchEmptyResult(t *testing.T) {
	bin := buildFakeScavenger(t)
	f := NewCLIFetcher(Config{Binary: bin})

	t.Setenv("FAKE_SCAVENGER_CAPSULE", "empty")
	got, err := f.Fetch(context.Background(), Req{File: "x.go"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Target != "" || got.Callees != "" || got.Body != "" {
		t.Errorf("expected empty capsule fields, got %+v", got)
	}
	if !strings.Contains(got.Raw, "0 tokens") {
		t.Errorf("Raw should contain raw empty-result string; got %q", got.Raw)
	}
}

func TestFetchTimeout(t *testing.T) {
	bin := buildFakeScavenger(t)
	// Force fake-scavenger to hang via a sleep wrapper would be the right
	// pattern, but here we just set a near-zero timeout so the subprocess
	// can't possibly finish in time even on a fast box. Slightly racy on
	// extremely fast machines; treat as best-effort.
	f := NewCLIFetcher(Config{Binary: bin, PerCallTimeout: 1 * time.Nanosecond})
	_, err := f.Fetch(context.Background(), Req{File: "x.go"})
	if err == nil {
		t.Errorf("expected timeout error, got nil")
	}
}

func TestFetchMissingBinary(t *testing.T) {
	f := NewCLIFetcher(Config{Binary: "/does/not/exist/scavenger"})
	_, err := f.Fetch(context.Background(), Req{File: "x.go"})
	if err == nil {
		t.Errorf("expected error for missing binary, got nil")
	}
}

func TestParseCapsuleHandlesMissingSections(t *testing.T) {
	raw := `[TARGET]
just target, no other sections
`
	got := parseCapsule(raw)
	if !strings.Contains(got.Target, "just target") {
		t.Errorf("Target=%q want substring 'just target'", got.Target)
	}
	if got.Callees != "" || got.Body != "" || got.Callers != "" {
		t.Errorf("expected absent sections to be empty, got %+v", got)
	}
}
