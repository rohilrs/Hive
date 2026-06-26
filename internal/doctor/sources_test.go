package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSourcesNoneBoundOrSyncedIsOK: empty SourcesStatus map (either no
// sources bound OR daemon just started and hasn't synced yet) → both
// checks ok with a message that acknowledges the ambiguity.
func TestSourcesNoneBoundOrSyncedIsOK(t *testing.T) {
	client := &stubRPCClient{sourcesStatus: map[string]SourceStatusEntry{}}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.staleness")
	if c.Status != StatusOK {
		t.Errorf("empty status: staleness=%s, want ok; msg=%q", c.Status, c.Message)
	}
	c2 := findCheck(t, checks, "sources.last_error")
	if c2.Status != StatusOK {
		t.Errorf("empty status: last_error=%s, want ok; msg=%q", c2.Status, c2.Message)
	}
}

// TestSourcesAllFreshIsOK: 2 sources both with LastSyncUnix=now-60s and
// no Error → staleness=ok, last_error=ok.
func TestSourcesAllFreshIsOK(t *testing.T) {
	now := time.Now().Unix()
	client := &stubRPCClient{sourcesStatus: map[string]SourceStatusEntry{
		"github": {LastSyncUnix: now - 60, Inserted: 1},
		"inbox":  {LastSyncUnix: now - 30, Inserted: 0},
	}}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.staleness")
	if c.Status != StatusOK {
		t.Errorf("all fresh: staleness=%s, want ok; msg=%q", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "2 source") {
		t.Errorf("expected message to mention 2 sources; got %q", c.Message)
	}
	c2 := findCheck(t, checks, "sources.last_error")
	if c2.Status != StatusOK {
		t.Errorf("all fresh: last_error=%s, want ok; msg=%q", c2.Status, c2.Message)
	}
	if !strings.Contains(c2.Message, "error-free") {
		t.Errorf("expected last_error message to say error-free; got %q", c2.Message)
	}
}

// TestSourcesStaleDetected: one source synced 20m ago (> 600s threshold)
// → staleness=warn with hint naming the source + age.
func TestSourcesStaleDetected(t *testing.T) {
	now := time.Now().Unix()
	client := &stubRPCClient{sourcesStatus: map[string]SourceStatusEntry{
		"github": {LastSyncUnix: now - 1200},
	}}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.staleness")
	if c.Status != StatusWarn {
		t.Errorf("stale source: staleness=%s, want warn; msg=%q", c.Status, c.Message)
	}
	if !strings.Contains(c.Hint, "github") {
		t.Errorf("expected hint to name the stale source; got %q", c.Hint)
	}
	if !strings.Contains(c.Hint, "1200") {
		t.Errorf("expected hint to mention the age (1200s); got %q", c.Hint)
	}
}

// TestSourcesLastErrorDetected: one source with non-empty Error →
// last_error=warn with hint listing it.
func TestSourcesLastErrorDetected(t *testing.T) {
	now := time.Now().Unix()
	client := &stubRPCClient{sourcesStatus: map[string]SourceStatusEntry{
		"linear": {LastSyncUnix: now - 30, Error: "rate limited"},
	}}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.last_error")
	if c.Status != StatusWarn {
		t.Errorf("errored source: last_error=%s, want warn; msg=%q", c.Status, c.Message)
	}
	if !strings.Contains(c.Hint, "linear") || !strings.Contains(c.Hint, "rate limited") {
		t.Errorf("expected hint to name source + error text; got %q", c.Hint)
	}
}

// TestSourcesBothStaleAndErrored: stale source + errored source → both
// checks warn.
func TestSourcesBothStaleAndErrored(t *testing.T) {
	now := time.Now().Unix()
	client := &stubRPCClient{sourcesStatus: map[string]SourceStatusEntry{
		"github": {LastSyncUnix: now - 1200},                       // stale, no error
		"linear": {LastSyncUnix: now - 30, Error: "401 not auth"},  // fresh, errored
	}}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	cs := findCheck(t, checks, "sources.staleness")
	if cs.Status != StatusWarn {
		t.Errorf("both: staleness=%s, want warn", cs.Status)
	}
	ce := findCheck(t, checks, "sources.last_error")
	if ce.Status != StatusWarn {
		t.Errorf("both: last_error=%s, want warn", ce.Status)
	}
}

// TestSourcesDaemonDownSkips: statusErr → both checks skipped.
func TestSourcesDaemonDownSkips(t *testing.T) {
	client := &stubRPCClient{statusErr: errSocketDown}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.staleness")
	if c.Status != StatusSkip {
		t.Errorf("daemon down: staleness=%s, want skip", c.Status)
	}
	c2 := findCheck(t, checks, "sources.last_error")
	if c2.Status != StatusSkip {
		t.Errorf("daemon down: last_error=%s, want skip", c2.Status)
	}
}

// TestSourcesRPCErrorPropagates: sources_status RPC returns an error →
// staleness=warn with the error text, last_error=skip.
func TestSourcesRPCErrorPropagates(t *testing.T) {
	client := &stubRPCClient{sourcesStatusErr: errors.New("boom")}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.staleness")
	if c.Status != StatusWarn {
		t.Errorf("rpc err: staleness=%s, want warn; msg=%q", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "boom") {
		t.Errorf("expected error text in message; got %q", c.Message)
	}
	c2 := findCheck(t, checks, "sources.last_error")
	if c2.Status != StatusSkip {
		t.Errorf("rpc err: last_error=%s, want skip", c2.Status)
	}
}

// TestSourcesNeverSyncedNotStale: LastSyncUnix=0 means "never synced
// yet" — treat as not stale (the daemon hasn't tried, not that it's
// failing to refresh). Note: a source only appears in the map AFTER
// its first sync attempt records state, so a 0 value here would
// indicate a deliberate "registered-but-not-yet-touched" entry — we
// guard for it to be safe.
func TestSourcesNeverSyncedNotStale(t *testing.T) {
	client := &stubRPCClient{sourcesStatus: map[string]SourceStatusEntry{
		"github": {LastSyncUnix: 0},
	}}
	checks := runSourcesChecks(context.Background(), t.TempDir(), client)

	c := findCheck(t, checks, "sources.staleness")
	if c.Status != StatusOK {
		t.Errorf("never synced: staleness=%s, want ok; msg=%q", c.Status, c.Message)
	}
}
