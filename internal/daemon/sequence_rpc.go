package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/roadmap"
	"github.com/rohilrs/Hive/internal/sequence"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

type sequenceParams struct {
	ProjectSlug  string `json:"project_slug"`
	TargetBranch string `json:"target_branch,omitempty"`
	Policy       string `json:"policy,omitempty"`
}

func (d *Daemon) loadProjectRoadmap(proj *store.Project) (*roadmap.Roadmap, string, error) {
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, "", fmt.Errorf("project %s has no repo_path", proj.Slug)
	}
	repo := *proj.RepoPath
	rmRel := "docs/superpowers/roadmaps/" + proj.Slug + ".md"
	// rmPath is the working-tree path; it doubles as the identifier writeback
	// callers (reconcile / sequence complete) use. The roadmap BODY, though, is
	// read working-tree-first then from the feature/target branch — see
	// readProjectDoc.
	rmPath := filepath.Join(repo, filepath.FromSlash(rmRel))
	body, ok := d.readProjectDoc(proj.Slug, repo, rmRel)
	if !ok {
		return nil, rmPath, fmt.Errorf("roadmap for %s not found in the working tree or on its feature/target branch — run `hive plan %s` (note: the roadmap lives on the feature branch; a shared repo's working tree may be checked out on another branch)", proj.Slug, proj.Slug)
	}
	rm, err := roadmap.Parse(body)
	if err != nil {
		return nil, rmPath, fmt.Errorf("roadmap parse error: %w", err)
	}
	return rm, rmPath, nil
}

// readProjectDoc reads a repo-relative doc, preferring the working tree (covers
// the same-branch + uncommitted-edit cases) and falling back to the project's
// feature/target branch via `git show` (covers a shared repo whose working tree
// is checked out on ANOTHER project's branch — the multi-project-one-repo setup,
// where the roadmap was committed to the feature branch and removed from main).
func (d *Daemon) readProjectDoc(slug, repoPath, repoRel string) ([]byte, bool) {
	if b, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(repoRel))); err == nil {
		return b, true
	}
	if ref := d.effectiveDocsRef(slug, repoPath); ref != "" {
		if b, err := gitShowBytes(repoPath, ref, repoRel); err == nil {
			return b, true
		}
	}
	return nil, false
}

// docExists reports whether a doc exists in the working tree (abs) or, for a
// repo-relative path, on the project's feature/target branch. An absolute spec
// link (repoRel == "") has no branch form and is checked against the working
// tree only.
func (d *Daemon) docExists(slug, repoPath, repoRel, abs string) bool {
	if _, err := os.Stat(abs); err == nil {
		return true
	}
	if repoRel == "" {
		return false
	}
	if ref := d.effectiveDocsRef(slug, repoPath); ref != "" {
		return gitFileExists(repoPath, ref, repoRel)
	}
	return false
}

// gitShowBytes returns the contents of repoRel at ref (stdout only, so file
// bytes aren't polluted by git stderr). A missing path → non-nil error.
func gitShowBytes(repoPath, ref, repoRel string) ([]byte, error) {
	return exec.Command("git", "-C", repoPath, "show", ref+":"+repoRel).Output()
}

// gitFileExists reports whether repoRel exists at ref.
func gitFileExists(repoPath, ref, repoRel string) bool {
	return exec.Command("git", "-C", repoPath, "cat-file", "-e", ref+":"+repoRel).Run() == nil
}

// specRepoRel maps a roadmap spec link to its repo-relative path for
// branch-aware existence checks. "" for an absolute link (no repo-relative
// form — checked against the working tree only).
func specRepoRel(roadmapRel, link string) string {
	if filepath.IsAbs(link) {
		return ""
	}
	if strings.HasPrefix(link, "./") {
		return filepath.ToSlash(filepath.Join(filepath.Dir(roadmapRel), link[2:]))
	}
	return filepath.ToSlash(link)
}

