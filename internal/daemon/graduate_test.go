package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/graduate"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
)

// stubGraduateRunner answers audit calls with a fixed verdict and verify calls
// per confirmVerify (true = confirm every finding, so a C/H verdict blocks;
// false = refute every finding). Lets a test drive either gate outcome.
type stubGraduateRunner struct {
	verdict       graduate.GraduationVerdict
	confirmVerify bool
}

func (s stubGraduateRunner) RunRoamingTool(_ context.Context, _, _, _ string, tool anthropic.ToolDef, _ []string, _ int) (*anthropic.TurnOutput, error) {
	var raw json.RawMessage
	if tool.Name == "submit_finding_verification" {
		if s.confirmVerify {
			raw = json.RawMessage(`{"confirmed":true,"reason":"stub confirm"}`)
		} else {
			raw = json.RawMessage(`{"confirmed":false,"reason":"stub refute"}`)
		}
	} else {
		b, _ := json.Marshal(s.verdict)
		raw = b
	}
	return &anthropic.TurnOutput{StopReason: "tool_use", ToolCalls: []anthropic.ToolCall{{ID: "stub", Name: tool.Name, Input: raw}}}, nil
}

// errGraduateRunner simulates an audit-infrastructure failure: claude never
// produces a verdict (timeout, E2BIG, crash) so RunRoamingTool returns (nil, err).
type errGraduateRunner struct{ err error }

func (r errGraduateRunner) RunRoamingTool(_ context.Context, _, _, _ string, _ anthropic.ToolDef, _ []string, _ int) (*anthropic.TurnOutput, error) {
	return nil, r.err
}

// bodyCapturingGateway records the PR body passed to OpenPR so a test can assert
// the audit-skipped note made it into the body.
type bodyCapturingGateway struct {
	stubGateway
	body string
}

func (g *bodyCapturingGateway) OpenPR(_ context.Context, _, _, _, _, body string, _ bool) (string, error) {
	g.body = body
	return "https://github.com/stub/pr/forced", nil
}

// setupGraduateRepo builds a repo where "feature" is one commit AHEAD of "main"
// with behind==0 and no conflicts — the clean-graduation precondition shape.
// CheckFeatureBranch uses local refs (feature..target / target..feature) so no
// origin remote is required.
func setupGraduateRepo(t *testing.T) string {
	t.Helper()
	repo := initGitRepo(t) // on main, one "init" commit
	mustRun(t, repo, "git", "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, repo, "git", "add", ".")
	mustRun(t, repo, "git", "commit", "-m", "feature work")
	// Leave the canonical repo checked out ON "feature" — the real graduation
	// state (`hive plan` leaves the repo on the feature branch). This is a
	// regression guard: a NAMED worktree checkout (git worktree add -B feature)
	// would fail here with "feature is already used by worktree", so the test
	// exercises the detached-worktree provisioning that graduate must use.
	return repo
}

// TestRunGraduateOpensPROnCleanVerdict pins Stages 2-5: with all tasks
// done+satisfied, a feature branch ahead of target, empty shippability gates
// (Stage 3 skips), and a CLEAN audit verdict, runGraduate opens a PR and returns
// its URL.
func TestRunGraduateOpensPROnCleanVerdict(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	slug := "grad"
	repo := setupGraduateRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: slug, Slug: slug, Name: "Grad", Status: "active", RepoPath: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	// feature_branch = "feature"; target defaults to "main". Blank ALL Stage-3
	// gate commands (the global defaults populate them) — the finish-branch
	// gates AND the build validate_command (build-validate gate) AND
	// prepare_command — so Stage 3 skips entirely and the test exercises only
	// Stages 2/4/5.
	writePerProjectConfig(t, d.HiveDir(), slug,
		"[integration]\nfeature_branch = \"feature\"\n\n"+
			"[pipelines.build]\n"+
			"validate_command = \"\"\n\n"+
			"[pipelines.finish_branch]\n"+
			"format_command = \"\"\n"+
			"typecheck_command = \"\"\n"+
			"lint_command = \"\"\n"+
			"test_command = \"\"\n"+
			"prepare_command = \"\"\n")

	// One done + gate-satisfied task → Stage 1 passes.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x",
		Status: "done", GateState: sequence.GateSatisfied,
		Pipeline: "build", Priority: "P1",
	}); err != nil {
		t.Fatal(err)
	}

	d.prGateway = &stubGateway{}
	d.SetGraduateRunner(stubGraduateRunner{verdict: graduate.GraduationVerdict{
		Status: "COMPLETE", Summary: "all good",
	}})

	proj, err := d.store.GetProject(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}

	res := d.runGraduate(ctx, proj, graduateOpts{}, func(string) {})
	if res.Err != nil {
		t.Fatalf("runGraduate err: %v", res.Err)
	}
	if res.PRURL == "" {
		t.Fatal("expected a PR URL, got empty")
	}
	if res.Verdict == nil || res.Verdict.Blocks() {
		t.Fatalf("expected a non-blocking verdict, got %+v", res.Verdict)
	}
}

