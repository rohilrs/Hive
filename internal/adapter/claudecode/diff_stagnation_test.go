package claudecode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStagnationTracker(t *testing.T) {
	const timeout = 100 * time.Millisecond
	tr := &stagnationTracker{timeout: timeout, lastChange: time.Now()}

	t0 := time.Now()

	// Empty hash: not started. Even past the timeout it must not fire.
	if tr.observe("", t0) {
		t.Fatal("empty hash at t0 fired (should never fire before content appears)")
	}
	if tr.observe("", t0.Add(2*timeout)) {
		t.Fatal("empty hash past timeout fired (not started → no fire)")
	}

	// First real content at t1 establishes the baseline; no fire.
	t1 := t0.Add(3 * timeout)
	if tr.observe("h1", t1) {
		t.Fatal("first non-empty hash fired (it just started; lastChange reset)")
	}

	// Same hash at t1 + timeout/2: within window, no fire.
	if tr.observe("h1", t1.Add(timeout/2)) {
		t.Fatal("same hash within window fired early")
	}

	// Same hash at t1 + timeout: window elapsed → FIRE.
	if !tr.observe("h1", t1.Add(timeout)) {
		t.Fatal("same hash for full timeout did not fire")
	}

	// A changing hash resets lastChange so it does not fire even though the
	// previous observe was a fire.
	t2 := t1.Add(timeout)
	if tr.observe("h2", t2) {
		t.Fatal("changed hash fired (a change must reset the window)")
	}
	if tr.observe("h2", t2.Add(timeout/2)) {
		t.Fatal("changed hash fired within its fresh window")
	}
	if !tr.observe("h2", t2.Add(timeout)) {
		t.Fatal("changed hash did not fire after its own full window")
	}
}

func TestWorktreeProgressHash(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME="+dir,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")

	base := filepath.Join(dir, "base.txt")
	if err := os.WriteFile(base, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	run("commit", "-q", "-m", "base")

	// Clean tree → no changes → "".
	if h := worktreeProgressHash(ctx, dir); h != "" {
		t.Fatalf("clean tree should hash to empty, got %q", h)
	}

	// New untracked file → non-empty hash.
	newFile := filepath.Join(dir, "new.txt")
	if err := os.WriteFile(newFile, []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hUntracked := worktreeProgressHash(ctx, dir)
	if hUntracked == "" {
		t.Fatal("untracked file should produce a non-empty hash")
	}

	// Modify a tracked file → hash changes.
	if err := os.WriteFile(base, []byte("modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hModified := worktreeProgressHash(ctx, dir)
	if hModified == "" {
		t.Fatal("tracked modification should produce a non-empty hash")
	}
	if hModified == hUntracked {
		t.Fatal("modifying a tracked file should change the hash")
	}

	// Writing the SAME content again → hash stable (no new content).
	if err := os.WriteFile(base, []byte("modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hStable := worktreeProgressHash(ctx, dir)
	if hStable != hModified {
		t.Fatalf("re-writing identical content changed the hash: %q != %q", hStable, hModified)
	}
}
