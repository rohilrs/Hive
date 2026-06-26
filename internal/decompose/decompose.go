package decompose

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/store"
	"github.com/rohilrs/Hive/internal/store/pricing"
)

// DefaultMaxSubtasks is the per-call cap when the caller passes 0.
const DefaultMaxSubtasks = 10

// HardMaxSubtasks is the absolute ceiling regardless of caller request.
const HardMaxSubtasks = 20

// maxRelevantFiles caps a sub-task's relevant_files list so the metadata stays bounded.
const maxRelevantFiles = 20

// DefaultModel is the Anthropic model Decompose uses by default.
// Sonnet for quality; cost is bounded by per-turn token usage.
const DefaultModel = "claude-sonnet-4-6"

const submitToolName = "submit_subtasks"

// ExistingRef is one existing-work item handed to Decompose: its ref (the valid
// merge_from token) plus the pre-formatted prompt line. The daemon builds these
// from ExistingItem so the decompose package stays free of store/source deps.
type ExistingRef struct {
	Ref   string
	Block string
}

var validPriority = map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}
var validPipeline = map[string]bool{"build": true, "debug": true, "plan": true, "finish-branch": true}

const systemPrompt = `You break down a software-engineering task into a sequence of independently shippable sub-tasks. Each sub-task should:
- Be small enough to ship in one pipeline run (rough rule: under a day's work).
- Have a clear acceptance criterion. When the project's pipeline commands are
  provided, write acceptance criteria using THOSE exact commands — do NOT assume
  a language or toolchain (e.g. don't write "go test" for a Node/pnpm project).
- Be ordered: earlier sub-tasks unblock later ones.
- Avoid speculative scope. Better to under-decompose than to invent work.

Sub-tasks run in PARALLEL by default. If a sub-task can only succeed after a sibling's work is merged (it imports/wires/uses that sibling's output), set its ` + "`" + `depends_on` + "`" + ` to the indices of those earlier siblings — don't rely on ordering alone.

Return the breakdown via the ` + "`" + `submit_subtasks` + "`" + ` tool. Do NOT respond with prose — the only valid output is the tool call.

Priority guidance (Hive uses P0..P3 where P0 is highest):
- "P0" for sub-tasks blocking the rest of the breakdown.
- "P1" for sequential mainline work.
- "P2" for cleanups / polish.
- "P3" for nice-to-haves / opportunistic.

Pipeline guidance (omit if unsure — default is build):
- "build" for new features / additions (default).
- "debug" for fix work where the issue is known but root cause is unclear.
- "plan" when the sub-task needs a spec + plan written before any code.
- "finish-branch" only for branch-cleanup / PR-finalization work.

Make each sub-task implement-ready so the downstream coding agent can start without rediscovering the codebase:
- Write the ` + "`" + `body` + "`" + ` with a "## Implementation context" section naming the concrete files to touch, the key functions/types to add or modify, existing patterns to follow, and integration points.
- Set ` + "`" + `relevant_files` + "`" + ` to the repo-relative paths the sub-task is expected to create or modify (best estimate).`