// TestRunGraduateBlocksOnConfirmedFinding pins Stage 4 blocking: when the audit
// verdict contains a confirmed High finding, runGraduate must return a non-nil
// error mentioning "completion audit found" and "1 High", must NOT open a PR,
// and must attach the blocking verdict to the result.
func TestRunGraduateBlocksOnConfirmedFinding(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	slug := "grad"
	repo := setupGraduateRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: slug, Slug: slug, Name: "Grad", Status: "active", RepoPath: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	writePerProjectConfig(t, d.HiveDir(), slug,
		"[integration]\nfeature_branch = \"feature\"\n\n"+
			"[pipelines.build]\n"+
			"validate_command = \"\"\n\n"+
			"[pipelines.finish_branch]\n"+
			"format_command = \"\"\n"+
			"typecheck_command = \"\"\n"+
			"lint_command = \"\"\n"+
			"test_command = \"\"\n"+
			"prepare_command = \"\"\n")

	// One done + gate-satisfied task → Stage 1 passes.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x",
		Status: "done", GateState: sequence.GateSatisfied,
		Pipeline: "build", Priority: "P1",
	}); err != nil {
		t.Fatal(err)
	}

	d.prGateway = &stubGateway{}
	d.SetGraduateRunner(stubGraduateRunner{
		confirmVerify: true,
		verdict: graduate.GraduationVerdict{
			Status:  "GAPS_FOUND",
			Summary: "incomplete",
			Findings: []graduate.Finding{
				{Severity: "High", Category: "Missing", Title: "missing thing", Evidence: "x.go:1"},
			},
		},
	})

	proj, err := d.store.GetProject(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}

	res := d.runGraduate(ctx, proj, graduateOpts{}, func(string) {})

	if res.Err == nil {
		t.Fatal("expected blocking error, got nil")
	}
	if !strings.Contains(res.Err.Error(), "completion audit found") {
		t.Errorf("err = %q, want it to mention 'completion audit found'", res.Err.Error())
	}
	if !strings.Contains(res.Err.Error(), "1 High") {
		t.Errorf("err = %q, want '1 High' from blockingSummary", res.Err.Error())
	}
	if res.PRURL != "" {
		t.Errorf("blocked run must not open a PR, got PRURL=%q", res.PRURL)
	}
	if res.Verdict == nil || !res.Verdict.Blocks() {
		t.Error("expected a blocking verdict on the result")
	}
}

// TestRunGraduateForcePastAuditInfraFailure pins FIX 4: under --force, a Stage-4
// audit-infrastructure failure (the runner returns an error, never a verdict)
// must NOT abort. The run proceeds to open the PR, and the PR body notes that the
// completion audit did not run.
func TestRunGraduateForcePastAuditInfraFailure(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	slug := "grad"
	repo := setupGraduateRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: slug, Slug: slug, Name: "Grad", Status: "active", RepoPath: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	writePerProjectConfig(t, d.HiveDir(), slug,
		"[integration]\nfeature_branch = \"feature\"\n\n"+
			"[pipelines.build]\n"+
			"validate_command = \"\"\n\n"+
			"[pipelines.finish_branch]\n"+
			"format_command = \"\"\n"+
			"typecheck_command = \"\"\n"+
			"lint_command = \"\"\n"+
			"test_command = \"\"\n"+
			"prepare_command = \"\"\n")
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x",
		Status: "done", GateState: sequence.GateSatisfied,
		Pipeline: "build", Priority: "P1",
	}); err != nil {
		t.Fatal(err)
	}

	gw := &bodyCapturingGateway{}
	d.prGateway = gw
	d.SetGraduateRunner(errGraduateRunner{err: fmt.Errorf("claude timed out before verdict")})

	proj, err := d.store.GetProject(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}

	res := d.runGraduate(ctx, proj, graduateOpts{Force: true}, func(string) {})
	if res.Err != nil {
		t.Fatalf("forced run must not return an error on audit-infra failure, got: %v", res.Err)
	}
	if res.PRURL == "" {
		t.Fatal("expected a PR URL (forced run should open the PR), got empty")
	}
	if res.Verdict != nil {
		t.Errorf("expected nil verdict when the audit did not run, got %+v", res.Verdict)
	}
	if !strings.Contains(gw.body, "DID NOT RUN") {
		t.Errorf("PR body should note the audit did not run; got:\n%s", gw.body)
	}
}

