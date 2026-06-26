package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// ResolveConfig holds the per-run knobs for the resolve pipeline.
type ResolveConfig struct {
	MaxIterations   int
	StageTimeout    time.Duration
	TestCommand     string
	ValidateCommand string
	ShellMaxBytes   int
	// PushFn commits the resolved merge and pushes the PR branch (no
	// force-push). Called once on the green path (or immediately on a
	// clean merge). Required.
	PushFn func(run *Run) error
}

// ResolvePipeline is the conflict-resolution FSM. It reproduces a merge
// conflict against the target branch, drives a bounded resolve subprocess
// per iteration, guards that the conflict markers are gone, then gates on
// the project's test + validate commands. On green it pushes; otherwise it
// feeds the failure back and retries until the iteration cap, then ends
// needs_attention. Lockfile/binary conflicts bail immediately — a model
// can't usefully merge them.
type ResolvePipeline struct {
	Adapter  adapter.Adapter
	Stages   StageStore     // nil = skip stage persistence (best-effort)
	Events   EventPublisher // nil = no event emission
	Feedback FeedbackStore
	Cfg      ResolveConfig
}

func (*ResolvePipeline) Name() string { return "resolve" }

func (p *ResolvePipeline) Run(ctx context.Context, run *Run) (*Result, error) {
	// Cheap guards against a misconfigured caller — both are dereferenced below
	// (run.Task.Body in the prompt; cfg.PushFn on the green/clean path).
	if run.Task == nil || p.Cfg.PushFn == nil {
		return &Result{Status: "needs_attention", Summary: "resolve misconfigured: nil task or PushFn"}, nil
	}
	cfg := p.Cfg
	// Per-run command overrides win over boot defaults, but only when non-empty.
	// Unlike EffTest/EffValidate (which unconditionally prefer Commands.Test even
	// when ""), resolve keeps the boot default on an empty per-run command — an
	// empty override here means "use whatever the project configures at boot."
	if run.Commands != nil {
		if run.Commands.Test != "" {
			cfg.TestCommand = run.Commands.Test
		}
		if run.Commands.Validate != "" {
			cfg.ValidateCommand = run.Commands.Validate
		}
	}
	maxIter := cfg.MaxIterations
	if maxIter < 1 {
		maxIter = 1
	}

	cc, err := BuildConflictContext(run.WorktreePath, run.TargetBranch)
	if err != nil {
		return &Result{Status: "needs_attention", Summary: "setup: " + err.Error()}, nil
	}
	if cc.Clean {
		// A textually-clean merge can still break the build (#271 was exactly
		// this). Gate it before pushing — never push a broken clean merge.
		if ok, out := runResolveGate(ctx, cfg.TestCommand, run.WorktreePath, cfg.StageTimeout, cfg.ShellMaxBytes); !ok {
			return &Result{Status: "needs_attention", Summary: "clean merge but test failed:\n" + out}, nil
		}
		if ok, out := runResolveGate(ctx, cfg.ValidateCommand, run.WorktreePath, cfg.StageTimeout, cfg.ShellMaxBytes); !ok {
			return &Result{Status: "needs_attention", Summary: "clean merge but validate failed:\n" + out}, nil
		}
		if perr := cfg.PushFn(run); perr != nil {
			return &Result{Status: "needs_attention", Summary: "push (clean merge): " + perr.Error()}, nil
		}
		return &Result{Status: "done", Summary: "resolved + pushed (awaiting merge confirmation)"}, nil
	}
	if bad := unresolvablePaths(cc); len(bad) > 0 {
		return &Result{Status: "needs_attention", Summary: "conflict in un-resolvable file(s): " + fmt.Sprint(bad)}, nil
	}

	var lastFail string
	for iter := 0; iter < maxIter; iter++ {
		fb, _ := p.Feedback.Get(ctx, run.ID, iter)

		stageID := int64(0)
		if p.Stages != nil {
			if id, berr := p.Stages.BeginStage(ctx, run.ID, "resolve", iter, ""); berr == nil {
				stageID = id
			}
		}
		p.emit(rpc.EventStageStarted, map[string]any{
			"run_id": run.ID, "stage_id": stageID, "name": "resolve", "iter": iter,
		})
		_, rerr := p.Adapter.RunStage(ctx, adapter.StageRequest{
			RunID:        run.ID,
			StageName:    "resolve",
			Iter:         iter,
			Cwd:          run.WorktreePath,
			AllowedTools: []string{"Read", "Edit", "MultiEdit"},
			SystemPrompt: resolvePrompt(cc, run.Task.Body, fb),
			UserPrompt:   "Resolve the conflicts in the listed files.",
			Timeout:      cfg.StageTimeout,
			// Per-stage + run scratch dirs: the adapter writes events.jsonl +
			// materializes the MCP/HOME scope here. Without StageDir the adapter
			// errors immediately (the fake adapter in unit tests ignores it).
			StageDir:         stageScratchDir(run, iter, "resolve"),
			RunDir:           run.RuntimeDir,
			OriginalRepoPath: derefRepoPath(run.Project),
			StageID:          stageID,
		})
		if p.Stages != nil && stageID != 0 {
			_ = p.Stages.EndStage(ctx, stageID, "", nil, 0, 0, 0, nil)
		}
		p.emit(rpc.EventStageEnded, map[string]any{
			"run_id": run.ID, "stage_id": stageID, "name": "resolve", "iter": iter,
		})
		if rerr != nil {
			// The resolve subprocess errored (e.g. claude failed to spawn or
			// exited non-zero before producing output). This was previously the
			// ONLY failure branch that neither logged nor recorded feedback, so a
			// subprocess that fails every iteration "exhausted" silently with zero
			// diagnosable trace (observed: #295's resolve died ~0.2s/iter, error
			// lost). Persist it as feedback AND log it so the cause is visible.
			lastFail = "resolve subprocess: " + rerr.Error()
			log.Printf("resolve: run %s iter %d subprocess error: %v", run.ID, iter, rerr)
			_ = p.Feedback.Put(ctx, run.ID, iter+1, Feedback{Summary: lastFail})
			continue
		}

		// Guard: every conflicted file must be marker-free now.
		if remaining := pathsWithMarkers(run.WorktreePath, cc); len(remaining) > 0 {
			lastFail = "unresolved conflict markers in: " + fmt.Sprint(remaining)
			_ = p.Feedback.Put(ctx, run.ID, iter+1, Feedback{Summary: lastFail})
			continue
		}

		// Edit-scope guard: validate runs against the whole worktree, but only
		// the conflicted paths get staged + pushed. An agent that edits an
		// out-of-scope file to make the gates pass produces a green validate
		// that doesn't match what's pushed (the stray edit is dropped). Reject.
		if stray := outOfScopeEdits(run.WorktreePath, cc); len(stray) > 0 {
			lastFail = "edited files outside the conflicted set (only edit the conflicted files): " + fmt.Sprint(stray)
			_ = p.Feedback.Put(ctx, run.ID, iter+1, Feedback{Summary: lastFail})
			continue
		}

		// Stage the resolved files so the merge commit (PushFn) captures them.
		paths := make([]string, 0, len(cc.Files))
		for _, f := range cc.Files {
			paths = append(paths, f.Path)
		}
		if _, gerr := gitIn(run.WorktreePath, append([]string{"add"}, paths...)...); gerr != nil {
			lastFail = "git add: " + gerr.Error()
			_ = p.Feedback.Put(ctx, run.ID, iter+1, Feedback{Summary: lastFail})
			continue
		}

		if ok, out := runResolveGate(ctx, cfg.TestCommand, run.WorktreePath, cfg.StageTimeout, cfg.ShellMaxBytes); !ok {
			lastFail = "test failed:\n" + out
			_ = p.Feedback.Put(ctx, run.ID, iter+1, Feedback{Summary: lastFail})
			continue
		}
		if ok, out := runResolveGate(ctx, cfg.ValidateCommand, run.WorktreePath, cfg.StageTimeout, cfg.ShellMaxBytes); !ok {
			lastFail = "validate failed:\n" + out
			_ = p.Feedback.Put(ctx, run.ID, iter+1, Feedback{Summary: lastFail})
			continue
		}

		if perr := cfg.PushFn(run); perr != nil {
			return &Result{Status: "needs_attention", Summary: "finalize push: " + perr.Error()}, nil
		}
		return &Result{Status: "done", Summary: "conflict resolved + pushed", Iterations: iter + 1}, nil
	}

	res := &Result{
		Status:        "needs_attention",
		Iterations:    maxIter,
		Summary:       "exhausted resolve iterations",
		ExhaustReason: lastFail,
	}
	if lastFail != "" {
		res.FinalFeedback = &Feedback{Summary: lastFail}
	}
	return res, nil
}

