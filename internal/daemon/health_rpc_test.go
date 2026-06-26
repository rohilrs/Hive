package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/store"
)

// seedRemediationProject inserts a project pointing at repo and writes a
// per-project config whose integration.feature_branch = "feature". The target
// branch defaults to "main" (effectiveTargetBranchForProject fallback).
func seedRemediationProject(t *testing.T, d *Daemon, repo string) {
	t.Helper()
	if err := d.store.InsertProject(context.Background(), &store.Project{
		ID: "p1", Slug: "slug", Name: "Demo", RepoPath: &repo, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	writePerProjectConfig(t, d.HiveDir(), "slug",
		"[integration]\nfeature_branch = \"feature\"\n")
}

func TestHandleHealthRemediate_RejectsBadAction(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "action": "bogus"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil || !strings.Contains(rerr.Message, "rebase or merge") {
		t.Fatalf("want bad-action error, got %v", rerr)
	}
}

func TestHandleHealthRemediate_MissingProject(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params, _ := json.Marshal(map[string]any{"project_slug": "ghost", "action": "rebase"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil || !strings.Contains(rerr.Message, "project not found") {
		t.Fatalf("want not-found error, got %v", rerr)
	}
}

func TestHandleHealthRemediate_RebaseHappyPath(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	repo := setupDivergedRepo(t) // from feature_branch_remediate_test.go
	seedRemediationProject(t, d, repo)

	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "action": "rebase"})
	out, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr != nil {
		t.Fatalf("handleHealthRemediate: %v", rerr)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["behind"].(float64) != 0 {
		t.Errorf("behind after rebase = %v, want 0", res["behind"])
	}
	if res["clean"] != true {
		t.Errorf("clean = %v, want true", res["clean"])
	}
}

func TestHandleHealthRemediate_MergeHappyPath(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	repo := setupDivergedRepo(t)
	seedRemediationProject(t, d, repo)

	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "action": "merge"})
	out, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr != nil {
		t.Fatalf("handleHealthRemediate: %v", rerr)
	}
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["behind"].(float64) != 0 {
		t.Errorf("behind after merge = %v, want 0", res["behind"])
	}
	if res["clean"] != true {
		t.Errorf("clean = %v, want true", res["clean"])
	}
}

func TestHandleHealthRemediate_RefusesDirty(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	repo := setupDivergedRepo(t)
	seedRemediationProject(t, d, repo)
	if err := os.WriteFile(filepath.Join(repo, "uncommitted.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "action": "rebase"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil || !strings.Contains(rerr.Message, "uncommitted changes") {
		t.Fatalf("want dirty refusal, got %v", rerr)
	}
}

// TestHandleHealthRemediate_AllowsRoadmapOnlyDirty verifies that when the ONLY
// uncommitted change in the working tree is the reconciler roadmap file
// (docs/superpowers/roadmaps/<slug>.md), handleHealthRemediate does NOT refuse
// with a "uncommitted changes" error — it proceeds with remediation.
func TestHandleHealthRemediate_AllowsRoadmapOnlyDirty(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)

	// Build a repo where:
	//  1. The roadmap file is tracked (committed) on main.
	//  2. feature and main have diverged by one commit each on different files
	//     (conflict-free, so feature is 1 behind main).
	//  3. The working tree has only the roadmap file dirty (not committed).
	repo := initGitRepo(t)

	// Commit the baseline roadmap file on main so it is a tracked file.
	roadmapPath := filepath.Join(repo, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(roadmapPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roadmapPath, "slug.md"), []byte("# roadmap v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "add roadmap baseline")

	// Branch feature off main's current tip.
	mustRun(t, repo, "git", "branch", "feature")

	// Advance main by one commit on its own file (feature is now 1 behind).
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte("main work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "main ahead")

	// Add a feature commit on a different file → diverged, conflict-free.
	mustRun(t, repo, "git", "checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "feature work")

	// Leave on main (canonical idle state).
	mustRun(t, repo, "git", "checkout", "main")

	// Seed the project.
	seedRemediationProject(t, d, repo)

	// Dirty ONLY the roadmap file — simulating the reconciler updating it.
	if err := os.WriteFile(filepath.Join(roadmapPath, "slug.md"), []byte("# roadmap v2 (reconciler update)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The handler must NOT refuse for dirtiness; the dirty-guard is the only
	// blocker here (no conflicts, feature is behind main by 1).
	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "action": "rebase"})
	out, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr != nil {
		if strings.Contains(rerr.Message, "uncommitted changes") {
			t.Fatalf("roadmap-only dirty was incorrectly refused: %v", rerr)
		}
		t.Fatalf("handleHealthRemediate: unexpected error: %v", rerr)
	}
	// Verify the remediation actually resolved the behind state.
	var res map[string]any
	_ = json.Unmarshal(out, &res)
	if res["behind"].(float64) != 0 {
		t.Errorf("behind after rebase = %v, want 0", res["behind"])
	}
}

func TestHandleHealthRemediate_RefusesPredictedConflict(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	// Same-file divergence → branchhealth predicts a conflict.
	repo := initGitRepo(t)
	mustRun(t, repo, "git", "branch", "feature")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "main shared")
	mustRun(t, repo, "git", "checkout", "feature")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "feature shared")
	mustRun(t, repo, "git", "checkout", "main")
	seedRemediationProject(t, d, repo)

	params, _ := json.Marshal(map[string]any{"project_slug": "slug", "action": "rebase"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil || !strings.Contains(rerr.Message, "conflicts") {
		t.Fatalf("want conflict refusal, got %v", rerr)
	}
}

func TestHandleHealthRemediate_RejectsEmptySlug(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	params, _ := json.Marshal(map[string]any{"action": "rebase"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil {
		t.Fatal("want error for empty project_slug, got nil")
	}
}

func TestHandleHealthRemediate_RefusesMissingRepoPath(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	if err := d.store.InsertProject(context.Background(), &store.Project{
		ID: "p2", Slug: "norepo", Name: "NoRepo", RepoPath: nil, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"project_slug": "norepo", "action": "rebase"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil || !strings.Contains(rerr.Message, "repo_path") {
		t.Fatalf("want repo_path error, got %v", rerr)
	}
}

func TestHandleHealthRemediate_RefusesUnconfiguredFeatureBranch(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	repo := initGitRepo(t)
	if err := d.store.InsertProject(context.Background(), &store.Project{
		ID: "p3", Slug: "nofeat", Name: "NoFeat", RepoPath: &repo, Status: "active",
	}); err != nil {
		t.Fatalf("InsertProject: %v", err)
	}
	// No per-project config written → effectiveFeatureBranchForProject returns "".
	params, _ := json.Marshal(map[string]any{"project_slug": "nofeat", "action": "rebase"})
	_, rerr := srv.handleHealthRemediate(context.Background(), params)
	if rerr == nil || !strings.Contains(rerr.Message, "no feature branch configured") {
		t.Fatalf("want unconfigured-feature-branch error, got %v", rerr)
	}
}
