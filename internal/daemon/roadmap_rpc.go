package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohilrs/Hive/internal/branchhealth"
	"github.com/rohilrs/Hive/internal/codeintel"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/decompose"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// RoadmapDecomposeParams is the params envelope for roadmap.decompose
// (Phase 8.B). The CLI passes a project slug + phase identifier
// (matching `## Phase <number>: ...` in the roadmap doc) and an optional
// per-call max-subtasks cap. The handler reads the project's roadmap
// markdown, parses it, finds the named phase, loads each linked spec,
// and runs decompose.DecomposeForRoadmap. No insertion — the CLI runs a
// follow-up task.decompose_apply after the operator confirms.
type RoadmapDecomposeParams struct {
	ProjectSlug string `json:"project_slug"`
	Phase       string `json:"phase"`
	MaxSubtasks int    `json:"max_subtasks,omitempty"`
}

// RoadmapDecomposeResult mirrors decompose.Result plus the contextual
// fields the CLI needs to display the proposal back to the operator
// (phase number/title, the resolved roadmap path, the raw spec hrefs).
type RoadmapDecomposeResult struct {
	PhaseNumber string                      `json:"phase_number"`
	PhaseTitle  string                      `json:"phase_title"`
	RoadmapPath string                      `json:"roadmap_path"`
	SpecPaths   []string                    `json:"spec_paths"`
	Subtasks    []decompose.ProposedSubtask `json:"subtasks"`
	TokensIn    int                         `json:"tokens_in"`
	TokensOut   int                         `json:"tokens_out"`
	CostUSD     float64                     `json:"cost_usd"`
}

// runDecomposeWork performs the project/roadmap/spec resolution, grounding,
// and the Sonnet decompose turn for one phase, returning the proposal result.
// progress is called with a coarse phase label before the two slow stages so
// async callers can stream decompose.progress events; pass a no-op to ignore.
// All failures are plain errors (the async caller maps them to a
// decompose.failed event).
func (s *RPCServer) runDecomposeWork(ctx context.Context, p RoadmapDecomposeParams, progress func(string)) (*RoadmapDecomposeResult, error) {
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", p.ProjectSlug, err)
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, fmt.Errorf("project %q has no repo_path", p.ProjectSlug)
	}
	repoRoot := *proj.RepoPath

	// Read the roadmap + specs branch-aware (working tree first, then the
	// feature/target branch) via loadProjectRoadmap — a shared repo's working
	// tree may be checked out on another project's branch, so the roadmap can
	// live only on this project's feature branch.
	rm, roadmapPath, err := s.d.loadProjectRoadmap(proj)
	if err != nil {
		return nil, err
	}
	phase, ok := rm.FindPhase(p.Phase)
	if !ok {
		return nil, fmt.Errorf("phase %q not found in %s", p.Phase, roadmapPath)
	}

	roadmapRel := "docs/superpowers/roadmaps/" + p.ProjectSlug + ".md"
	var specs []decompose.SpecContent
	for _, link := range phase.SpecPaths {
		rel := specRepoRel(roadmapRel, link)
		if rel == "" {
			// Absolute link: working-tree only (no branch form).
			if body, rerr := os.ReadFile(link); rerr == nil {
				specs = append(specs, decompose.SpecContent{Path: link, Body: string(body)})
			}
			continue
		}
		if body, ok := s.d.readProjectDoc(p.ProjectSlug, repoRoot, rel); ok {
			specs = append(specs, decompose.SpecContent{Path: link, Body: string(body)})
		}
	}

	existingItems, _ := s.d.gatherExistingWork(ctx, proj, p.Phase)
	existing := make([]decompose.ExistingRef, 0, len(existingItems))
	for _, it := range existingItems {
		existing = append(existing, decompose.ExistingRef{
			Ref:   it.Ref,
			Block: formatExistingWorkBlock([]ExistingItem{it}),
		})
	}

	progress("preparing codebase context")
	groundSrc := phase.Body
	for _, sp := range specs {
		groundSrc += "\n" + sp.Body
	}
	codebaseContext := codeintel.BuildContext(ctx, s.d.plannerGrounderFor(proj.Slug, repoRoot), groundSrc)

	progress("running model")
	res, err := decompose.DecomposeForRoadmap(ctx, s.d.decomposeRunner, *proj, phase, specs, p.MaxSubtasks, s.d.decomposeStackHint(proj.Slug), existing, codebaseContext)
	if err != nil {
		return nil, err
	}

	return &RoadmapDecomposeResult{
		PhaseNumber: phase.Number,
		PhaseTitle:  phase.Title,
		RoadmapPath: roadmapPath,
		SpecPaths:   phase.SpecPaths,
		Subtasks:    res.Subtasks,
		TokensIn:    res.InputTokens,
		TokensOut:   res.OutputTokens,
		CostUSD:     res.CostUSD,
	}, nil
}

