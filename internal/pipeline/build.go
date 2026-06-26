package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/store/pricing"
	"github.com/rohilrs/Hive/internal/verdict"
	"github.com/rohilrs/Hive/pkg/rpc"
)

type BuildConfig struct {
	MaxIterations int
	Ladder        ModelLadder
	StageTimeout  time.Duration
	SkillsImpl    []string
	SkillsReview  []string

	// LoopCheckAfterIter gates L3 loop detection. The detector runs
	// after each review iter where iter >= this value. Default 1 means
	// iter 0 never checks (no prior to compare); set to a large value
	// to disable.
	LoopCheckAfterIter int

	// LoopSimilarityThreshold is the cutoff above which two iters are
	// considered a loop. Range [0,1]; default 0.85.
	LoopSimilarityThreshold float64

	// LoopDiffBase is the git ref to diff against for per-iter snapshot.
	// Empty defaults to "main".
	LoopDiffBase string

	// TestCommand is the shell command run after review APPROVE.
	// Empty string skips the test stage entirely.
	TestCommand string

	// ValidateCommand is the shell command run after TestCommand
	// passes (or is skipped). Empty string skips the validate stage.
	ValidateCommand string

	// TestStageTimeout bounds TestCommand execution. Zero disables
	// the timeout (use context cancellation).
	TestStageTimeout time.Duration

	// ValidateStageTimeout bounds ValidateCommand execution.
	ValidateStageTimeout time.Duration

	// ShellOutputMaxBytes caps the captured output written to feedback.
	// Default 8192 when zero.
	ShellOutputMaxBytes int

	// Phase 4.4: documenter (terminal, non-blocking Build stage).
	DocumenterEnabled      bool
	DocumenterModel        string
	DocumenterTimeout      time.Duration
	DocumenterSkills       []string
	DocumenterUpdateReadme bool
	DocumenterCodeComments bool
}

type BuildPipeline struct {
	Adapter  adapter.Adapter
	Feedback FeedbackStore
	Stages   StageStore     // Phase 3.1: nil = skip stage/tool persistence (best-effort)
	Stalls   StallRecorder  // Phase 3.3: nil = skip L3 row writes (best-effort)
	Loop     LoopDetector   // Phase 3.3: nil = disable L3 entirely
	Events   EventPublisher // Phase 3.5a: nil = no event emission
	Cfg      BuildConfig
}

func (*BuildPipeline) Name() string { return "build" }