func (d *Daemon) derivePlan(ctx context.Context, proj *store.Project, rm *roadmap.Roadmap) (sequence.Plan, error) {
	tasks, err := d.store.ListTasksByProject(ctx, proj.ID)
	if err != nil {
		return sequence.Plan{}, err
	}
	order := make([]string, 0, len(rm.Phases))
	titles := make(map[string]string, len(rm.Phases))
	for _, ph := range rm.Phases {
		order = append(order, ph.Number)
		titles[ph.Number] = ph.Title
	}
	views := make([]sequence.TaskView, 0, len(tasks))
	for _, t := range tasks {
		phase, _ := t.Metadata["roadmap_phase"].(string)
		views = append(views, sequence.TaskView{
			ID: t.ID, Title: t.Title, Phase: phase, Status: t.Status, GateState: t.GateState,
		})
	}
	completed := map[string]bool{}
	if disp, derr := d.store.GetSequenceDispatcher(ctx, proj.ID); derr == nil {
		for _, p := range disp.CompletedPhases {
			completed[p] = true
		}
	} else if !errors.Is(derr, store.ErrNotFound) {
		return sequence.Plan{}, fmt.Errorf("load completed phases: %w", derr)
	}
	return sequence.Derive(order, titles, views, completed), nil
}

func (s *RPCServer) checkEnableGate(ctx context.Context, proj *store.Project) error {
	rm, rmPath, err := s.d.loadProjectRoadmap(proj)
	if err != nil {
		return err
	}
	if len(rm.Phases) == 0 {
		return fmt.Errorf("roadmap has no phases")
	}
	plan, err := s.d.derivePlan(ctx, proj, rm)
	if err != nil {
		return err
	}
	activeNum := plan.ActivePhase
	if activeNum == "" {
		return nil // all phases complete already
	}
	ph, ok := rm.FindPhase(activeNum)
	if !ok {
		// Defensive: ActivePhase came from Derive over this same roadmap, so
		// a miss here means a derivation/roadmap inconsistency, not completion.
		// Allow enable rather than hard-failing on an internal mismatch.
		return nil
	}
	if len(ph.SpecPaths) == 0 {
		return fmt.Errorf("phase %s has no linked spec in the roadmap", activeNum)
	}
	roadmapRel := "docs/superpowers/roadmaps/" + proj.Slug + ".md"
	for _, link := range ph.SpecPaths {
		abs := resolveSpecPath(*proj.RepoPath, rmPath, link)
		if !s.d.docExists(proj.Slug, *proj.RepoPath, specRepoRel(roadmapRel, link), abs) {
			return fmt.Errorf("phase %s spec missing: %s", activeNum, link)
		}
	}
	return nil
}

func (s *RPCServer) handleSequenceEnable(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	policy := p.Policy
	if policy == "" {
		policy = "pr_opened"
	}
	switch policy {
	case "pr_opened", "human_merge", "auto_merge_on_green", "manual":
		// ok
	default:
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "unknown advancement policy " + policy + " (want pr_opened|human_merge|auto_merge_on_green|manual)"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	if err := s.checkEnableGate(ctx, proj); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "cannot enable sequenced dispatch: " + err.Error()}
	}
	keys := map[string]any{"dispatch_mode": "sequenced"}
	if p.TargetBranch != "" {
		keys["target_branch"] = p.TargetBranch
	}
	if err := config.SetProjectScheduler(s.d.cfg.HiveDir, proj.Slug, keys); err != nil {
		return nil, internalErr(err)
	}
	if err := s.d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
		ProjectID: proj.ID, Status: "active", AdvancementPolicy: policy,
	}); err != nil {
		return nil, internalErr(err)
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventSequenceCreated,
		Data: map[string]any{"project_id": proj.ID, "slug": proj.Slug, "policy": policy},
	})
	out, _ := json.Marshal(map[string]any{"project_slug": proj.Slug, "policy": policy})
	return out, nil
}

