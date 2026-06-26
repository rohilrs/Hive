package capsule

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shared fake-scavenger build. Mirrors the pattern in capsule_test.go.
// Uses os.MkdirTemp (not t.TempDir) so the binary outlives the first
// test that triggers the build — subsequent tests in the same process
// reuse the cached path.
var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

func buildFakeScav(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fake-scavenger-mcp-")
		if err != nil {
			fakeErr = errors.New("mkdir fake-scavenger: " + err.Error())
			return
		}
		out := filepath.Join(dir, "fake-scavenger")
		cmd := exec.Command("go", "build", "-o", out, "../../../scripts/fake-scavenger")
		if msg, err := cmd.CombinedOutput(); err != nil {
			fakeErr = errors.New("build fake-scavenger: " + err.Error() + "; output: " + string(msg))
			return
		}
		fakePath = out
	})
	if fakeErr != nil {
		t.Fatalf("buildFakeScav: %v", fakeErr)
	}
	return fakePath
}

// newTestFetcher constructs an MCPFetcher pointed at the fake-scavenger
// binary. Returns the fetcher and a cleanup func that closes it.
func newTestFetcher(t *testing.T, env map[string]string) (*MCPFetcher, func()) {
	t.Helper()
	bin := buildFakeScav(t)
	for k, v := range env {
		t.Setenv(k, v)
	}
	f := NewMCPFetcher(Config{Binary: bin})
	cleanup := func() { _ = f.Close() }
	t.Cleanup(cleanup)
	return f, cleanup
}

func TestMCPFetcherFirstFetchSpawnsBridge(t *testing.T) {
	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_SPAWN_LOG": spawnLog})
	cap, err := f.Fetch(context.Background(), Req{File: "x.go", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cap.Body == "" {
		t.Errorf("Capsule.Body empty; expected the fake fixture")
	}
	if got := countLines(t, spawnLog); got != 1 {
		t.Errorf("spawn count=%d, want 1", got)
	}
}

func TestMCPFetcherSecondFetchReusesBridge(t *testing.T) {
	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_SPAWN_LOG": spawnLog})
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), Req{File: "b.go", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, spawnLog); got != 1 {
		t.Errorf("spawn count=%d, want 1 (bridge reused)", got)
	}
}

func TestMCPFetcherMultipleReposSpawnSeparateBridges(t *testing.T) {
	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	r1 := t.TempDir()
	r2 := t.TempDir()
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_SPAWN_LOG": spawnLog})
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: r1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: r2}); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, spawnLog); got != 2 {
		t.Errorf("spawn count=%d, want 2 (one per repo)", got)
	}
}

func TestMCPFetcherBudgetIgnored(t *testing.T) {
	f, _ := newTestFetcher(t, nil)
	// Budget=999 set; we can't directly inspect the wire request here
	// without modifying the fake to echo. Verify by absence of side
	// effects: the Fetch succeeds and returns the standard capsule.
	cap, err := f.Fetch(context.Background(), Req{File: "x.go", Cwd: "/tmp", Budget: 999})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cap.Body == "" {
		t.Error("Capsule.Body empty; Budget shouldn't have changed behavior")
	}
}

func TestMCPFetcherSubprocessDeathTriggersRespawn(t *testing.T) {
	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	f, _ := newTestFetcher(t, map[string]string{
		"FAKE_SCAVENGER_SPAWN_LOG":  spawnLog,
		"FAKE_SCAVENGER_EXIT_AFTER": "1",
	})
	// First Fetch: succeeds; subprocess exits after this tools/call.
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: "/tmp"}); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	// Give the subprocess a moment to actually exit.
	time.Sleep(50 * time.Millisecond)
	// Second Fetch: detects dead bridge → returns error.
	if _, err := f.Fetch(context.Background(), Req{File: "b.go", Cwd: "/tmp"}); err == nil {
		t.Error("expected error on second Fetch (bridge died); got nil")
	}
	// Third Fetch: respawns a fresh bridge (still EXIT_AFTER=1, so this
	// new bridge will succeed on its first call).
	if _, err := f.Fetch(context.Background(), Req{File: "c.go", Cwd: "/tmp"}); err != nil {
		t.Errorf("third Fetch (respawn): %v", err)
	}
	if got := countLines(t, spawnLog); got != 2 {
		t.Errorf("spawn count=%d, want 2 (initial + respawn)", got)
	}
}

func TestMCPFetcherCloseShutsDownAllBridges(t *testing.T) {
	spawnLog := filepath.Join(t.TempDir(), "spawns.log")
	r1 := t.TempDir()
	r2 := t.TempDir()
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_SPAWN_LOG": spawnLog})
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: r1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: r2}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// After Close, the bridges map is empty; a new Fetch would respawn.
	// Verify a post-Close Fetch works (proves Close was clean).
	if _, err := f.Fetch(context.Background(), Req{File: "a.go", Cwd: r1}); err != nil {
		t.Errorf("post-Close Fetch: %v", err)
	}
	if got := countLines(t, spawnLog); got != 3 {
		t.Errorf("spawn count=%d, want 3 (r1, r2, post-Close r1)", got)
	}
}

func TestMCPFetcherConcurrentFetchesSamRepoSerialized(t *testing.T) {
	f, _ := newTestFetcher(t, nil)
	const N = 5
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := f.Fetch(context.Background(), Req{File: "x.go", Cwd: "/tmp"})
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("Fetch[%d]: %v", i, e)
		}
	}
}

func TestMCPFetcherHandshakeFailureSurfacesError(t *testing.T) {
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_FAIL_HANDSHAKE": "1"})
	_, err := f.Fetch(context.Background(), Req{File: "x.go", Cwd: "/tmp"})
	if err == nil {
		t.Fatal("expected handshake error; got nil")
	}
	if !strings.Contains(err.Error(), "handshake") && !strings.Contains(err.Error(), "initialize") {
		t.Errorf("error doesn't mention handshake: %v", err)
	}
}

func TestMCPFetcherToolErrorResponseReturned(t *testing.T) {
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_TOOL_ERROR": "file not found"})
	_, err := f.Fetch(context.Background(), Req{File: "x.go", Cwd: "/tmp"})
	if err == nil {
		t.Fatal("expected tool error; got nil")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Errorf("error doesn't wrap message: %v", err)
	}
}

func TestMCPFetcherParsesTargetBodyShape(t *testing.T) {
	capsuleText := "[TARGET]\nthe target\n\n[BODY] symname\nthe body text"
	f, _ := newTestFetcher(t, map[string]string{"FAKE_SCAVENGER_CAPSULE": capsuleText})
	cap, err := f.Fetch(context.Background(), Req{File: "x.go", Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if cap.Target == "" || !strings.Contains(cap.Target, "the target") {
		t.Errorf("Target wrong: %q", cap.Target)
	}
	if cap.Body == "" || !strings.Contains(cap.Body, "the body text") {
		t.Errorf("Body wrong: %q", cap.Body)
	}
}

// countLines returns the number of newline-terminated lines in path.
// Returns 0 if the file doesn't exist (no spawns yet).
func countLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("open spawn log: %v", err)
	}
	defer f.Close()
	n := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		n++
	}
	return n
}

// Suppress unused-import warning if some test doesn't reference io.
var _ = io.EOF