func (p *BuildPipeline) Run(ctx context.Context, run *Run) (*Result, error) {
	res := &Result{}

	// Phase 3.3: separate ladder index from iter counter. iter drives
	// the loop counter and stages.iter accounting; ladderIdx drives
	// WorkerLadder / ReviewerLadder model selection. Baseline:
	// ladderIdx advances with iter (preserving pre-3.3 behavior where
	// iter N used Ladder[N]). L3 escalation can JUMP ladderIdx ahead
	// of iter, in which case iter catches up later.
	ladderIdx := 0

	// Phase 3.3: hold previous iter's diff + FileRefs in scope so the
	// loop detector can compare. Updated at end of each iter.
	var prevIter *Iteration

	// Why the loop exhausted, for an accurate terminal summary. Updated
	// whenever an iteration ends without reaching "done": review not
	// approving (default) vs. review approving but a post-approve gate
	// (test/validate) failing. The latter previously masqueraded as
	// "without APPROVE", which is misleading when the review DID approve.
	exhaustReason := "review never approved within the iteration cap"

	// lastFB holds the most recent per-iteration feedback produced
	// by the reviewer (CR refs/summary) or by a shell failure (synthetic
	// ref). On any give-up path (needs_attention) we copy it into
	// res.FinalFeedback so the daemon can persist it to the task for
	// injection into the next run's iter-0 implement prompt.
	var lastFB Feedback
	// lastFBSet is true once we have captured at least one real feedback
	// value. Without this flag, an empty zero-value Feedback would be
	// misleadingly copied into FinalFeedback.
	lastFBSet := false

	for iter := 0; iter < p.Cfg.MaxIterations; iter++ {
		if iter > ladderIdx {
			ladderIdx = iter
		}
		workerModel := p.Cfg.Ladder.WorkerAt(ladderIdx)
		reviewerModel := p.Cfg.Ladder.ReviewerAt(ladderIdx)

		var priorFeedback Feedback
		if iter > 0 && p.Feedback != nil {
			fb, err := p.Feedback.Get(ctx, run.ID, iter-1)
			if err != nil && !errors.Is(err, ErrFeedbackNotFound) {
				return nil, fmt.Errorf("iter %d read feedback: %w", iter, err)
			}
			priorFeedback = fb
		}

		implStageID := int64(0)
		if p.Stages != nil {
			id, berr := p.Stages.BeginStage(ctx, run.ID, "implement", iter, workerModel)
			if berr != nil {
				log.Printf("pipeline: BeginStage implement iter %d: %v", iter, berr)
			} else {
				implStageID = id
			}
		}
		p.emit(rpc.EventStageStarted, map[string]any{
			"run_id": run.ID, "stage_id": implStageID,
			"name": "implement", "iter": iter, "model": workerModel,
		})
		implOut, err := p.Adapter.RunStage(ctx, adapter.StageRequest{
			RunID:            run.ID,
			StageName:        "implement",
			Iter:             iter,
			Model:            workerModel,
			Skills:           p.Cfg.SkillsImpl,
			SystemPrompt:     p.implementPrompt(run, iter, priorFeedback, run.Prediction),
			UserPrompt:       taskImplementPrompt(run.Task),
			Timeout:          p.Cfg.StageTimeout,
			Cwd:              run.WorktreePath,
			StageDir:         stageScratchDir(run, iter, "implement"),
			RunDir:           run.RuntimeDir,
			OriginalRepoPath: derefRepoPath(run.Project),
			StageID:          implStageID,
		})
		if err != nil {
			return nil, fmt.Errorf("iter %d implement: %w", iter, err)
		}
		if p.Stages != nil && implStageID != 0 {
			cost := computeCost(workerModel, implOut.Tokens)
			if eerr := p.Stages.EndStage(ctx, implStageID, "", nil, implOut.Tokens.Input, implOut.Tokens.Output, implOut.Tokens.CacheHit, cost); eerr != nil {
				log.Printf("pipeline: EndStage implement iter %d: %v", iter, eerr)
			}
			if perr := p.Stages.PutToolCalls(ctx, run.ID, implStageID, toPipelineToolCalls(implOut.ToolCalls)); perr != nil {
				log.Printf("pipeline: PutToolCalls implement iter %d: %v", iter, perr)
			}
		}
		p.emit(rpc.EventStageEnded, map[string]any{
			"run_id": run.ID, "stage_id": implStageID,
			"name": "implement", "iter": iter,
			"tokens_in": implOut.Tokens.Input, "tokens_out": implOut.Tokens.Output,
		})

		// Phase 3.3: capture diff for loop detector. Best-effort —
		// failure (empty worktree path, not a git dir) leaves diff
		// empty and the detector still gets the FileRefs signal.
		var iterDiff string
		if p.Loop != nil && run.WorktreePath != "" {
			base := p.Cfg.LoopDiffBase
			if base == "" {
				base = "main"
			}
			d, derr := captureWorktreeDiff(ctx, run.WorktreePath, base)
			if derr != nil {
				log.Printf("pipeline: captureWorktreeDiff iter %d: %v", iter, derr)
			}
			iterDiff = d
		}

		reviewStageID := int64(0)
		if p.Stages != nil {
			id, berr := p.Stages.BeginStage(ctx, run.ID, "review", iter, reviewerModel)
			if berr != nil {
				log.Printf("pipeline: BeginStage review iter %d: %v", iter, berr)
			} else {
				reviewStageID = id
			}
		}
		p.emit(rpc.EventStageStarted, map[string]any{
			"run_id": run.ID, "stage_id": reviewStageID,
			"name": "review", "iter": iter, "model": reviewerModel,
		})
		revOut, err := p.Adapter.RunStage(ctx, adapter.StageRequest{
			RunID:            run.ID,
			StageName:        "review",
			Iter:             iter,
			Model:            reviewerModel,
			Skills:           p.Cfg.SkillsReview,
			SystemPrompt:     p.reviewPrompt(run, iter),
			UserPrompt:       "Review the changes in this worktree.",
			Timeout:          p.Cfg.StageTimeout,
			Cwd:              run.WorktreePath,
			StageDir:         stageScratchDir(run, iter, "review"),
			RunDir:           run.RuntimeDir,
			OriginalRepoPath: derefRepoPath(run.Project),
			VerdictToolName:  "hive_submit_review_verdict",
			StageID:          reviewStageID,
		})
		if err != nil {
			return nil, fmt.Errorf("iter %d review: %w", iter, err)
		}
		if p.Stages != nil && reviewStageID != 0 {
			var verdictKind string
			var verdictConf *float64
			if revOut.Verdict != nil {
				verdictKind = string(revOut.Verdict.Kind)
				if revOut.Verdict.Confidence > 0 {
					c := float64(revOut.Verdict.Confidence) / 100.0
					verdictConf = &c
				}
			}
			cost := computeCost(reviewerModel, revOut.Tokens)
			if eerr := p.Stages.EndStage(ctx, reviewStageID, verdictKind, verdictConf, revOut.Tokens.Input, revOut.Tokens.Output, revOut.Tokens.CacheHit, cost); eerr != nil {
				log.Printf("pipeline: EndStage review iter %d: %v", iter, eerr)
			}
			if perr := p.Stages.PutToolCalls(ctx, run.ID, reviewStageID, toPipelineToolCalls(revOut.ToolCalls)); perr != nil {
				log.Printf("pipeline: PutToolCalls review iter %d: %v", iter, perr)
			}
		}
		reviewVerdict := ""
		if revOut.Verdict != nil {
			reviewVerdict = string(revOut.Verdict.Kind)
		}
		p.emit(rpc.EventStageEnded, map[string]any{
			"run_id": run.ID, "stage_id": reviewStageID,
			"name": "review", "iter": iter, "verdict": reviewVerdict,
		})

		if p.Feedback != nil && revOut.Verdict != nil && revOut.Verdict.FromTool && len(revOut.Verdict.FileRefs) > 0 {
			fb := Feedback{
				Summary:  revOut.Verdict.Summary,
				FileRefs: revOut.Verdict.FileRefs,
			}
			if err := p.Feedback.Put(ctx, run.ID, iter, fb); err != nil {
				return nil, fmt.Errorf("iter %d persist feedback: %w", iter, err)
			}
			lastFB = fb
			lastFBSet = true
		}

		res.Iterations = iter + 1
		if revOut.Verdict != nil && revOut.Verdict.Kind == adapter.VerdictApprove {
			// Phase 3.4: gate "done" behind test + validate stages.
			// Either failing routes back through implement next iter
			// with the failure output as feedback. Empty commands
			// skip the corresponding stage entirely (no rows, no
			// feedback). Successful path summary mentions which
			// post-review stages ran.
			testCmd, testTO := run.EffTest(p.Cfg.TestCommand, p.Cfg.TestStageTimeout)
			testOK, terr := p.runShellPipelineStage(ctx, run, iter, "test", testCmd, testTO)
			if terr != nil {
				return nil, terr
			}
			if !testOK {
				// Feedback persisted; loop to next implement iter.
				// Clear prevIter so L3 doesn't compare a test-failure
				// iter to a review-CR iter (different FileRef shapes).
				exhaustReason = fmt.Sprintf("review approved but the test command failed (iter %d) — check `[pipelines.build] test_command`", iter)
				if p.Feedback != nil {
					if shellFB, ferr := p.Feedback.Get(ctx, run.ID, iter); ferr == nil {
						lastFB = shellFB
						lastFBSet = true
					}
				}
				prevIter = nil
				continue
			}
			validateCmd, validateTO := run.EffValidate(p.Cfg.ValidateCommand, p.Cfg.ValidateStageTimeout)
			validateOK, verr := p.runShellPipelineStage(ctx, run, iter, "validate", validateCmd, validateTO)
			if verr != nil {
				return nil, verr
			}
			if !validateOK {
				exhaustReason = fmt.Sprintf("review approved but the validate command failed (iter %d) — check `[pipelines.build] validate_command`", iter)
				if p.Feedback != nil {
					if shellFB, ferr := p.Feedback.Get(ctx, run.ID, iter); ferr == nil {
						lastFB = shellFB
						lastFBSet = true
					}
				}
				prevIter = nil
				continue
			}
			skipped, reason := p.runDocumentStage(ctx, run, iter)
			res.DocumentationSkipped = skipped
			res.DocumentationSkipReason = reason
			// Commit the agent's final worktree state so finish-branch has a real
			// diff to push/PR. The worker can't `git commit` (adapter sandbox), so
			// Hive owns it. Best-effort — a commit failure is logged, not fatal.
			if cerr := commitWorktreeChanges(ctx, run); cerr != nil {
				log.Printf("pipeline: commit worktree for run %s: %v", run.ID, cerr)
			}
			res.Status = "done"
			res.Summary = approvalSummary(iter, testCmd, validateCmd)
			res.EndedAt = time.Now()
			return res, nil
		}

		// Phase 3.3: loop detection — only when (a) detector is wired,
		// (b) we have a prior iter to compare to, (c) iter is at/past
		// the gate, (d) this iter is CHANGES_REQUESTED (APPROVE already
		// returned above).
		currIter := Iteration{Diff: iterDiff}
		if revOut.Verdict != nil {
			currIter.FileRefs = revOut.Verdict.FileRefs
		}
		if p.Loop != nil && prevIter != nil && iter >= p.Cfg.LoopCheckAfterIter {
			sim, lerr := p.Loop.ClassifyLoopSimilarity(ctx, *prevIter, currIter)
			if lerr != nil {
				log.Printf("pipeline: loop detector iter %d: %v (proceeding without)", iter, lerr)
			} else if sim >= p.Cfg.LoopSimilarityThreshold {
				prevModel := workerModel
				newIdx, escalated := escalateLadderIdx(ladderIdx, len(p.Cfg.Ladder.Worker))
				if !escalated {
					// Already at top of ladder before this check — nowhere
					// to escalate. Mark the run needs_attention and end.
					details := fmt.Sprintf(`{"sim_score":%.3f,"prev_iter":%d,"this_iter":%d,"prev_model":%q,"escalated_to":""}`,
						sim, iter-1, iter, prevModel)
					p.recordL3(ctx, run.ID, reviewStageID, "marked_needs_attention", details)
					res.Status = "needs_attention"
					loopSummary := fmt.Sprintf("loop_detected: similarity=%.2f", sim)
					res.Summary = loopSummary
					res.ExhaustReason = loopSummary
					if lastFBSet {
						fb := lastFB
						res.FinalFeedback = &fb
					}
					res.EndedAt = time.Now()
					return res, nil
				}
				escalatedTo := p.Cfg.Ladder.WorkerAt(newIdx)
				details := fmt.Sprintf(`{"sim_score":%.3f,"prev_iter":%d,"this_iter":%d,"prev_model":%q,"escalated_to":%q}`,
					sim, iter-1, iter, prevModel, escalatedTo)
				p.recordL3(ctx, run.ID, reviewStageID, "escalated_model", details)
				ladderIdx = newIdx
			}
		}
		prevIter = &currIter
	}

	res.Status = "needs_attention"
	res.Summary = "exhausted iteration cap: " + exhaustReason
	res.ExhaustReason = exhaustReason
	if lastFBSet {
		fb := lastFB
		res.FinalFeedback = &fb
	}
	res.EndedAt = time.Now()
	return res, nil
}