// emit publishes a typed event if Events is wired. Best-effort.
func (p *ResolvePipeline) emit(t rpc.EventType, data map[string]any) {
	if p.Events == nil {
		return
	}
	p.Events.Publish(rpc.EventMessage{Type: t, Data: data})
}

// pathsWithMarkers returns the conflicted files that still contain conflict
// markers in the working tree after a resolve attempt.
func pathsWithMarkers(worktree string, cc *ConflictContext) []string {
	var out []string
	for _, f := range cc.Files {
		if hasConflictMarkers(osReadFile(worktree, f.Path)) {
			out = append(out, f.Path)
		}
	}
	return out
}

// outOfScopeEdits returns worktree paths that are modified in the working tree
// but are NOT in the conflicted set. The resolve agent is told to touch only
// the conflicted files; validate runs against the whole worktree while only the
// conflicted paths are staged + pushed, so a stray edit makes the gate green on
// content that never ships. Flagging it forces a retry.
//
// Uses `git diff --name-only` (unstaged working-tree diff) rather than
// `git status --porcelain`. After a conflicted merge, git auto-merges
// non-conflicting files and STAGES them (worktree == index), so they do NOT
// appear in `git diff`. The agent's edits and the resolved conflicted files are
// UNSTAGED (worktree != index), so they DO appear in `git diff`. Using
// `git status --porcelain` (the previous approach) included staged auto-merged
// files and wrongly rejected every iteration on real merges.
//
// This guard runs before `git add <conflictPaths>`, so the conflicted files are
// still unstaged (listed by `git diff --name-only`) and excluded by the in-set
// check below.
func outOfScopeEdits(worktree string, cc *ConflictContext) []string {
	inSet := make(map[string]bool, len(cc.Files))
	for _, f := range cc.Files {
		inSet[f.Path] = true
	}
	// Working-tree diff (UNSTAGED) = the agent's edits + the resolved conflicted
	// files. The merge's auto-merged files are STAGED (worktree == index) and are
	// correctly NOT listed here — `git status --porcelain` would wrongly include
	// them and reject every iteration.
	out, err := gitIn(worktree, "diff", "--name-only")
	if err != nil {
		return nil
	}
	var stray []string
	seen := make(map[string]bool)
	for _, path := range strings.Split(strings.TrimSpace(out), "\n") {
		path = strings.Trim(path, "\"")
		if path == "" || inSet[path] || seen[path] {
			continue
		}
		seen[path] = true
		stray = append(stray, path)
	}
	return stray
}

