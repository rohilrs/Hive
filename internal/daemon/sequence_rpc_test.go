package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// TestLoadRoadmapAndGateFromFeatureBranch covers the shared-repo case: the
// roadmap + spec are committed on the project's feature branch and ABSENT from
// the working tree (checked out on another branch). loadProjectRoadmap and the
// enable gate must read them from the branch instead of failing "not found".
func TestLoadRoadmapAndGateFromFeatureBranch(t *testing.T) {
	ctx := context.Background()
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	repo := initGitRepo(t) // on main, one commit, no roadmap

	mustGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Put roadmap + linked spec on feat/x, then return the working tree to main
	// so neither file exists on disk.
	mustGit("checkout", "-q", "-b", "feat/x")
	writeFile("docs/superpowers/roadmaps/seqp.md",
		"# r\n\n## Phase 1: First\n\nSee [spec](docs/superpowers/specs/p1.md)\n")
	writeFile("docs/superpowers/specs/p1.md", "# spec\n")
	mustGit("add", "-A")
	mustGit("commit", "-q", "-m", "docs on feature branch")
	mustGit("checkout", "-q", "main")
	if _, err := os.Stat(filepath.Join(repo, "docs/superpowers/roadmaps/seqp.md")); !os.IsNotExist(err) {
		t.Fatal("precondition: roadmap should be absent from the working tree on main")
	}

	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repo,
	}); err != nil {
		t.Fatal(err)
	}
	writePerProjectConfig(t, d.HiveDir(), "seqp", "[integration]\nfeature_branch = \"feat/x\"\n")

	// Read must succeed from the feature branch.
	rm, _, err := d.loadProjectRoadmap(&store.Project{ID: "p1", Slug: "seqp", RepoPath: &repo})
	if err != nil {
		t.Fatalf("loadProjectRoadmap should read from feat/x, got: %v", err)
	}
	if len(rm.Phases) != 1 || rm.Phases[0].Number != "1" {
		t.Fatalf("parsed phases = %+v, want one phase numbered 1", rm.Phases)
	}

	// One pending task in phase 1 makes the phase ACTIVE so the gate reaches the
	// spec-existence check (which must also resolve the spec on the branch).
	if err := d.store.InsertTask(ctx, &store.Task{
		ID: "t1", ProjectID: "p1", Source: "inbox", Title: "t1", Status: "pending",
		Metadata: map[string]any{"roadmap_phase": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	proj, _ := d.store.GetProjectBySlug(ctx, "seqp")
	if err := srv.checkEnableGate(ctx, proj); err != nil {
		t.Fatalf("checkEnableGate should pass reading spec from feat/x, got: %v", err)
	}
}

// writeSeqRoadmap creates docs/superpowers/roadmaps/<slug>.md under repo with
// the given body. Named to avoid collision with writeRoadmap in roadmap_rpc_test.go.
func writeSeqRoadmap(t *testing.T, repo, slug, body string) {
	t.Helper()
	dir := filepath.Join(repo, "docs", "superpowers", "roadmaps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDispatchModeTransitions(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	rp := initGitRepo(t)
	_ = d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "demo", Name: "D", Status: "active", RepoPath: &rp})

	// auto_all: writes scheduler config, no dispatcher row, no gate.
	if err := srv.applyDispatchMode(ctx, mustProj(t, d, "demo"), "auto_all", "", ""); err != nil {
		t.Fatalf("auto_all: %v", err)
	}
	if got := d.scheduler.effectiveDispatchModeForProject("demo"); got != "auto_all" {
		t.Errorf("dispatch mode = %q, want auto_all", got)
	}

	// sequenced WITHOUT a roadmap → gate rejects (no config change).
	if err := srv.applyDispatchMode(ctx, mustProj(t, d, "demo"), "sequenced", "main", "pr_opened"); err == nil {
		t.Error("sequenced without a roadmap must fail the gate")
	}

	// manual: scheduler config = manual, no dispatcher.
	if err := srv.applyDispatchMode(ctx, mustProj(t, d, "demo"), "manual", "", ""); err != nil {
		t.Fatalf("manual: %v", err)
	}
	if got := d.scheduler.effectiveDispatchModeForProject("demo"); got != "manual" {
		t.Errorf("dispatch mode = %q, want manual", got)
	}
}

// mustProj fetches a project or fails the test.
func mustProj(t *testing.T, d *Daemon, slug string) *store.Project {
	t.Helper()
	p, err := d.store.GetProjectBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("GetProjectBySlug %s: %v", slug, err)
	}
	return p
}

