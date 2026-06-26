package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/rohilrs/Hive/internal/branchhealth"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// HealthRemediateParams is the request envelope for health.remediate.
type HealthRemediateParams struct {
	ProjectSlug string `json:"project_slug"`
	Action      string `json:"action"` // "rebase" | "merge"
}

// handleHealthRemediate rebases the feature branch onto its target, or merges
// the target into the feature, for the given project — then returns the fresh
// health report. Guards (re-checked server-side, never trusting the caller):
// refuses a dirty tree or a predicted conflict; serializes per-repo; the git
// helpers auto-abort on any mid-operation failure. Local only — no push.
// After remediation the canonical checkout is intentionally left on the feature branch.
func (s *RPCServer) handleHealthRemediate(ctx context.Context, params json.RawMessage) (json.RawMessage, *rpc.RPCError) {
	var p HealthRemediateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: err.Error()}
	}
	if p.ProjectSlug == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project_slug required"}
	}
	if p.Action != "rebase" && p.Action != "merge" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "action must be rebase or merge"}
	}
	proj, err := s.d.store.GetProjectBySlug(ctx, p.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, &rpc.RPCError{Code: rpc.ErrProjectNotFound, Message: "project not found: " + p.ProjectSlug}
		}
		return nil, internalErr(err)
	}
	if proj.RepoPath == nil || *proj.RepoPath == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "project has no repo_path"}
	}
	repo := *proj.RepoPath
	feature := s.d.scheduler.effectiveFeatureBranchForProject(p.ProjectSlug)
	target := s.d.scheduler.effectiveTargetBranchForProject(p.ProjectSlug)
	if feature == "" {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "no feature branch configured for project " + p.ProjectSlug}
	}
	if feature == target {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "feature branch equals target branch (" + target + "); nothing to remediate"}
	}

	// Serialize the whole check-then-mutate against concurrent git work.
	unlock := lockRepoGit(repo)
	defer unlock()

	// Refresh origin refs first so the report reflects LIVE origin state
	// (local-feature-vs-origin-feature + behind-target), not a stale snapshot.
	fetchForHealth(repo, feature, target)

	roadmapRel := "docs/superpowers/roadmaps/" + p.ProjectSlug + ".md"
	pre, herr := branchhealth.CheckFeatureBranch(repo, feature, target, roadmapRel)
	if herr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "health check: " + herr.Error()}
	}
	// Dirty guard: the roadmap reconciler keeps docs/superpowers/roadmaps/<slug>.md
	// perpetually dirty (it regenerates it every loop). CheckFeatureBranch already
	// excludes roadmapRel from Dirty, so pre.Dirty here reflects only real
	// uncommitted user work.
	if pre.Dirty {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "working tree has uncommitted changes; commit or stash first"}
	}
	if len(pre.ConflictPaths) > 0 {
		return nil, &rpc.RPCError{Code: rpc.ErrInvalidParams, Message: "predicted conflicts on " + strings.Join(pre.ConflictPaths, ", ") + "; resolve manually"}
	}

	// Clear the reconciler roadmap dirtiness so the rebase/merge sees a clean tree.
	// Stash (don't discard) to be safe; after the action we DROP it — the
	// reconciler regenerates the roadmap and the action may have changed it, so
	// popping the stale stash would conflict. We already verified above that the
	// roadmap is the ONLY dirty path, so nothing else is at risk.
	roadmapStashed := false
	if isPathDirty(repo, roadmapRel) {
		if _, serr := gitC(repo, "stash", "push", "--", roadmapRel); serr == nil {
			roadmapStashed = true
		} else {
			log.Printf("health.remediate: stash roadmap %s failed: %v", roadmapRel, serr)
		}
	}

	switch p.Action {
	case "rebase":
		if err := rebaseFeatureBranch(repo, feature, target); err != nil {
			if roadmapStashed {
				_, _ = gitC(repo, "stash", "drop")
			}
			return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "rebase: " + err.Error()}
		}
	case "merge":
		if err := mergeTargetIntoFeature(repo, feature, target); err != nil {
			if roadmapStashed {
				_, _ = gitC(repo, "stash", "drop")
			}
			return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "merge: " + err.Error()}
		}
	}
	if roadmapStashed {
		_, _ = gitC(repo, "stash", "drop")
	}

	post, herr := branchhealth.CheckFeatureBranch(repo, feature, target, roadmapRel)
	if herr != nil {
		return nil, &rpc.RPCError{Code: rpc.ErrInternal, Message: "post-remediation health check: " + herr.Error()}
	}
	out, _ := json.Marshal(map[string]any{
		"action":         p.Action,
		"clean":          post.Clean,
		"report":         branchhealth.RenderHealthReport(post),
		"behind":         post.Behind,
		"ahead":          post.Ahead,
		"dirty":          post.Dirty,
		"origin_state":   post.OriginState,
		"conflict_paths": post.ConflictPaths,
	})
	return out, nil
}