// unresolvablePaths flags conflicted files a model can't usefully merge:
// dependency lockfiles (regenerated, not hand-merged) and binary files
// (a NUL byte in the merged content is the heuristic).
func unresolvablePaths(cc *ConflictContext) []string {
	lockfiles := map[string]bool{
		"pnpm-lock.yaml": true, "package-lock.json": true, "yarn.lock": true,
		"go.sum": true, "Cargo.lock": true, "poetry.lock": true,
	}
	var bad []string
	for _, f := range cc.Files {
		base := f.Path
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if lockfiles[base] || strings.ContainsRune(f.Merged, '\x00') {
			bad = append(bad, f.Path)
		}
	}
	return bad
}

// runResolveGate runs a test/validate gate command. An empty command is a
// pass (the stage is skipped). Returns (passed, output).
func runResolveGate(ctx context.Context, cmd, cwd string, timeout time.Duration, maxBytes int) (bool, string) {
	if strings.TrimSpace(cmd) == "" {
		return true, ""
	}
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	out, ok, _ := RunShellStage(ctx, cmd, cwd, timeout, maxBytes)
	return ok, out
}

// resolvePrompt renders the system prompt for the resolve subprocess: the
// conflicted files (with markers + each side's diff), the task intent, the
// edit-scope constraint, and any prior validation failure to address.
func resolvePrompt(cc *ConflictContext, taskBody string, fb Feedback) string {
	var b strings.Builder
	b.WriteString("You are resolving a git merge conflict. Merge the work on the TARGET branch (")
	b.WriteString(cc.TargetBranch)
	b.WriteString(") into this branch.\n\n")
	b.WriteString("## Task this branch implements\n")
	b.WriteString(taskBody + "\n\n")
	if fb.Summary != "" {
		b.WriteString("## A previous resolve attempt failed validation — fix this:\n")
		b.WriteString(fb.Summary + "\n\n")
	}
	// Conflicted files are listed by PATH only — the agent reads them with the
	// Read tool. We deliberately do NOT embed file contents/diffs here: this
	// prompt becomes claude's `--append-system-prompt` argv, and Linux caps a
	// single argv string at 128 KiB (MAX_ARG_STRLEN); a multi-file conflict
	// with full content + diffs blew past it → `fork/exec: argument list too
	// long`, killing every resolve iteration before claude even started. The
	// on-disk files carry the <<<<<<< / ======= / >>>>>>> markers (both sides'
	// content is between them), so Read is authoritative and never stale.
	b.WriteString("## Conflicted files\n")
	b.WriteString("Read EACH file below with the Read tool — each is in your working directory and ")
	b.WriteString("contains git conflict markers (`<<<<<<<` ours / `=======` / `>>>>>>>` theirs); the ")
	b.WriteString("text between the markers is both sides' versions.\n")
	for _, f := range cc.Files {
		b.WriteString("- " + f.Path + "\n")
	}
	b.WriteString("\n## Rules\n")
	b.WriteString("- Resolve EVERY conflict marker; leave no <<<<<<< / ======= / >>>>>>> behind.\n")
	b.WriteString("- Keep BOTH sides' intent unless they truly contradict; reconcile follow-on breakage ")
	b.WriteString("(e.g. a helper whose signature diverged) WITHIN these files.\n")
	b.WriteString("- Edit ONLY the conflicted files listed above; touch nothing else.\n")
	b.WriteString("- The change must pass the project's tests; the harness re-runs them.\n")
	return b.String()
}
