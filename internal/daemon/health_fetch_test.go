package daemon

import (
	"testing"

	"github.com/rohilrs/Hive/internal/branchhealth"
)

// TestFetchForHealthRefreshesOriginState: a feature branch whose origin counterpart
// advanced elsewhere reads "synced" from cached refs, but "behind" after fetchForHealth.
func TestFetchForHealthRefreshesOriginState(t *testing.T) {
	origin := t.TempDir()
	mustGit(t, "", "init", "-q", "--bare", "-b", "main", origin)
	repoA := t.TempDir()
	mustGit(t, "", "clone", "-q", origin, repoA)
	writeF(t, repoA, "f.txt", "base\n")
	mustGit(t, repoA, "add", "-A")
	mustGit(t, repoA, "commit", "-qm", "base")
	mustGit(t, repoA, "branch", "feature")
	mustGit(t, repoA, "branch", "target")
	mustGit(t, repoA, "push", "-q", "origin", "main", "feature", "target")

	// A second clone advances origin/feature.
	repoB := t.TempDir()
	mustGit(t, "", "clone", "-q", origin, repoB)
	mustGit(t, repoB, "checkout", "-q", "feature")
	writeF(t, repoB, "f.txt", "advanced on origin\n")
	mustGit(t, repoB, "add", "-A")
	mustGit(t, repoB, "commit", "-qm", "origin advance")
	mustGit(t, repoB, "push", "-q", "origin", "feature")

	// repoA hasn't fetched: local feature looks synced with its stale origin ref.
	pre, err := branchhealth.CheckFeatureBranch(repoA, "feature", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if pre.OriginState != "synced" {
		t.Fatalf("pre-fetch OriginState=%q want synced (stale snapshot)", pre.OriginState)
	}

	fetchForHealth(repoA, "feature", "target")

	post, err := branchhealth.CheckFeatureBranch(repoA, "feature", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if post.OriginState != "behind" {
		t.Fatalf("post-fetch OriginState=%q want behind (live)", post.OriginState)
	}
}