// TestRunGraduateSetsStageAndBuildSummary verifies that a clean dry-run stamps
// Stage == "complete" and a non-empty BuildSummary onto the returned result.
func TestRunGraduateSetsStageAndBuildSummary(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)
	slug := "grad"
	repo := setupGraduateRepo(t)
	if err := d.store.InsertProject(ctx, &store.Project{ID: slug, Slug: slug, Name: "G", Status: "active", RepoPath: &repo}); err != nil {
		t.Fatal(err)
	}
	writePerProjectConfig(t, d.HiveDir(), slug,
		"[integration]\nfeature_branch = \"feature\"\n\n[pipelines.build]\nvalidate_command = \"\"\n\n[pipelines.finish_branch]\nformat_command = \"\"\ntypecheck_command = \"\"\nlint_command = \"\"\ntest_command = \"\"\nprepare_command = \"\"\n")
	if err := d.store.InsertTask(ctx, &store.Task{ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x", Status: "done", GateState: sequence.GateSatisfied, Pipeline: "build", Priority: "P1"}); err != nil {
		t.Fatal(err)
	}
	d.prGateway = &stubGateway{}
	d.SetGraduateRunner(stubGraduateRunner{verdict: graduate.GraduationVerdict{
		Status: "COMPLETE", Summary: "all good",
	}})
	proj, _ := d.store.GetProject(ctx, slug)
	res := d.runGraduate(ctx, proj, graduateOpts{DryRun: true}, func(string) {})
	if res.Stage != "complete" {
		t.Errorf("Stage=%q want complete", res.Stage)
	}
	if res.BuildSummary == "" {
		t.Errorf("BuildSummary should be set")
	}
}

// TestGraduatePreconditionsBlockIncomplete pins Stage 1: graduation must refuse
// while any task for the project is not done+satisfied. The incomplete task
// fails at Stage 1 before Stage 2 (branch health) is reached, so no git repo
// is required here.
func TestGraduatePreconditionsBlockIncomplete(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)

	slug := "grad"
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: slug, Slug: slug, Name: slug, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	// One task that is NOT done (running) — Stage 1 must flag it.
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: slug + "-t", ProjectID: slug, Source: "inbox", Title: "x",
		Status: "running", GateState: sequence.GateSatisfied,
		Pipeline: "build", Priority: "P1",
	}); err != nil {
		t.Fatal(err)
	}

	proj, err := d.store.GetProject(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}

	res := d.graduatePreconditions(ctx, proj)
	if res.OK {
		t.Fatal("expected preconditions to fail with an incomplete task")
	}
	if !strings.Contains(res.Reason, "not complete") {
		t.Errorf("reason=%q", res.Reason)
	}
}

func TestPersistGraduateResultWritesBothFiles(t *testing.T) {
	d := newTestDaemon(t)
	rec := graduate.GraduateResult{Slug: "p", Mode: "dry-run", Outcome: "blocked", Stage: "audit",
		Verdict: &graduate.GraduationVerdict{Status: "GAPS_FOUND", Findings: []graduate.Finding{{Severity: "High", Title: "x"}}}}
	d.persistGraduateResult(rec)
	jsonPath := filepath.Join(d.HiveDir(), "graduate-p-result.json")
	mdPath := filepath.Join(d.HiveDir(), "graduate-p-result.md")
	jb, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var got graduate.GraduateResult
	if err := json.Unmarshal(jb, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Outcome != "blocked" || got.Verdict == nil || got.Verdict.Status != "GAPS_FOUND" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if _, err := os.ReadFile(mdPath); err != nil {
		t.Errorf("md not written: %v", err)
	}
}