// handleRoadmapContent returns the project's roadmap markdown, read branch-aware
// (working tree first, then the feature/target branch) so the TUI roadmap viewer
// resolves it even when the shared repo's working tree is checked out on another
// project's branch. The viewer reads the working tree itself as a fast path and
// only calls this on a miss.
func (s *RPCServer) handleRoadmapContent(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		ProjectSlug string `json:"project_slug"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project has no repo_path"}
	}
	rmRel := "docs/superpowers/roadmaps/" + p.ProjectSlug + ".md"
	body, ok := s.d.readProjectDoc(p.ProjectSlug, *proj.RepoPath, rmRel)
	if !ok {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "roadmap for " + p.ProjectSlug + " not found in the working tree or on its feature/target branch — run `hive plan " + p.ProjectSlug + "`"}
	}
	out, _ := json.Marshal(map[string]any{"content": string(body)})
	return out, nil
}

// handleRoadmapDecompose STARTS an async decompose and returns a decompose_id
// immediately. The proposal (or error) is delivered via the event bus
// (decompose.proposed / decompose.failed); progress streams as
// decompose.progress. This keeps the RPC off any read deadline — the grounding
// index + Sonnet turn can take minutes without an i/o timeout. The apply path
// (roadmap.decompose_apply) is unchanged.
func (s *RPCServer) handleRoadmapDecompose(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p RoadmapDecomposeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	if p.Phase == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "phase required"}
	}
	if s.d.decomposeRunner == nil {
		return nil, &rpc.RPCError{
			Code:    rpc.ErrInternal,
			Message: "decompose runner not configured (composition root must wire SetDecomposeRunner; see cmd/hive/cmd_daemon.go)",
		}
	}

	// startOrExisting returns the freshly-minted id on a NEW registration, or a
	// DIFFERENT existing id on a dedup hit. Spawn a worker only on a fresh
	// registration; a dedup hit means a worker is already running and its
	// broadcast events reach this client too.
	fresh := newID("decompose")
	id := s.d.decomposeJobs.startOrExisting(fresh, p.ProjectSlug, p.Phase)
	if id == fresh {
		go s.runDecomposeAsync(id, p)
	}

	raw, _ := json.Marshal(map[string]string{"decompose_id": id})
	return raw, nil
}

// runDecomposeAsync runs the decompose work under the DAEMON context (so a
// client disconnect doesn't cancel it) and publishes lifecycle events.
func (s *RPCServer) runDecomposeAsync(id string, p RoadmapDecomposeParams) {
	defer s.d.decomposeJobs.finish(id)
	defer func() {
		if r := recover(); r != nil {
			s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventDecomposeFailed, Data: map[string]any{
				"decompose_id": id, "project_slug": p.ProjectSlug, "phase": p.Phase,
				"error": fmt.Sprintf("decompose panicked: %v", r),
			}})
		}
	}()

	progress := func(label string) {
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventDecomposeProgress, Data: map[string]any{
			"decompose_id": id, "project_slug": p.ProjectSlug, "phase": p.Phase, "phase_label": label,
		}})
	}

	res, err := s.runDecomposeWork(s.d.ctx, p, progress)
	if err != nil {
		s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventDecomposeFailed, Data: map[string]any{
			"decompose_id": id, "project_slug": p.ProjectSlug, "phase": p.Phase, "error": err.Error(),
		}})
		return
	}
	s.d.bus.Publish(rpc.EventMessage{Type: rpc.EventDecomposeProposed, Data: map[string]any{
		"decompose_id": id, "project_slug": p.ProjectSlug, "phase": p.Phase, "result": res,
	}})
}

// RoadmapSyncLinearResult is the result of roadmap.sync_linear: counts the
// operator sees after the mirror reconcile (recomputed from the persisted map).
type RoadmapSyncLinearResult struct {
	Document   int      `json:"document"`   // 1 if a doc id is recorded, else 0
	Milestones int      `json:"milestones"` // count of milestones in the mirror map after sync
	Errors     []string `json:"errors,omitempty"`
}

// handleRoadmapSyncLinear exposes syncRoadmapToLinear as an RPC for the manual
// `hive roadmap sync-linear` CLI and recovery. Best-effort: a partial-sync
// error is returned in the result's Errors field, not as an RPC error (the
// sync still persisted whatever it could).
func (s *RPCServer) handleRoadmapSyncLinear(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		ProjectSlug string `json:"project_slug"`
	}
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
	syncErr := s.d.syncRoadmapToLinear(ctx, proj)
	// Recompute counts from the persisted map (syncRoadmapToLinear persisted it).
	reloaded, gerr := s.d.store.GetProject(ctx, proj.ID)
	if gerr != nil {
		log.Printf("roadmap.sync_linear: reload after sync failed: %v", gerr)
		reloaded = proj
	}
	m := loadMirrorState(reloaded)
	res := RoadmapSyncLinearResult{Milestones: len(m.Milestones)}
	if m.DocumentID != "" {
		res.Document = 1
	}
	if syncErr != nil {
		res.Errors = append(res.Errors, syncErr.Error())
	}
	raw, _ := json.Marshal(res)
	return raw, nil
}

// resolveSpecPath maps a markdown-link href found in a roadmap's phase
// body to an absolute filesystem path. Three cases:
//   - absolute path → as-is
//   - "./..." prefix → resolved against the roadmap file's directory
//     (so "./specs/foo.md" sits next to the roadmap on disk)
//   - bare path ("docs/superpowers/specs/foo.md") → resolved against
//     the project's repo root (the planner system prompt produces these
//     repo-relative links by default)
func resolveSpecPath(repoRoot, roadmapPath, link string) string {
	if filepath.IsAbs(link) {
		return link
	}
	if strings.HasPrefix(link, "./") {
		return filepath.Join(filepath.Dir(roadmapPath), link[2:])
	}
	return filepath.Join(repoRoot, link)
}

// RoadmapPlanSetupParams is the params envelope for roadmap.plan_setup. The
// daemon owns the repo path + git, so the `hive plan` flow delegates the
// branch ensure/checkout/health-check/persist work here.
type RoadmapPlanSetupParams struct {
	ProjectSlug   string `json:"project_slug"`
	FeatureBranch string `json:"feature_branch"`
}

// handleRoadmapPlanSetup ensures/adopts the feature branch, checks it out in the
// project repo, runs a health-check against the target branch, persists the
// feature branch to the per-project config overlay, and returns the report.
func (s *RPCServer) handleRoadmapPlanSetup(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p RoadmapPlanSetupParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found: " + p.ProjectSlug}
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project has no repo_path"}
	}
	repo := *proj.RepoPath
	// Resolve the feature branch: explicit param wins, else existing config.
	feature := p.FeatureBranch
	if feature == "" {
		feature = s.d.scheduler.effectiveFeatureBranchForProject(p.ProjectSlug)
	}
	if feature == "" {
		// Neither requested nor configured → integration is inert for this
		// project; do nothing and let the planner run on the current branch.
		out, _ := json.Marshal(map[string]any{"active": false})
		return out, nil
	}
	target := s.d.scheduler.effectiveTargetBranchForProject(p.ProjectSlug)

	// Serialize canonical-repo git mutations against concurrent remediation
	// (health.remediate also takes this lock on the same repo path).
	unlock := lockRepoGit(repo)
	defer unlock()

	// Refuse to switch branches over a dirty tree — never clobber uncommitted work.
	if st, _ := gitC(repo, "status", "--porcelain"); st != "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "working tree at " + repo + " has uncommitted changes; commit or stash before planning on a feature branch"}
	}
	created, eerr := ensureFeatureBranch(repo, feature, target)
	if eerr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "ensure feature branch: " + eerr.Error()}
	}
	if out, cerr := gitC(repo, "checkout", feature); cerr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "checkout " + feature + ": " + cerr.Error() + " (" + out + ")"}
	}
	roadmapRel := "docs/superpowers/roadmaps/" + p.ProjectSlug + ".md"
	report, herr := branchhealth.CheckFeatureBranch(repo, feature, target, roadmapRel)
	if herr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "health check: " + herr.Error()}
	}
	if perr := config.SetProjectIntegration(s.d.cfg.HiveDir, p.ProjectSlug, map[string]any{"feature_branch": feature}); perr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "persist feature_branch: " + perr.Error()}
	}
	out, _ := json.Marshal(map[string]any{
		"active":         true,
		"feature_branch": feature,
		"target_branch":  target,
		"created":        created,
		"clean":          report.Clean,
		"report":         branchhealth.RenderHealthReport(report),
		"behind":         report.Behind,
		"ahead":          report.Ahead,
		"dirty":          report.Dirty,
		"origin_state":   report.OriginState,
		"conflict_paths": report.ConflictPaths,
	})
	return out, nil
}

// handleRoadmapPlanPush pushes the project's configured feature branch to origin.
func (s *RPCServer) handleRoadmapPlanPush(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p struct {
		ProjectSlug string `json:"project_slug"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project not found"}
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project has no repo_path"}
	}
	feature := s.d.scheduler.effectiveFeatureBranchForProject(p.ProjectSlug)
	if feature == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project has no feature_branch configured"}
	}
	if perr := pushFeatureBranch(*proj.RepoPath, feature); perr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "push: " + perr.Error()}
	}
	out, _ := json.Marshal(map[string]any{"pushed": true, "feature_branch": feature})
	return out, nil
}