// runShellPipelineStage runs one shell-driven stage (test or validate)
// with full observability:
//   - BeginStage/EndStage rows (no model, no tokens, no cost)
//   - Verdict APPROVE on exit 0, CHANGES_REQUESTED on non-zero
//   - On failure, a synthetic FileRef written to FeedbackStore so the
//     next iter's implement prompt includes the failure output
//
// stageName is "test" or "validate". Empty command skips entirely
// (returns success=true without writing any rows or feedback).
// Returns (success, err); err only for unrecoverable setup errors.
func (p *BuildPipeline) runShellPipelineStage(ctx context.Context, run *Run, iter int, stageName, command string, timeout time.Duration) (bool, error) {
	if command == "" {
		return true, nil
	}
	command = renderStageCommand(ctx, command, run.TargetBranch, run.WorktreePath)
	maxBytes := p.Cfg.ShellOutputMaxBytes
	if maxBytes <= 0 {
		maxBytes = 8192
	}

	stageID := int64(0)
	if p.Stages != nil {
		id, berr := p.Stages.BeginStage(ctx, run.ID, stageName, iter, "")
		if berr != nil {
			log.Printf("pipeline: BeginStage %s iter %d: %v", stageName, iter, berr)
		} else {
			stageID = id
		}
	}
	p.emit(rpc.EventStageStarted, map[string]any{
		"run_id": run.ID, "stage_id": stageID,
		"name": stageName, "iter": iter,
	})

	output, ok, runErr := RunShellStage(ctx, command, run.WorktreePath, timeout, maxBytes)
	if runErr != nil {
		if p.Stages != nil && stageID != 0 {
			_ = p.Stages.EndStage(ctx, stageID, "", nil, 0, 0, 0, nil)
		}
		return false, fmt.Errorf("iter %d %s: %w", iter, stageName, runErr)
	}

	verdictKind := "APPROVE"
	if !ok {
		verdictKind = "CHANGES_REQUESTED"
	}
	if p.Stages != nil && stageID != 0 {
		if eerr := p.Stages.EndStage(ctx, stageID, verdictKind, nil, 0, 0, 0, nil); eerr != nil {
			log.Printf("pipeline: EndStage %s iter %d: %v", stageName, iter, eerr)
		}
	}
	p.emit(rpc.EventStageEnded, map[string]any{
		"run_id": run.ID, "stage_id": stageID,
		"name": stageName, "iter": iter, "verdict": verdictKind,
	})

	if !ok {
		refs := []verdict.FileRef{{
			Path:      "(" + stageName + " failures)",
			Line:      0,
			Comment:   command + " exited non-zero:\n" + output,
			Reasoning: stageName + " must pass before the change is accepted",
		}}
		if p.Feedback != nil {
			if perr := p.Feedback.Put(ctx, run.ID, iter, Feedback{FileRefs: refs}); perr != nil {
				return false, fmt.Errorf("iter %d %s persist feedback: %w", iter, stageName, perr)
			}
		}
	}

	return ok, nil
}