func TestSequencePauseResumeSkip(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "seqp", Name: "S", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened"}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.InsertTask(ctx, &store.Task{ID: "t1", ProjectID: "p1", Source: "inbox", Title: "x", Status: "needs_attention", GateState: "built"}); err != nil {
		t.Fatal(err)
	}
	call := func(method string, params map[string]any) *rpc.RPCError {
		raw, _ := json.Marshal(params)
		var rerr *rpc.RPCError
		switch method {
		case rpc.MethodSequencePause:
			_, rerr = srv.handleSequencePause(ctx, raw)
		case rpc.MethodSequenceResume:
			_, rerr = srv.handleSequenceResume(ctx, raw)
		case rpc.MethodSequenceSkip:
			_, rerr = srv.handleSequenceSkip(ctx, raw)
		}
		return rerr
	}
	if e := call(rpc.MethodSequencePause, map[string]any{"project_slug": "seqp"}); e != nil {
		t.Fatalf("pause: %v", e)
	}
	if disp, _ := d.store.GetSequenceDispatcher(ctx, "p1"); disp.Status != "paused" {
		t.Errorf("status = %q, want paused", disp.Status)
	}
	if e := call(rpc.MethodSequenceResume, map[string]any{"project_slug": "seqp"}); e != nil {
		t.Fatalf("resume: %v", e)
	}
	if disp, _ := d.store.GetSequenceDispatcher(ctx, "p1"); disp.Status != "active" {
		t.Errorf("status = %q, want active", disp.Status)
	}
	if e := call(rpc.MethodSequenceSkip, map[string]any{"task_id": "t1"}); e != nil {
		t.Fatalf("skip: %v", e)
	}
	if tk, _ := d.store.GetTask(ctx, "t1"); tk.GateState != "skipped" {
		t.Errorf("gate = %q, want skipped", tk.GateState)
	}
}

