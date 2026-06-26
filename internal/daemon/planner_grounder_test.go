package daemon

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

// TestPlannerGrounderRef pins the grounding-ref selection AND resolution: the
// planner grounds the FEATURE branch when it exists (local or origin), else the
// TARGET branch — and resolves whichever to a git-USABLE ref. A bare branch name
// only resolves to a LOCAL branch, so an origin-only branch (the common
// fresh-clone case) must be grounded as `origin/<branch>`, else git grep + the
// on-demand scavenger index both fail to resolve it.
func TestPlannerGrounderRef(t *testing.T) {
	ctx := context.Background()
	mustGit := func(repo string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	headSHA := func(repo string) string {
		out, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").CombinedOutput()
		return strings.TrimSpace(string(out))
	}

	cases := []struct {
		name          string
		featureBranch string
		targetBranch  string
		makeFeature   string // "" | "local" | "origin"
		makeTarget    string // "" | "local" | "origin"
		wantRef       string
	}{
		{"feature local -> bare", "spec/feat", "release", "local", "local", "spec/feat"},
		{"feature origin-only -> origin/<fb>", "spec/feat", "release", "origin", "local", "origin/spec/feat"},
		{"feature absent -> target local", "spec/ghost", "release", "", "local", "release"},
		{"no feature, target origin-only -> origin/<target>", "", "staging", "", "origin", "origin/staging"},
		{"no feature, target local -> bare", "", "release", "", "local", "release"},
		{"neither -> main (local default)", "", "", "", "", "main"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newTestDaemon(t)
			repo := initGitRepo(t) // on main, one commit
			sha := headSHA(repo)
			mk := func(branch, how string) {
				switch how {
				case "local":
					mustGit(repo, "branch", branch)
				case "origin":
					mustGit(repo, "update-ref", "refs/remotes/origin/"+branch, sha)
				}
			}
			if c.featureBranch != "" {
				mk(c.featureBranch, c.makeFeature)
			}
			if c.targetBranch != "" && c.targetBranch != "main" {
				mk(c.targetBranch, c.makeTarget)
			}

			slug := "g"
			if err := d.store.InsertProject(ctx, &store.Project{
				ID: slug, Slug: slug, Name: slug, Status: "active", RepoPath: &repo,
			}); err != nil {
				t.Fatal(err)
			}
			cfg := "[integration]\n"
			if c.featureBranch != "" {
				cfg += "feature_branch = \"" + c.featureBranch + "\"\n"
			}
			cfg += "\n[scheduler]\n"
			if c.targetBranch != "" {
				cfg += "target_branch = \"" + c.targetBranch + "\"\n"
			}
			writePerProjectConfig(t, d.HiveDir(), slug, cfg)

			g := d.plannerGrounderFor(slug, repo)
			if g == nil {
				t.Fatal("expected a grounder, got nil")
			}
			if got := g.Ref(); got != c.wantRef {
				t.Errorf("Ref()=%q want %q", got, c.wantRef)
			}
		})
	}
}

// TestMaybeFetchGroundBranchThrottle pins the throttle: the first call attempts a
// fetch, a second within the window is skipped (plannerGrounderFor runs per
// planner tool-call, so the fetch must not hit the network every turn). The
// fetch itself fails harmlessly here (no real remote) — we assert the throttle,
// not the fetch result.
func TestMaybeFetchGroundBranchThrottle(t *testing.T) {
	d := newTestDaemon(t)
	repo := initGitRepo(t)
	if !d.maybeFetchGroundBranch(repo, "main") {
		t.Error("first call should attempt a fetch (not throttled)")
	}
	if d.maybeFetchGroundBranch(repo, "main") {
		t.Error("second call within the throttle window should be skipped")
	}
	// A different branch is tracked independently → not throttled.
	if !d.maybeFetchGroundBranch(repo, "other") {
		t.Error("a different branch should not be throttled by the first")
	}
}