// teardownSequencing performs the sequencing cleanup shared by handleSequenceDisable
// and applyDispatchMode's manual/auto_all transitions: it sweeps every task parked
// at GateAwaitingMerge to GateSatisfied (so they don't strand invisibly once the
// dispatcher row is gone and the merge-poller stops watching them) and removes the
// project's sequence dispatcher row. It does NOT touch [scheduler] dispatch_mode —
// callers own that write so the resulting mode (manual vs auto_all) is explicit.
//
// Safe to call on a project that was never sequenced: the sweep is a no-op when no
// tasks are at awaiting_merge, and DeleteSequenceDispatcher is idempotent for a
// missing row.
func (s *RPCServer) teardownSequencing(ctx context.Context, proj *store.Project) error {
	if tasks, lerr := s.d.store.ListTasksByProject(ctx, proj.ID); lerr == nil {
		for _, t := range tasks {
			if t.GateState != sequence.GateAwaitingMerge {
				continue
			}
			if uerr := s.d.store.UpdateTaskGateState(ctx, t.ID, sequence.GateSatisfied); uerr != nil {
				log.Printf("sequence teardown: sweep %s awaiting_merge->satisfied: %v", t.ID, uerr)
				continue
			}
			s.d.refreshTaskStatus(ctx, t.ID) // status follows the forced gate to done
			s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventSequenceGateChanged, Data: map[string]any{
				"project_id": proj.ID, "task_id": t.ID, "gate_state": sequence.GateSatisfied,
			}})
		}
	}
	if err := s.d.store.DeleteSequenceDispatcher(ctx, proj.ID); err != nil {
		return fmt.Errorf("dispatcher row cleanup failed: %w", err)
	}
	return nil
}

// applyDispatchMode transitions a project to the given dispatch mode, owning the
// full lifecycle (not just the config write):
//   - "sequenced": run checkEnableGate; on pass, write [scheduler]
//     dispatch_mode+target_branch and upsert an active sequence dispatcher with the
//     policy (default "pr_opened"). Gate failure returns the error with NO state change.
//   - "manual" / "auto_all": run teardownSequencing first (sweep GateAwaitingMerge ->
//     Satisfied + remove the dispatcher row, so no stale dispatcher lingers if the
//     project was sequenced), then write [scheduler] dispatch_mode.
//
// target/policy are used only for "sequenced".
func (s *RPCServer) applyDispatchMode(ctx context.Context, proj *store.Project, mode, target, policy string) error {
	switch mode {
	case "sequenced":
		if policy == "" {
			policy = "pr_opened"
		}
		// Validate the policy against the same allowlist handleSequenceEnable
		// enforces — applyDispatchMode is also reached from the edit handler with
		// user-supplied input, so a bad policy must not reach the dispatcher row.
		switch policy {
		case "pr_opened", "human_merge", "auto_merge_on_green", "manual":
		default:
			return fmt.Errorf("unknown advancement policy %q (want pr_opened|human_merge|auto_merge_on_green|manual)", policy)
		}
		if err := s.checkEnableGate(ctx, proj); err != nil {
			return fmt.Errorf("cannot enable sequenced dispatch: %w", err)
		}
		keys := map[string]any{"dispatch_mode": "sequenced"}
		if target != "" {
			keys["target_branch"] = target
		}
		if err := config.SetProjectScheduler(s.d.cfg.HiveDir, proj.Slug, keys); err != nil {
			return err
		}
		return s.d.store.UpsertSequenceDispatcher(ctx, &store.SequenceDispatcher{
			ProjectID: proj.ID, Status: "active", AdvancementPolicy: policy,
		})
	case "manual", "auto_all":
		if err := s.teardownSequencing(ctx, proj); err != nil {
			return err
		}
		return config.SetProjectScheduler(s.d.cfg.HiveDir, proj.Slug, map[string]any{"dispatch_mode": mode})
	default:
		return fmt.Errorf("unknown dispatch mode %q (want manual|auto_all|sequenced)", mode)
	}
}

