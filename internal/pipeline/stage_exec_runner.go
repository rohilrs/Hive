package pipeline

import (
	"context"
	"log"
	"time"

	"github.com/rohilrs/Hive/internal/adapter"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// stageExec runs individual pipeline stages with stage-row + event
// observability. Shared by the Plan and Debug pipelines. Build keeps its
// own inline execution (interleaved with loop detection + test/validate
// gates), so it does not use this.
type stageExec struct {
	adapter adapter.Adapter
	stages  StageStore
	events  EventPublisher
	timeout time.Duration
}

func (e stageExec) emit(t rpc.EventType, data map[string]any) {
	if e.events == nil {
		return
	}
	e.events.Publish(rpc.EventMessage{Type: t, Data: data})
}

// worker runs a non-verdict worker stage.
func (e stageExec) worker(ctx context.Context, run *Run, iter int, stageName, model string, skills []string, systemPrompt, userPrompt string) (*adapter.StageOutput, error) {
	return e.run(ctx, run, iter, stageName, model, skills, systemPrompt, userPrompt, "")
}

// verdict runs a verdict-driven stage; verdictTool is wired so the worker
// must call it, and the stage row records the verdict kind + confidence.
func (e stageExec) verdict(ctx context.Context, run *Run, iter int, stageName, model string, skills []string, systemPrompt, userPrompt, verdictTool string) (*adapter.StageOutput, error) {
	return e.run(ctx, run, iter, stageName, model, skills, systemPrompt, userPrompt, verdictTool)
}

func (e stageExec) run(ctx context.Context, run *Run, iter int, stageName, model string, skills []string, systemPrompt, userPrompt, verdictTool string) (*adapter.StageOutput, error) {
	stageID := int64(0)
	if e.stages != nil {
		id, berr := e.stages.BeginStage(ctx, run.ID, stageName, iter, model)
		if berr != nil {
			log.Printf("pipeline: BeginStage %s iter %d: %v", stageName, iter, berr)
		} else {
			stageID = id
		}
	}
	e.emit(rpc.EventStageStarted, map[string]any{
		"run_id": run.ID, "stage_id": stageID, "name": stageName, "iter": iter, "model": model,
	})
	out, err := e.adapter.RunStage(ctx, adapter.StageRequest{
		RunID:            run.ID,
		StageName:        stageName,
		Iter:             iter,
		Model:            model,
		Skills:           skills,
		SystemPrompt:     systemPrompt,
		UserPrompt:       userPrompt,
		Timeout:          e.timeout,
		Cwd:              run.WorktreePath,
		StageDir:         stageScratchDir(run, iter, stageName),
		RunDir:           run.RuntimeDir,
		OriginalRepoPath: derefRepoPath(run.Project),
		VerdictToolName:  verdictTool,
		StageID:          stageID,
	})
	if err != nil {
		return nil, err
	}
	if e.stages != nil && stageID != 0 {
		var vKind string
		var vConf *float64
		if verdictTool != "" && out.Verdict != nil {
			vKind = string(out.Verdict.Kind)
			if out.Verdict.Confidence > 0 {
				c := float64(out.Verdict.Confidence) / 100.0
				vConf = &c
			}
		}
		cost := computeCost(model, out.Tokens)
		if eerr := e.stages.EndStage(ctx, stageID, vKind, vConf, out.Tokens.Input, out.Tokens.Output, out.Tokens.CacheHit, cost); eerr != nil {
			log.Printf("pipeline: EndStage %s iter %d: %v", stageName, iter, eerr)
		}
		if perr := e.stages.PutToolCalls(ctx, run.ID, stageID, toPipelineToolCalls(out.ToolCalls)); perr != nil {
			log.Printf("pipeline: PutToolCalls %s iter %d: %v", stageName, iter, perr)
		}
	}
	endData := map[string]any{
		"run_id": run.ID, "stage_id": stageID, "name": stageName, "iter": iter,
		"tokens_in": out.Tokens.Input, "tokens_out": out.Tokens.Output,
	}
	if verdictTool != "" {
		v := ""
		if out.Verdict != nil {
			v = string(out.Verdict.Kind)
		}
		endData["verdict"] = v
	}
	e.emit(rpc.EventStageEnded, endData)
	return out, nil
}
