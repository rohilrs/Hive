package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/verdict"
)

// PlanConfig configures the Plan pipeline.
type PlanConfig struct {
	MaxIterations    int         // spec-write<->spec-review loop cap
	Ladder           ModelLadder // worker + reviewer model ladders
	StageTimeout     time.Duration
	SkillsBrainstorm []string
	SkillsSpec       []string
	SkillsReview     []string
	SkillsPlan       []string
}

// PlanPipeline implements the Plan FSM:
//
//	brainstorm -> (spec-write <-> spec-review)* -> plan-write
//
// brainstorm + plan-write are single worker passes; the middle is a
// verdict-driven loop reusing Build's FileRefs feedback path. The worker
// writes spec/plan markdown to docs/superpowers/{specs,plans}/ on the
// branch, which the operator reviews.
type PlanPipeline struct {
	Adapter  adapter.Adapter
	Feedback FeedbackStore
	Stages   StageStore
	Events   EventPublisher
	Cfg      PlanConfig
}

func (*PlanPipeline) Name() string { return "plan" }

func (p *PlanPipeline) exec() stageExec {
	return stageExec{adapter: p.Adapter, stages: p.Stages, events: p.Events, timeout: p.Cfg.StageTimeout}
}

func (p *PlanPipeline) Run(ctx context.Context, run *Run) (*Result, error) {
	res := &Result{}
	maxIters := p.Cfg.MaxIterations
	if maxIters <= 0 {
		maxIters = 3
	}
	ex := p.exec()

	// Stage 1: brainstorm (single worker pass).
	if _, err := ex.worker(ctx, run, 0, "brainstorm",
		p.Cfg.Ladder.WorkerAt(0), p.Cfg.SkillsBrainstorm,
		p.brainstormPrompt(), run.Task.Body); err != nil {
		return nil, fmt.Errorf("brainstorm: %w", err)
	}

	// Stage 2-3: spec-write <-> spec-review loop.
	specApproved := false
	for iter := 0; iter < maxIters; iter++ {
		var priorRefs []verdict.FileRef
		if iter > 0 && p.Feedback != nil {
			fb, ferr := p.Feedback.Get(ctx, run.ID, iter-1)
			if ferr != nil && !errors.Is(ferr, ErrFeedbackNotFound) {
				return nil, fmt.Errorf("iter %d read feedback: %w", iter, ferr)
			}
			priorRefs = fb.FileRefs
		}

		if _, err := ex.worker(ctx, run, iter, "spec-write",
			p.Cfg.Ladder.WorkerAt(iter), p.Cfg.SkillsSpec,
			p.specWritePrompt(iter, priorRefs), "Write/revise the spec in this worktree."); err != nil {
			return nil, fmt.Errorf("iter %d spec-write: %w", iter, err)
		}

		revOut, err := ex.verdict(ctx, run, iter, "spec-review",
			p.Cfg.Ladder.ReviewerAt(iter), p.Cfg.SkillsReview,
			p.specReviewPrompt(), "Review the spec in this worktree.", "hive_submit_review_verdict")
		if err != nil {
			return nil, fmt.Errorf("iter %d spec-review: %w", iter, err)
		}
		res.Iterations = iter + 1

		if p.Feedback != nil && revOut.Verdict != nil && revOut.Verdict.FromTool && len(revOut.Verdict.FileRefs) > 0 {
			if perr := p.Feedback.Put(ctx, run.ID, iter, Feedback{
				Summary:  revOut.Verdict.Summary,
				FileRefs: revOut.Verdict.FileRefs,
			}); perr != nil {
				return nil, fmt.Errorf("iter %d persist feedback: %w", iter, perr)
			}
		}
		if revOut.Verdict != nil && revOut.Verdict.Kind == adapter.VerdictApprove {
			specApproved = true
			break
		}
	}

	if !specApproved {
		res.Status = "needs_attention"
		res.Summary = "spec not approved within " + strconv.Itoa(maxIters) + " iterations"
		res.EndedAt = time.Now()
		return res, nil
	}

	// Stage 4: plan-write (single worker pass).
	if _, err := ex.worker(ctx, run, 0, "plan-write",
		p.Cfg.Ladder.WorkerAt(0), p.Cfg.SkillsPlan,
		p.planWritePrompt(), "Write the implementation plan in this worktree."); err != nil {
		return nil, fmt.Errorf("plan-write: %w", err)
	}

	res.Status = "done"
	res.Summary = "spec approved on iter " + strconv.Itoa(res.Iterations) + "; plan written"
	res.EndedAt = time.Now()
	return res, nil
}

func (p *PlanPipeline) brainstormPrompt() string {
	return "You are scoping a task BEFORE any implementation. Read the task " +
		"description and explore this worktree to understand the codebase. Produce " +
		"a short requirements analysis and write it to a new file under " +
		"docs/superpowers/specs/ named <short-slug>-brainstorm.md (create the " +
		"directory if needed). Cover: the goal, constraints/assumptions, open " +
		"questions, and a proposed approach. Do NOT write production code."
}

func (p *PlanPipeline) specWritePrompt(iter int, refs []verdict.FileRef) string {
	if iter == 0 || len(refs) == 0 {
		return "Using the brainstorm notes, write a design spec for this task to a " +
			"new file under docs/superpowers/specs/ named " +
			"YYYY-MM-DD-<short-slug>.md (today's date). Cover: TL;DR, goals, " +
			"non-goals, architecture, and a sub-task breakdown. Do NOT write " +
			"production code — this is a design document."
	}
	var b strings.Builder
	b.WriteString("Revise the design spec you wrote. The reviewer requested these ")
	b.WriteString("changes — address EVERY one:\n\n")
	for _, r := range refs {
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
	return b.String()
}

func (p *PlanPipeline) specReviewPrompt() string {
	return "You are reviewing a DESIGN SPEC (not code). Read the spec under " +
		"docs/superpowers/specs/ and decide whether it is complete, coherent, and " +
		"implementable. You MUST call hive_submit_review_verdict to finish; do not " +
		"produce a final text answer without calling the tool.\n\n" +
		"When verdict=CHANGES_REQUESTED, file_refs is REQUIRED: enumerate every " +
		"change, each as {path, line, comment, reasoning}. path is required, line " +
		"optional. The comment states WHAT to change; reasoning explains WHY. An " +
		"empty file_refs on CHANGES_REQUESTED will be rejected and the run fails."
}

func (p *PlanPipeline) planWritePrompt() string {
	return "The design spec is approved. Write a task-by-task implementation plan " +
		"to a new file under docs/superpowers/plans/ named YYYY-MM-DD-<short-slug>.md " +
		"(today's date). Follow a bite-sized TDD structure: each task lists exact " +
		"file paths, a failing test, the minimal implementation, the verification " +
		"command, and a commit. Use the approved spec as the source of truth."
}