func (s *RPCServer) handleSequenceDisable(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	// Disable = the operator is taking over manually. teardownSequencing sweeps any
	// awaiting_merge tasks to satisfied (so they don't strand invisibly once the
	// dispatcher row is gone) and removes the dispatcher row; then revert the
	// scheduler to manual. If the dispatcher-row removal fails, dispatch_mode has NOT
	// yet been touched (teardown runs before the config write), so the project stays
	// sequenced — surface a retry-able partial state rather than a bare internal error.
	if err := s.teardownSequencing(ctx, proj); err != nil {
		return nil, internalErr(fmt.Errorf("disable failed during sequencing teardown (retry `sequence disable`): %w", err))
	}
	if err := config.SetProjectScheduler(s.d.cfg.HiveDir, proj.Slug, map[string]any{"dispatch_mode": "manual"}); err != nil {
		return nil, internalErr(err)
	}
	s.d.bus.Publish(rpc.EventMessage{
		Type: rpc.EventSequenceUpdated,
		Data: map[string]any{"project_id": proj.ID, "slug": proj.Slug, "disabled": true},
	})
	out, _ := json.Marshal(map[string]any{"project_slug": proj.Slug, "disabled": true})
	return out, nil
}

func (s *RPCServer) handleSequenceStatus(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p sequenceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	rm, _, err := s.d.loadProjectRoadmap(proj)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	plan, err := s.d.derivePlan(ctx, proj, rm)
	if err != nil {
		return nil, internalErr(err)
	}
	status, policy := "", ""
	disp, derr := s.d.store.GetSequenceDispatcher(ctx, proj.ID)
	if derr != nil && !errors.Is(derr, store.ErrNotFound) {
		return nil, internalErr(derr)
	}
	if disp != nil {
		status, policy = disp.Status, disp.AdvancementPolicy
	}
	target := s.d.scheduler.effectiveTargetBranchForProject(proj.Slug)
	out, _ := json.Marshal(sequenceStatusView(proj.Slug, status, policy, target, plan))
	return out, nil
}

type seqTaskView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	GateState string `json:"gate_state"`
}
type seqPhaseView struct {
	Number   string        `json:"number"`
	Title    string        `json:"title"`
	Complete bool          `json:"complete"`
	Tasks    []seqTaskView `json:"tasks"`
	Blocked  []seqTaskView `json:"blocked,omitempty"`
}
type seqStatusView struct {
	Slug        string         `json:"slug"`
	Status      string         `json:"status,omitempty"`
	Policy      string         `json:"policy,omitempty"`
	Target      string         `json:"target,omitempty"`
	ActivePhase string         `json:"active_phase"`
	Complete    bool           `json:"complete"`
	Phases      []seqPhaseView `json:"phases"`
	Unsequenced []seqTaskView  `json:"unsequenced,omitempty"`
}

func toTaskViews(in []sequence.TaskView) []seqTaskView {
	out := make([]seqTaskView, 0, len(in))
	for _, t := range in {
		out = append(out, seqTaskView{ID: t.ID, Title: t.Title, Status: t.Status, GateState: t.GateState})
	}
	return out
}

func sequenceStatusView(slug, status, policy, target string, plan sequence.Plan) seqStatusView {
	v := seqStatusView{Slug: slug, Status: status, Policy: policy, Target: target, ActivePhase: plan.ActivePhase, Complete: plan.Complete}
	for _, ph := range plan.Phases {
		v.Phases = append(v.Phases, seqPhaseView{
			Number: ph.Number, Title: ph.Title, Complete: ph.Complete,
			Tasks: toTaskViews(ph.Tasks), Blocked: toTaskViews(ph.Blocked),
		})
	}
	v.Unsequenced = toTaskViews(plan.Unsequenced)
	return v
}