// emit publishes a typed event if Events is wired. Best-effort:
// failures are silent (Publish must not block).
func (p *BuildPipeline) emit(t rpc.EventType, data map[string]any) {
	if p.Events == nil {
		return
	}
	p.Events.Publish(rpc.EventMessage{Type: t, Data: data})
}

// recordL3 writes an L3 stall row best-effort. Failures are logged
// and ignored — observability never blocks the pipeline.
func (p *BuildPipeline) recordL3(ctx context.Context, runID string, stageID int64, action, details string) {
	if p.Stalls == nil {
		return
	}
	if err := p.Stalls.RecordStall(ctx, runID, stageID, 3, time.Now().Unix(), action, details); err != nil {
		log.Printf("pipeline: RecordStall L3 run %s: %v", runID, err)
	}
}

// approvalSummary picks the right "done" summary string based on
// which post-review stages actually ran. Keeps backward-compatibility
// with pre-3.4 tests that asserted "approved on iter N" while still
// surfacing the new 3.4 stages in the typical case.
func approvalSummary(iter int, testCmd, validateCmd string) string {
	switch {
	case testCmd == "" && validateCmd == "":
		return "approved on iter " + strconv.Itoa(iter)
	case testCmd != "" && validateCmd != "":
		return "approved + tests + validate on iter " + strconv.Itoa(iter)
	case testCmd != "":
		return "approved + tests on iter " + strconv.Itoa(iter)
	default:
		return "approved + validate on iter " + strconv.Itoa(iter)
	}
}

