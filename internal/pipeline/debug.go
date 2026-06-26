package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/internal/verdict"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// DebugConfig configures the Debug pipeline.
type DebugConfig struct {
	MaxIterations       int
	Ladder              ModelLadder
	StageTimeout        time.Duration
	VerifyCommand       string // shell gate; empty means verify always "passes"
	VerifyStageTimeout  time.Duration
	ShellOutputMaxBytes int
	SkillsReproduce     []string
	SkillsIsolate       []string
	SkillsFix           []string
}

// DebugPipeline implements the Debug FSM:
//
//	reproduce -> (isolate -> fix -> verify)*
//
// reproduce adds a failing test; the loop isolates + fixes and re-runs
// the verify shell command until it passes (or MaxIterations).
type DebugPipeline struct {
	Adapter  adapter.Adapter
	Feedback FeedbackStore
	Stages   StageStore
	Events   EventPublisher
	Cfg      DebugConfig
}

func (*DebugPipeline) Name() string { return "debug" }

func (p *DebugPipeline) exec() stageExec {
	return stageExec{adapter: p.Adapter, stages: p.Stages, events: p.Events, timeout: p.Cfg.StageTimeout}
}

func (p *DebugPipeline) Run(ctx context.Context, run *Run) (*Result, error) {
	res := &Result{}
	maxIters := p.Cfg.MaxIterations
	if maxIters <= 0 {
		maxIters = 3
	}
	ex := p.exec()

	// reproduce (single pass): add a failing test capturing the bug.
	if _, err := ex.worker(ctx, run, 0, "reproduce", p.Cfg.Ladder.WorkerAt(0),
		p.Cfg.SkillsReproduce, p.reproducePrompt(), run.Task.Body); err != nil {
		return nil, fmt.Errorf("reproduce: %w", err)
	}

	for iter := 0; iter < maxIters; iter++ {
		var priorRefs []verdict.FileRef
		if iter > 0 && p.Feedback != nil {
			fb, ferr := p.Feedback.Get(ctx, run.ID, iter-1)
			if ferr != nil && !errors.Is(ferr, ErrFeedbackNotFound) {
				return nil, fmt.Errorf("iter %d read feedback: %w", iter, ferr)
			}
			priorRefs = fb.FileRefs
		}

		if _, err := ex.worker(ctx, run, iter, "isolate", p.Cfg.Ladder.WorkerAt(iter),
			p.Cfg.SkillsIsolate, p.isolatePrompt(priorRefs), "Diagnose the root cause."); err != nil {
			return nil, fmt.Errorf("iter %d isolate: %w", iter, err)
		}
		if _, err := ex.worker(ctx, run, iter, "fix", p.Cfg.Ladder.WorkerAt(iter),
			p.Cfg.SkillsFix, p.fixPrompt(priorRefs), "Apply the fix."); err != nil {
			return nil, fmt.Errorf("iter %d fix: %w", iter, err)
		}

		ok, verr := p.runVerify(ctx, run, iter)
		if verr != nil {
			return nil, verr
		}
		res.Iterations = iter + 1
		if ok {
			res.Status = "done"
			res.Summary = "bug fixed + verified on iter " + strconv.Itoa(iter)
			res.EndedAt = time.Now()
			return res, nil
		}
		// verify failed: runVerify wrote synthetic feedback; loop.
	}

	res.Status = "needs_attention"
	res.Summary = "verify never passed within " + strconv.Itoa(maxIters) + " iterations"
	res.EndedAt = time.Now()
	return res, nil
}

// runVerify runs the verify shell gate. Empty command => pass. On
// failure, writes a synthetic FileRef so the next iter's isolate/fix
// prompts include the failure output. Mirrors Build's shell-stage gate.
func (p *DebugPipeline) runVerify(ctx context.Context, run *Run, iter int) (bool, error) {
	command := p.Cfg.VerifyCommand
	if command == "" {
		return true, nil
	}
	maxBytes := p.Cfg.ShellOutputMaxBytes
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	ex := p.exec()
	stageID := int64(0)
	if p.Stages != nil {
		id, berr := p.Stages.BeginStage(ctx, run.ID, "verify", iter, "")
		if berr != nil {
			log.Printf("debug: BeginStage verify iter %d: %v", iter, berr)
		} else {
			stageID = id
		}
	}
	ex.emit(rpc.EventStageStarted, map[string]any{
		"run_id": run.ID, "stage_id": stageID, "name": "verify", "iter": iter,
	})
	output, ok, runErr := RunShellStage(ctx, command, run.WorktreePath, p.Cfg.VerifyStageTimeout, maxBytes)
	if runErr != nil {
		if p.Stages != nil && stageID != 0 {
			_ = p.Stages.EndStage(ctx, stageID, "", nil, 0, 0, 0, nil)
		}
		return false, fmt.Errorf("iter %d verify: %w", iter, runErr)
	}
	verdictKind := "APPROVE"
	if !ok {
		verdictKind = "CHANGES_REQUESTED"
	}
	if p.Stages != nil && stageID != 0 {
		if eerr := p.Stages.EndStage(ctx, stageID, verdictKind, nil, 0, 0, 0, nil); eerr != nil {
			log.Printf("debug: EndStage verify iter %d: %v", iter, eerr)
		}
	}
	ex.emit(rpc.EventStageEnded, map[string]any{
		"run_id": run.ID, "stage_id": stageID, "name": "verify", "iter": iter, "verdict": verdictKind,
	})
	if !ok && p.Feedback != nil {
		refs := []verdict.FileRef{{
			Path:      "(verify failures)",
			Comment:   command + " exited non-zero:\n" + output,
			Reasoning: "verify must pass before the bug is considered fixed",
		}}
		if perr := p.Feedback.Put(ctx, run.ID, iter, Feedback{FileRefs: refs}); perr != nil {
			return false, fmt.Errorf("iter %d verify persist feedback: %w", iter, perr)
		}
	}
	return ok, nil
}

func (p *DebugPipeline) reproducePrompt() string {
	return "You are debugging a reported issue. FIRST reproduce it: add a FAILING " +
		"test in this worktree that captures the bug (it must fail for the right " +
		"reason). Do NOT fix the bug yet — only demonstrate it."
}

func (p *DebugPipeline) isolatePrompt(refs []verdict.FileRef) string {
	return "Diagnose the ROOT CAUSE of the failing test / reported bug. Explain " +
		"what is actually wrong before changing code." + renderDebugFeedback(refs)
}

func (p *DebugPipeline) fixPrompt(refs []verdict.FileRef) string {
	return "Apply the minimal fix so the failing test passes, without breaking " +
		"other tests." + renderDebugFeedback(refs)
}

// renderDebugFeedback appends prior verify-failure output to a prompt.
func renderDebugFeedback(refs []verdict.FileRef) string {
	if len(refs) == 0 {
		return ""
	}
	s := "\n\nThe previous attempt did not pass verify:\n"
	for _, r := range refs {
		s += "- " + r.Comment + "\n"
	}
	return s
}