// Decompose runs one tool-use turn against the given Runner to produce
// a validated proposal of sub-tasks. Pure: no DB writes, no event-bus
// publishes. maxSubtasks <= 0 → DefaultMaxSubtasks; capped at HardMaxSubtasks.
// stackHint, when non-empty, is appended to the user prompt to tell the model
// the project's actual build/test toolchain (e.g. pnpm commands) so acceptance
// criteria don't default to a wrong language (Go's `go test`). Callers in the
// daemon build it from the per-project pipeline config.
func Decompose(ctx context.Context, runner Runner, task store.Task, project store.Project, maxSubtasks int, stackHint string, existing []ExistingRef, codebaseContext string) (*Result, error) {
	if maxSubtasks <= 0 {
		maxSubtasks = DefaultMaxSubtasks
	}
	if maxSubtasks > HardMaxSubtasks {
		maxSubtasks = HardMaxSubtasks
	}

	userPrompt := fmt.Sprintf(`Decompose this task into sub-tasks:

Title: %s
Body:
%s

Project: %s (%s)
Pipeline (parent's): %s`, task.Title, task.Body, project.Name, project.Slug, task.Pipeline)
	if strings.TrimSpace(stackHint) != "" {
		userPrompt += "\n\n" + stackHint
	}
	if strings.TrimSpace(codebaseContext) != "" {
		userPrompt += "\n\n" + codebaseContext
	}
	if len(existing) > 0 {
		var eb strings.Builder
		eb.WriteString("\n\nEXISTING WORK — reconcile against these. If a proposed sub-task covers work already represented below, MERGE it: write the unified sub-task and set merge_from to that item's ref. Do NOT emit a separate duplicate. Only items that genuinely belong to THIS phase should be merged; ignore unrelated ones.\n")
		for _, e := range existing {
			eb.WriteString(e.Block)
			if !strings.HasSuffix(e.Block, "\n") {
				eb.WriteString("\n")
			}
		}
		userPrompt += eb.String()
	}

	tool := anthropic.ToolDef{
		Name:        submitToolName,
		Description: "Return the proposed sub-task breakdown. Each sub-task should be independently shippable.",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"subtasks"},
			"properties": map[string]any{
				"subtasks": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": HardMaxSubtasks,
					"items": map[string]any{
						"type":     "object",
						"required": []any{"title", "body", "priority"},
						"properties": map[string]any{
							"title":      map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
							"body":       map[string]any{"type": "string", "minLength": 1},
							"priority":   map[string]any{"type": "string", "enum": []any{"P0", "P1", "P2", "P3"}},
							"pipeline":   map[string]any{"type": "string", "enum": []any{"build", "debug", "plan", "finish-branch"}},
							"merge_from": map[string]any{"type": "string", "description": "If this sub-task covers an existing item from EXISTING WORK, set this to that item's ref (e.g. \"hive:task-123\" or \"linear:uuid\"). Omit for new work."},
							"depends_on": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "integer", "minimum": 0},
								"description": "0-based indices of EARLIER sub-tasks in this list that must MERGE before this one starts (only reference sub-tasks BEFORE this one). Set this when a sub-task builds on a sibling's output (e.g. a 'wire everything together' task depends on the tasks that produce the pieces). Omit for independent, parallel-safe tasks.",
							},
							"relevant_files": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Repo-relative file paths this sub-task is expected to CREATE or MODIFY (your best estimate). Used to prime the implementer's tooling.",
							},
						},
					},
				},
			},
		},
	}

	turnOut, err := runner.RunTurn(ctx, anthropic.TurnInput{
		Model:     DefaultModel,
		System:    systemPrompt,
		Messages:  []anth.MessageParam{anth.NewUserMessage(anth.NewTextBlock(userPrompt))},
		Tools:     []anthropic.ToolDef{tool},
		MaxTokens: 4096,
	})
	if err != nil {
		return nil, fmt.Errorf("decompose: %w", err)
	}
	if turnOut.StopReason != "tool_use" || len(turnOut.ToolCalls) == 0 {
		return nil, fmt.Errorf("decompose: assistant did not call submit_subtasks (stop_reason=%s)", turnOut.StopReason)
	}

	// Find the submit_subtasks call (assistant may emit other tools
	// theoretically; we only honor ours).
	var raw json.RawMessage
	for _, tc := range turnOut.ToolCalls {
		if tc.Name == submitToolName {
			raw = tc.Input
			break
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("decompose: no submit_subtasks tool_use in response")
	}

	var parsed struct {
		Subtasks []ProposedSubtask `json:"subtasks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decompose: parse subtasks: %w", err)
	}

	clean, err := Validate(parsed.Subtasks, maxSubtasks)
	if err != nil {
		return nil, err
	}

	if len(existing) > 0 {
		validRefs := make(map[string]bool, len(existing))
		for _, e := range existing {
			validRefs[e.Ref] = true
		}
		for i := range clean {
			mf := strings.TrimSpace(clean[i].MergeFrom)
			clean[i].MergeFrom = mf
			if mf == "" {
				continue
			}
			if !strings.HasPrefix(mf, "hive:") && !strings.HasPrefix(mf, "linear:") {
				return nil, fmt.Errorf("decompose: invalid merge_from %q at index %d (want hive:/linear: prefix)", mf, i)
			}
			if !validRefs[mf] {
				return nil, fmt.Errorf("decompose: fabricated merge_from %q at index %d (not in existing work set)", mf, i)
			}
		}
	} else {
		// No existing-work context → merge_from is meaningless; strip any stray value
		// so a hallucinated ref can't leak onto a new task.
		for i := range clean {
			clean[i].MergeFrom = ""
		}
	}

	// Cost calc via the pricing helper. Lookup falls back to (Model{}, false)
	// for unknown model; cost is then 0 — a non-zero token count with zero
	// cost is a signal the pricing table needs updating.
	mp, _ := pricing.Lookup(DefaultModel)
	cost := pricing.Cost(int(turnOut.TokensIn), int(turnOut.TokensOut), 0, mp)

	return &Result{
		Subtasks:     clean,
		Model:        DefaultModel,
		CostUSD:      cost,
		InputTokens:  int(turnOut.TokensIn),
		OutputTokens: int(turnOut.TokensOut),
	}, nil
}

// Validate enforces the rules from spec §8 on a ProposedSubtask slice:
// ≥1 item, ≤maxSubtasks, valid priority + pipeline, non-empty title
// (≤200 chars) + body. Dedupes duplicate titles silently (keeps first).
// Returns the cleaned slice (with defaults applied, dups removed) or
// an error.
//
// Called by Decompose post-LLM-response and by the daemon's
// handleDecomposeApply RPC handler (defense against tampered payloads).
func Validate(items []ProposedSubtask, maxSubtasks int) ([]ProposedSubtask, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("decompose: empty breakdown")
	}
	if len(items) > maxSubtasks {
		return nil, fmt.Errorf("decompose: oversize breakdown (got %d, max %d)", len(items), maxSubtasks)
	}

	seen := map[string]bool{}
	out := make([]ProposedSubtask, 0, len(items))
	// inputToOutput[inputIdx] = index in `out`, or -1 if that input item was
	// dropped (duplicate title). Used to remap depends_on after dedup.
	inputToOutput := make([]int, len(items))
	for k := range inputToOutput {
		inputToOutput[k] = -1
	}
	for i, it := range items {
		it.Title = strings.TrimSpace(it.Title)
		if it.Title == "" {
			return nil, fmt.Errorf("decompose: empty title at index %d", i)
		}
		if len(it.Title) > 200 {
			return nil, fmt.Errorf("decompose: title at index %d too long (%d chars, max 200)", i, len(it.Title))
		}
		if strings.TrimSpace(it.Body) == "" {
			return nil, fmt.Errorf("decompose: empty body at index %d", i)
		}
		if !validPriority[it.Priority] {
			return nil, fmt.Errorf("decompose: invalid priority %q at index %d", it.Priority, i)
		}
		if it.Pipeline == "" {
			it.Pipeline = "build"
		}
		if !validPipeline[it.Pipeline] {
			return nil, fmt.Errorf("decompose: invalid pipeline %q at index %d", it.Pipeline, i)
		}
		// Sanitize depends_on into a DAG: keep only backward references
		// (0 <= dep < i), deduped, ascending. Drop forward/self/negative/
		// out-of-range indices rather than hard-failing on a model mistake.
		if len(it.DependsOn) > 0 {
			depSeen := map[int]bool{}
			cleanDeps := make([]int, 0, len(it.DependsOn))
			for _, d := range it.DependsOn {
				if d < 0 || d >= i || depSeen[d] {
					continue
				}
				depSeen[d] = true
				cleanDeps = append(cleanDeps, d)
			}
			sort.Ints(cleanDeps)
			if len(cleanDeps) == 0 {
				it.DependsOn = nil
			} else {
				it.DependsOn = cleanDeps
			}
		}
		// Sanitize relevant_files: trim, drop empties, dedupe, cap.
		if len(it.RelevantFiles) > 0 {
			rfSeen := map[string]bool{}
			cleanRF := make([]string, 0, len(it.RelevantFiles))
			for _, f := range it.RelevantFiles {
				f = strings.TrimSpace(f)
				if f == "" || rfSeen[f] {
					continue
				}
				rfSeen[f] = true
				cleanRF = append(cleanRF, f)
				if len(cleanRF) >= maxRelevantFiles {
					break
				}
			}
			if len(cleanRF) == 0 {
				it.RelevantFiles = nil
			} else {
				it.RelevantFiles = cleanRF
			}
		}
		if seen[it.Title] {
			continue // dedupe silently — keep first
		}
		seen[it.Title] = true
		inputToOutput[i] = len(out)
		out = append(out, it)
	}
	// Remap depends_on from original-input indices to output indices. Dedup may
	// have dropped earlier items, so an input index no longer lines up with the
	// returned slice; drop deps whose target was a dropped duplicate, re-check
	// backward-only in output space, dedup, sort.
	for oi := range out {
		if len(out[oi].DependsOn) == 0 {
			continue
		}
		remapped := make([]int, 0, len(out[oi].DependsOn))
		depSeen := map[int]bool{}
		for _, in := range out[oi].DependsOn {
			if in < 0 || in >= len(inputToOutput) {
				continue
			}
			o := inputToOutput[in]
			if o < 0 || o >= oi || depSeen[o] {
				continue // dropped dup, not strictly backward in output space, or already added
			}
			depSeen[o] = true
			remapped = append(remapped, o)
		}
		sort.Ints(remapped)
		if len(remapped) == 0 {
			out[oi].DependsOn = nil
		} else {
			out[oi].DependsOn = remapped
		}
	}
	return out, nil
}