func stageScratchDir(run *Run, iter int, name string) string {
	return filepath.Join(run.RuntimeDir, fmt.Sprintf("stage-%d-%s", iter, name))
}

// taskImplementPrompt builds the implement-stage user prompt from the task.
// The title is always present and is the primary descriptor; the body adds
// detail. Composing both ensures the worker always receives the task
// description — the implement system prompt only gives generic instructions
// and never includes the title — and that the prompt is never empty (claude
// --print rejects an empty prompt for title-only / empty-body tasks).
func taskImplementPrompt(t *store.Task) string {
	if strings.TrimSpace(t.Body) == "" {
		return t.Title
	}
	return t.Title + "\n\n" + t.Body
}

func (p *BuildPipeline) implementPrompt(run *Run, iter int, fb Feedback, pred *predictor.Result) string {
	if iter == 0 {
		base := "You are implementing a code change in this worktree. Read the task " +
			"description, make the changes, and explain what you did."
		if pred != nil && len(pred.InlineCapsules) > 0 {
			base = base + renderPrefetchBlock(pred)
		}
		// Inject prior-failure context when the task carries it from a
		// previous run that gave up. Malformed JSON or empty values skip
		// the block — fall through to the normal iter-0 return.
		if run.Task != nil && run.Task.LastFailureFeedback != "" {
			var prev struct {
				Summary       string            `json:"summary"`
				FileRefs      []verdict.FileRef `json:"file_refs"`
				ExhaustReason string            `json:"exhaust_reason"`
			}
			if json.Unmarshal([]byte(run.Task.LastFailureFeedback), &prev) == nil &&
				(prev.Summary != "" || len(prev.FileRefs) > 0 || prev.ExhaustReason != "") {
				var pb strings.Builder
				pb.WriteString("## A previous attempt at this task failed — address this before anything else\n\n")
				if prev.ExhaustReason != "" {
					pb.WriteString("Outcome: " + prev.ExhaustReason + "\n\n")
				}
				if prev.Summary != "" {
					pb.WriteString(prev.Summary + "\n\n")
				}
				for _, r := range prev.FileRefs {
					fmt.Fprintf(&pb, "- %s", r.Path)
					if r.Line > 0 {
						fmt.Fprintf(&pb, ":%d", r.Line)
					}
					pb.WriteString(" — " + r.Comment + "\n")
				}
				pb.WriteString("\n")
				return pb.String() + base
			}
		}
		return base
	}
	if fb.Summary == "" && len(fb.FileRefs) == 0 {
		// loud-fail handling lives in Task 10; for this task, fall back to generic
		return "You are revising a previous implementation. The reviewer requested " +
			"changes; apply them in this worktree."
	}
	var b strings.Builder
	if fb.Summary != "" {
		b.WriteString("## Previous review summary\n\n")
		b.WriteString(fb.Summary)
		b.WriteString("\n\n")
	}
	if len(fb.FileRefs) > 0 {
		b.WriteString("You are revising a previous implementation. The reviewer flagged ")
		b.WriteString("the following issues — address EVERY one:\n\n")
		for _, r := range fb.FileRefs {
			b.WriteString("- ")
			b.WriteString(r.Path)
			if r.Line > 0 {
				fmt.Fprintf(&b, ":%d", r.Line)
			}
			b.WriteString(" — ")
			b.WriteString(r.Comment)
			if r.Reasoning != "" {
				b.WriteString(" (")
				b.WriteString(r.Reasoning)
				b.WriteString(")")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderPrefetchBlock formats a predictor.Result for inclusion in the
// implement-stage system prompt. Top-K capsules go inline (each as a
// fenced code block with the target signature header); overflow
// candidates appear as a short pointer list; the prefetch.md path is
// referenced so the worker can read deeper context if needed.
func renderPrefetchBlock(pred *predictor.Result) string {
	var b strings.Builder
	b.WriteString("\n\n## Pre-fetched context\n\n")
	b.WriteString("The following capsules were pre-fetched by the predictor as likely-relevant to this task. ")
	b.WriteString("Read them before exploring the worktree from scratch.\n\n")
	for _, c := range pred.InlineCapsules {
		b.WriteString("```\n")
		b.WriteString(c.Raw)
		b.WriteString("\n```\n\n")
	}
	if len(pred.Overflow) > 0 {
		b.WriteString("### Additional candidates (not pre-fetched)\n\n")
		for _, cand := range pred.Overflow {
			b.WriteString("- `")
			b.WriteString(cand.File)
			if cand.Symbol != "" {
				b.WriteString(":")
				b.WriteString(cand.Symbol)
			}
			b.WriteString("` — ")
			b.WriteString(cand.Reason)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if pred.FullBundlePath != "" {
		b.WriteString("Full bundle (all fetched capsules): `")
		b.WriteString(pred.FullBundlePath)
		b.WriteString("`\n")
	}
	return b.String()
}

func (p *BuildPipeline) reviewPrompt(run *Run, iter int) string {
	return "You are reviewing code changes. Read the diff (`git diff` in this " +
		"worktree) and decide whether to APPROVE or request changes. You MUST " +
		"call hive_submit_review_verdict to finish; do not produce a final " +
		"text answer without calling the tool.\n\n" +
		"When verdict=CHANGES_REQUESTED, file_refs is REQUIRED. Enumerate every " +
		"change you want made, each as {path, line, comment, reasoning}. The " +
		"path is required; the line is optional (omit for file-level concerns). " +
		"The comment states WHAT to change; the reasoning explains WHY so the " +
		"implementer has context, not just a checklist. An empty file_refs on " +
		"CHANGES_REQUESTED will be rejected and the run will fail.\n\n" +
		"Also provide a summary: one paragraph stating the overall finding — " +
		"the cross-cutting concern or architectural observation that unifies your " +
		"per-file feedback. Use summary for holistic issues not anchored to any " +
		"single file (e.g. \"error handling is inconsistent throughout the " +
		"package\"). For APPROVE verdicts, summary can briefly confirm what looks " +
		"correct. summary is optional but strongly encouraged."
}

// derefRepoPath safely extracts the repo path from a Project, or returns
// "" if RepoPath is nil. Pipeline runs without a repo path shouldn't
// normally happen (scheduler.dispatch validates), but a defensive nil
// check keeps the FSM total in tests that don't set it.
func derefRepoPath(p *store.Project) string {
	if p == nil || p.RepoPath == nil {
		return ""
	}
	return *p.RepoPath
}

// computeCost returns a pointer to the USD cost computed from tokens
// at the model's price. Returns nil if the model isn't in the pricing
// registry — caller writes NULL in stages.cost_usd to distinguish from
// "free".
func computeCost(model string, tokens adapter.TokenUsage) *float64 {
	m, ok := pricing.Lookup(model)
	if !ok {
		// Operator-visible signal: a model not in our pricing registry
		// results in NULL cost, which silently aggregates to 0. Logging
		// here makes the "why is total cost flat after I changed models"
		// case discoverable. Per-stage, so at most ~twice per run.
		log.Printf("pipeline: no pricing for model %q; cost will be NULL", model)
		return nil
	}
	c := pricing.Cost(tokens.Input, tokens.Output, tokens.CacheHit, m)
	return &c
}

// toPipelineToolCalls converts the adapter's per-call records to the
// pipeline's mirror struct. Tests don't import internal/adapter.
func toPipelineToolCalls(in []adapter.ToolCallRecord) []ToolCallRecord {
	out := make([]ToolCallRecord, len(in))
	for i, c := range in {
		out[i] = ToolCallRecord{
			Name:      c.Name,
			ArgsJSON:  c.ArgsJSON,
			StartedAt: c.StartedAt,
			EndedAt:   c.EndedAt,
			Success:   c.Success,
		}
	}
	return out
}

// RunDocument re-runs the terminal documenter stage standalone, outside
// the normal Build FSM (Phase 4.4 follow-up: `hive document <run-id>`).
// Used to fill in docs after a non-blocking skip. Requires the documenter
// to be enabled (it supplies model/timeout/prompt config). Returns
// (skipped, reason): skipped=true means the stage ran but errored.
func (p *BuildPipeline) RunDocument(ctx context.Context, run *Run) (bool, string, error) {
	if !p.Cfg.DocumenterEnabled {
		return false, "", errDocumenterDisabled
	}
	skipped, reason := p.runDocumentStage(ctx, run, 0)
	return skipped, reason, nil
}

// errDocumenterDisabled is returned by RunDocument when the documenter is
// off in config — the manual re-run needs its model/prompt settings.
var errDocumenterDisabled = errors.New("documenter is disabled in config")

// runDocumentStage runs the terminal documenter stage (Phase 4.4).
// Non-blocking: it never returns an error to the FSM. Returns
// (skipped, reason): skipped is true only when the documenter was
// enabled but its stage failed (the run still completes "done"). When
// the documenter is disabled it returns (false, "") — not a skip, off.
func (p *BuildPipeline) runDocumentStage(ctx context.Context, run *Run, iter int) (bool, string) {
	if !p.Cfg.DocumenterEnabled {
		return false, ""
	}
	model := p.Cfg.DocumenterModel
	if model == "" {
		model = p.Cfg.Ladder.WorkerAt(0)
	}
	timeout := p.Cfg.DocumenterTimeout
	if timeout == 0 {
		timeout = p.Cfg.StageTimeout
	}
	stageID := int64(0)
	if p.Stages != nil {
		id, berr := p.Stages.BeginStage(ctx, run.ID, "document", iter, model)
		if berr != nil {
			log.Printf("pipeline: BeginStage document iter %d: %v", iter, berr)
		} else {
			stageID = id
		}
	}
	p.emit(rpc.EventStageStarted, map[string]any{
		"run_id": run.ID, "stage_id": stageID, "name": "document", "iter": iter, "model": model,
	})
	out, err := p.Adapter.RunStage(ctx, adapter.StageRequest{
		RunID:            run.ID,
		StageName:        "document",
		Iter:             iter,
		Model:            model,
		Skills:           p.Cfg.DocumenterSkills,
		SystemPrompt:     p.documentPrompt(),
		UserPrompt:       "Document the change in this worktree.",
		Timeout:          timeout,
		Cwd:              run.WorktreePath,
		StageDir:         stageScratchDir(run, iter, "document"),
		RunDir:           run.RuntimeDir,
		OriginalRepoPath: derefRepoPath(run.Project),
		DocToolName:      "hive_submit_documentation",
		StageID:          stageID,
	})
	if err != nil {
		if p.Stages != nil && stageID != 0 {
			_ = p.Stages.EndStage(ctx, stageID, "", nil, 0, 0, 0, nil)
		}
		p.emit(rpc.EventStageEnded, map[string]any{
			"run_id": run.ID, "stage_id": stageID, "name": "document", "iter": iter,
		})
		log.Printf("pipeline: documenter stage failed for run %s (non-blocking): %v", run.ID, err)
		return true, "documenter stage error: " + err.Error()
	}
	if p.Stages != nil && stageID != 0 {
		cost := computeCost(model, out.Tokens)
		if eerr := p.Stages.EndStage(ctx, stageID, "", nil, out.Tokens.Input, out.Tokens.Output, out.Tokens.CacheHit, cost); eerr != nil {
			log.Printf("pipeline: EndStage document iter %d: %v", iter, eerr)
		}
		if perr := p.Stages.PutToolCalls(ctx, run.ID, stageID, toPipelineToolCalls(out.ToolCalls)); perr != nil {
			log.Printf("pipeline: PutToolCalls document iter %d: %v", iter, perr)
		}
	}
	p.emit(rpc.EventStageEnded, map[string]any{
		"run_id": run.ID, "stage_id": stageID, "name": "document", "iter": iter,
		"tokens_in": out.Tokens.Input, "tokens_out": out.Tokens.Output,
	})
	return false, ""
}

func (p *BuildPipeline) documentPrompt() string {
	b := "The change in this worktree has been implemented, reviewed, and passed " +
		"tests. Document it. Append a concise CHANGELOG entry summarizing WHAT " +
		"changed and WHY."
	if p.Cfg.DocumenterUpdateReadme {
		b += " Update the README if the change affects public API or setup."
	}
	if p.Cfg.DocumenterCodeComments {
		b += " Add brief code comments ONLY where the WHY is non-obvious (a hidden " +
			"constraint, a subtle invariant, a workaround) — never narrate what the " +
			"code already says."
	} else {
		b += " Do NOT add inline code comments this run; limit yourself to the " +
			"CHANGELOG and existing doc files."
	}
	b += " After writing the docs, call the hive_submit_documentation tool once with a " +
		"one-paragraph summary of what you documented, the list of files you changed, and " +
		"the CHANGELOG entry text. This is optional but preferred — it records what was documented."
	return b
}