func TestSequencePauseSkipErrors(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	// Project with NO dispatcher row.
	if err := d.store.InsertProject(ctx, &store.Project{ID: "p1", Slug: "noseq", Name: "S", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	js := func(m map[string]any) json.RawMessage { b, _ := json.Marshal(m); return b }

	// pause unknown slug -> ProjectNotFound.
	if _, e := srv.handleSequencePause(ctx, js(map[string]any{"project_slug": "ghost"})); e == nil || e.Code != rpc.ErrProjectNotFound {
		t.Errorf("pause unknown slug: got %v, want ErrProjectNotFound", e)
	}
	// pause project with no dispatcher row -> InvalidParams ("not enabled").
	if _, e := srv.handleSequencePause(ctx, js(map[string]any{"project_slug": "noseq"})); e == nil || e.Code != rpc.ErrInvalidParams {
		t.Errorf("pause not-enabled: got %v, want ErrInvalidParams", e)
	}
	// skip unknown task -> TaskNotFound.
	if _, e := srv.handleSequenceSkip(ctx, js(map[string]any{"task_id": "nope"})); e == nil || e.Code != rpc.ErrTaskNotFound {
		t.Errorf("skip unknown task: got %v, want ErrTaskNotFound", e)
	}
	// missing required params -> InvalidParams.
	if _, e := srv.handleSequencePause(ctx, js(map[string]any{})); e == nil || e.Code != rpc.ErrInvalidParams {
		t.Errorf("pause missing slug: got %v, want ErrInvalidParams", e)
	}
}

func TestSequenceEnableGateAndStatus(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	repo := t.TempDir()
	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}

	enable := func() *rpc.RPCError {
		raw, _ := json.Marshal(map[string]any{"project_slug": "seqp"})
		_, rerr := srv.handleSequenceEnable(ctx, raw)
		return rerr
	}

	// No roadmap -> rejected.
	if enable() == nil {
		t.Fatal("expected enable to fail without a roadmap")
	}
	// Roadmap phase 1 links a missing spec -> rejected.
	writeSeqRoadmap(t, repo, "seqp", "## Phase 1: First\n\nSee [spec](docs/superpowers/specs/p1.md)\n")
	if enable() == nil {
		t.Fatal("expected enable to fail when phase 1 spec missing")
	}
	// Create the spec -> enable succeeds.
	specDir := filepath.Join(repo, "docs", "superpowers", "specs")
	_ = os.MkdirAll(specDir, 0o755)
	_ = os.WriteFile(filepath.Join(specDir, "p1.md"), []byte("# spec"), 0o644)
	if e := enable(); e != nil {
		t.Fatalf("enable failed: %v", e)
	}
	if _, err := d.store.GetSequenceDispatcher(ctx, "p1"); err != nil {
		t.Fatalf("dispatcher row missing: %v", err)
	}
	if got := d.scheduler.effectiveDispatchModeForProject("seqp"); got != "sequenced" {
		t.Errorf("dispatch_mode = %q, want sequenced", got)
	}

	rawS, _ := json.Marshal(map[string]any{"project_slug": "seqp"})
	out, rerr := srv.handleSequenceStatus(ctx, rawS)
	if rerr != nil {
		t.Fatalf("status err: %v", rerr)
	}
	var resp struct {
		ActivePhase string `json:"active_phase"`
		Target      string `json:"target"`
		Phases      []struct {
			Number string `json:"number"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActivePhase != "1" || len(resp.Phases) != 1 {
		t.Errorf("status = %+v, want active 1 / 1 phase", resp)
	}
	// sequence.status now surfaces the resolved target branch (defaults to "main"
	// since no target_branch was configured for this project).
	if resp.Target != "main" {
		t.Errorf("target = %q, want main", resp.Target)
	}
}

// TestProjectListDispatchMode verifies project.list surfaces each project's
// resolved dispatch mode: a bare project fails closed to "manual", while a
// project that has been enabled for sequenced dispatch reports "sequenced".
func TestProjectListDispatchMode(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()

	// Bare project: no per-project scheduler override -> "manual".
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "pbare", Slug: "bare", Name: "Bare", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}

	// Sequenced project: enable via the same path the RPC uses (writes a
	// per-project [scheduler] dispatch_mode = sequenced override).
	repo := t.TempDir()
	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "pseq", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}
	writeSeqRoadmap(t, repo, "seqp", "## Phase 1: First\n\nSee [spec](docs/superpowers/specs/p1.md)\n")
	specDir := filepath.Join(repo, "docs", "superpowers", "specs")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "p1.md"), []byte("# spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	rawEn, _ := json.Marshal(map[string]any{"project_slug": "seqp"})
	if _, e := srv.handleSequenceEnable(ctx, rawEn); e != nil {
		t.Fatalf("enable: %v", e)
	}

	out, rerr := srv.handleListProjects(ctx)
	if rerr != nil {
		t.Fatalf("project.list: %v", rerr)
	}
	var views []rpc.ProjectView
	if err := json.Unmarshal(out, &views); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range views {
		got[v.Slug] = v.DispatchMode
	}
	if got["bare"] != "manual" {
		t.Errorf("bare DispatchMode = %q, want manual", got["bare"])
	}
	if got["seqp"] != "sequenced" {
		t.Errorf("seqp DispatchMode = %q, want sequenced", got["seqp"])
	}
}

func TestSequenceEnablePolicyAllowSet(t *testing.T) {
	policies := []string{"pr_opened", "human_merge", "auto_merge_on_green", "manual"}
	for _, policy := range policies {
		t.Run(policy, func(t *testing.T) {
			d := newTestDaemon(t)
			srv := NewRPCServer(d)
			ctx := context.Background()
			repo := t.TempDir()
			repoPath := repo
			if err := d.store.InsertProject(ctx, &store.Project{
				ID: "p1", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repoPath,
			}); err != nil {
				t.Fatal(err)
			}
			// Valid roadmap + spec so the enable gate passes and we reach the
			// policy check.
			writeSeqRoadmap(t, repo, "seqp", "## Phase 1: First\n\nSee [spec](docs/superpowers/specs/p1.md)\n")
			specDir := filepath.Join(repo, "docs", "superpowers", "specs")
			if err := os.MkdirAll(specDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(specDir, "p1.md"), []byte("# spec"), 0o644); err != nil {
				t.Fatal(err)
			}

			raw, _ := json.Marshal(map[string]any{"project_slug": "seqp", "policy": policy})
			if _, e := srv.handleSequenceEnable(ctx, raw); e != nil {
				t.Fatalf("enable policy %q: %v", policy, e)
			}
			disp, err := d.store.GetSequenceDispatcher(ctx, "p1")
			if err != nil {
				t.Fatalf("dispatcher row missing: %v", err)
			}
			if disp.AdvancementPolicy != policy {
				t.Errorf("AdvancementPolicy = %q, want %q", disp.AdvancementPolicy, policy)
			}
		})
	}
}

func TestSequenceEnableRejectsUnknownPolicy(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	repo := t.TempDir()
	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}
	// Valid roadmap + spec so the enable gate passes and the unknown policy is
	// the thing that gets rejected (not the gate).
	writeSeqRoadmap(t, repo, "seqp", "## Phase 1: First\n\nSee [spec](docs/superpowers/specs/p1.md)\n")
	specDir := filepath.Join(repo, "docs", "superpowers", "specs")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "p1.md"), []byte("# spec"), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{"project_slug": "seqp", "policy": "bogus"})
	_, e := srv.handleSequenceEnable(ctx, raw)
	if e == nil || e.Code != rpc.ErrInvalidParams {
		t.Fatalf("enable bogus policy: got %v, want ErrInvalidParams", e)
	}
	// No dispatcher row should have been created.
	if _, err := d.store.GetSequenceDispatcher(ctx, "p1"); err == nil {
		t.Errorf("dispatcher row created for rejected policy")
	}
}

func TestSequenceAdvance(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	repo := t.TempDir()
	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}
	// 1-phase roadmap on disk so loadProjectRoadmap/derivePlan work.
	writeSeqRoadmap(t, repo, "seqp", "## Phase 1: First\n\nSee [spec](docs/superpowers/specs/p1.md)\n")

	// Three tasks all in roadmap phase 1: two awaiting_merge, one built. The
	// roadmap_phase metadata is what derivePlan keys on to place them in phase 1.
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := d.store.InsertTask(ctx, &store.Task{
			ID: id, ProjectID: "p1", Source: "inbox", Title: id, Status: "needs_attention",
			Metadata: map[string]any{"roadmap_phase": "1"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.store.UpdateTaskGateState(ctx, "t1", sequence.GateAwaitingMerge); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpdateTaskGateState(ctx, "t2", sequence.GateAwaitingMerge); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpdateTaskGateState(ctx, "t3", sequence.GateBuilt); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{"project_slug": "seqp"})
	out, rerr := srv.handleSequenceAdvance(ctx, raw)
	if rerr != nil {
		t.Fatalf("advance: %v", rerr)
	}

	var r struct {
		Advanced    int    `json:"advanced"`
		ActivePhase string `json:"active_phase"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.Advanced != 2 {
		t.Errorf("advanced = %d, want 2", r.Advanced)
	}
	if r.ActivePhase != "1" {
		t.Errorf("active_phase = %q, want 1", r.ActivePhase)
	}

	// The two awaiting_merge tasks are now satisfied.
	for _, id := range []string{"t1", "t2"} {
		tk, err := d.store.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.GateState != sequence.GateSatisfied {
			t.Errorf("%s gate = %q, want satisfied", id, tk.GateState)
		}
	}
	// The built task is untouched.
	tk3, err := d.store.GetTask(ctx, "t3")
	if err != nil {
		t.Fatal(err)
	}
	if tk3.GateState != sequence.GateBuilt {
		t.Errorf("t3 gate = %q, want built (unchanged)", tk3.GateState)
	}
}

// TestSequenceDisableSweepsAwaitingMerge verifies that disabling a sequenced
// project sweeps its awaiting_merge tasks to satisfied so they don't strand
// invisibly once the dispatcher row (and the merge-poller's attention) is gone.
func TestSequenceDisableSweepsAwaitingMerge(t *testing.T) {
	d := newTestDaemon(t)
	srv := NewRPCServer(d)
	ctx := context.Background()
	repo := t.TempDir()
	repoPath := repo
	if err := d.store.InsertProject(ctx, &store.Project{
		ID: "p1", Slug: "seqp", Name: "SeqP", Status: "active", RepoPath: &repoPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
		ProjectID: "p1", Status: "active", AdvancementPolicy: "human_merge",
	}); err != nil {
		t.Fatal(err)
	}
	// a awaiting_merge (swept), b built (untouched), c already satisfied (no-op).
	for _, id := range []string{"a", "b", "c"} {
		if err := d.store.InsertTask(ctx, &store.Task{
			ID: id, ProjectID: "p1", Source: "inbox", Title: id, Status: "done",
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.store.UpdateTaskGateState(ctx, "a", sequence.GateAwaitingMerge)
	_ = d.store.UpdateTaskGateState(ctx, "b", sequence.GateBuilt)
	_ = d.store.UpdateTaskGateState(ctx, "c", sequence.GateSatisfied)

	raw, _ := json.Marshal(map[string]any{"project_slug": "seqp"})
	if _, rerr := srv.handleSequenceDisable(ctx, raw); rerr != nil {
		t.Fatalf("disable: %v", rerr)
	}

	if tk, _ := d.store.GetTask(ctx, "a"); tk.GateState != sequence.GateSatisfied {
		t.Errorf("a gate = %q, want satisfied (swept)", tk.GateState)
	}
	if tk, _ := d.store.GetTask(ctx, "b"); tk.GateState != sequence.GateBuilt {
		t.Errorf("b gate = %q, want built (untouched)", tk.GateState)
	}
	// Dispatcher row is gone (reverted to manual).
	if _, err := d.store.GetSequenceDispatcher(ctx, "p1"); err == nil {
		t.Errorf("dispatcher row still present after disable")
	}
}

func TestHandleSequenceComplete(t *testing.T) {
	// Helper to build a standard 2-phase roadmap on disk.
	setup := func(t *testing.T) (*Daemon, *RPCServer, context.Context, string) {
		t.Helper()
		d := newTestDaemon(t)
		srv := NewRPCServer(d)
		ctx := context.Background()
		repo := t.TempDir()
		repoPath := repo
		if err := d.store.InsertProject(ctx, &store.Project{
			ID: "p1", Slug: "comp", Name: "Comp", Status: "active", RepoPath: &repoPath,
		}); err != nil {
			t.Fatal(err)
		}
		// 2-phase roadmap so we can verify active_phase advances to "2" after completing "1".
		writeSeqRoadmap(t, repo, "comp",
			"## Phase 1: First\n\n**Status:** In progress\n\n## Phase 2: Second\n\n**Status:** Pending\n")
		return d, srv, ctx, repo
	}

	call := func(t *testing.T, srv *RPCServer, ctx context.Context, slug, phase string) (map[string]any, *rpc.RPCError) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"project_slug": slug, "phase": phase})
		out, rerr := srv.handleSequenceComplete(ctx, raw)
		if rerr != nil {
			return nil, rerr
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		return m, nil
	}

	t.Run("zero-task phase completes and advances active phase", func(t *testing.T) {
		d, srv, ctx, _ := setup(t)
		// Phase 1 has zero tasks: MarkPhaseComplete should succeed.
		// Need a dispatcher row so derivePlan can load completedPhases.
		if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
			ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
		}); err != nil {
			t.Fatal(err)
		}
		res, rerr := call(t, srv, ctx, "comp", "1")
		if rerr != nil {
			t.Fatalf("complete zero-task phase: %v", rerr)
		}
		// active_phase should now be "2" (next phase).
		if res["active_phase"] != "2" {
			t.Errorf("active_phase = %v, want 2", res["active_phase"])
		}
		if res["phase"] != "1" {
			t.Errorf("phase = %v, want 1", res["phase"])
		}
		// The dispatcher's completed_phases should record phase "1".
		disp, err := d.store.GetSequenceDispatcher(ctx, "p1")
		if err != nil {
			t.Fatalf("get dispatcher: %v", err)
		}
		found := false
		for _, p := range disp.CompletedPhases {
			if p == "1" {
				found = true
			}
		}
		if !found {
			t.Errorf("completed_phases = %v, want to contain 1", disp.CompletedPhases)
		}
	})

	t.Run("guard: unresolved task blocks completion", func(t *testing.T) {
		d, srv, ctx, _ := setup(t)
		if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
			ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
		}); err != nil {
			t.Fatal(err)
		}
		// Insert a task in phase 1 with gate_state=none (unresolved).
		// Abandoned tasks also land here: abandoning a run does NOT advance the
		// task's gate_state, so an abandoned task stays at gate_state=none and
		// is correctly treated as unresolved by this guard.
		if err := d.store.InsertTask(ctx, &store.Task{
			ID: "tblocked", ProjectID: "p1", Source: "inbox", Title: "unresolved",
			Status: "running", GateState: sequence.GateNone,
			Metadata: map[string]any{"roadmap_phase": "1"},
		}); err != nil {
			t.Fatal(err)
		}
		_, rerr := call(t, srv, ctx, "comp", "1")
		if rerr == nil {
			t.Fatal("expected error for unresolved task, got nil")
		}
		if rerr.Code != rpc.ErrInvalidParams {
			t.Errorf("error code = %d, want ErrInvalidParams", rerr.Code)
		}
		// The hint must not mention "abandon" — abandoning a run does not resolve
		// the gate; the only valid resolution paths are finish or `hive sequence skip`.
		if strings.Contains(rerr.Message, "abandon") {
			t.Errorf("error message must not mention 'abandon', got: %q", rerr.Message)
		}
		// Phase must not have been marked complete.
		disp, err := d.store.GetSequenceDispatcher(ctx, "p1")
		if err != nil {
			t.Fatalf("get dispatcher: %v", err)
		}
		for _, p := range disp.CompletedPhases {
			if p == "1" {
				t.Errorf("phase 1 incorrectly recorded as complete despite guard failure")
			}
		}
	})

	t.Run("guard: satisfied and skipped tasks pass", func(t *testing.T) {
		d, srv, ctx, _ := setup(t)
		if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
			ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
		}); err != nil {
			t.Fatal(err)
		}
		// Two tasks: one satisfied, one skipped — both resolved, so guard passes.
		for _, pair := range []struct {
			id   string
			gate string
		}{
			{"tsat", sequence.GateSatisfied},
			{"tskip", sequence.GateSkipped},
		} {
			if err := d.store.InsertTask(ctx, &store.Task{
				ID: pair.id, ProjectID: "p1", Source: "inbox", Title: pair.id,
				Status: "done", GateState: pair.gate,
				Metadata: map[string]any{"roadmap_phase": "1"},
			}); err != nil {
				t.Fatal(err)
			}
		}
		res, rerr := call(t, srv, ctx, "comp", "1")
		if rerr != nil {
			t.Fatalf("expected success with satisfied+skipped tasks: %v", rerr)
		}
		if res["active_phase"] != "2" {
			t.Errorf("active_phase = %v, want 2", res["active_phase"])
		}
	})

	t.Run("idempotent: completing same phase twice succeeds", func(t *testing.T) {
		d, srv, ctx, _ := setup(t)
		if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
			ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
		}); err != nil {
			t.Fatal(err)
		}
		// First call.
		if _, rerr := call(t, srv, ctx, "comp", "1"); rerr != nil {
			t.Fatalf("first complete: %v", rerr)
		}
		// Second call — must not error.
		if _, rerr := call(t, srv, ctx, "comp", "1"); rerr != nil {
			t.Fatalf("second complete (idempotent): %v", rerr)
		}
	})

	t.Run("unknown project returns ErrProjectNotFound", func(t *testing.T) {
		_, srv, ctx, _ := setup(t)
		_, rerr := call(t, srv, ctx, "ghost", "1")
		if rerr == nil || rerr.Code != rpc.ErrProjectNotFound {
			t.Errorf("got %v, want ErrProjectNotFound", rerr)
		}
	})

	t.Run("unknown phase in roadmap returns ErrInvalidParams", func(t *testing.T) {
		d, srv, ctx, _ := setup(t)
		if err := d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
			ProjectID: "p1", Status: "active", AdvancementPolicy: "pr_opened",
		}); err != nil {
			t.Fatal(err)
		}
		_, rerr := call(t, srv, ctx, "comp", "99")
		if rerr == nil || rerr.Code != rpc.ErrInvalidParams {
			t.Errorf("got %v, want ErrInvalidParams for unknown phase", rerr)
		}
	})

	t.Run("missing params returns ErrInvalidParams", func(t *testing.T) {
		_, srv, ctx, _ := setup(t)
		raw, _ := json.Marshal(map[string]any{"project_slug": "comp"}) // missing phase
		_, rerr := srv.handleSequenceComplete(ctx, raw)
		if rerr == nil || rerr.Code != rpc.ErrInvalidParams {
			t.Errorf("got %v, want ErrInvalidParams for missing phase", rerr)
		}
	})
}
