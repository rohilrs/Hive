package claudecli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/verdict"
)

const loopSimilarityModel = "claude-haiku-4-5"

const loopSimilaritySystemPrompt = `You are a loop-detection heuristic for an automated code-review pipeline. Two consecutive iterations of (worker edit + reviewer feedback) are provided. Your job is to judge whether the model is going in circles — making the same edits while the reviewer asks for the same changes — versus making real progress.

Score 1.0 = identical situation (same files touched, same reviewer comments, no meaningful change between iters).
Score 0.5 = partial overlap; some progress but the reviewer is still flagging related concerns.
Score 0.0 = different situation entirely; worker pivoted to a different area or reviewer escalated to new issues.

Return ONLY raw JSON with this exact shape, no prose, no markdown fences:
{"similarity": 0.0-1.0, "reason": "<one short sentence>"}`

// ClassifyLoopSimilarity satisfies pipeline.LoopDetector. Returns
// similarity in [0.0, 1.0]; on parse / subprocess failure, returns
// (0, error) so the pipeline gracefully degrades to "no loop detected"
// while still logging the failure for operator visibility.
func (c *Client) ClassifyLoopSimilarity(ctx context.Context, prev, curr pipeline.Iteration) (float64, error) {
	userText := fmt.Sprintf(`PREVIOUS ITERATION
Worker diff:
%s

Reviewer FileRefs:
%s

CURRENT ITERATION
Worker diff:
%s

Reviewer FileRefs:
%s`,
		truncateForPrompt(prev.Diff, 4000),
		renderFileRefs(prev.FileRefs),
		truncateForPrompt(curr.Diff, 4000),
		renderFileRefs(curr.FileRefs),
	)

	text, err := c.runAndExtractText(ctx, loopSimilarityModel, loopSimilaritySystemPrompt, userText, 30*time.Second)
	if err != nil && text == "" {
		return 0, fmt.Errorf("loop similarity subprocess: %w", err)
	}

	var resp struct {
		Similarity float64 `json:"similarity"`
		Reason     string  `json:"reason"`
	}
	if jsonErr := json.Unmarshal([]byte(stripCodeFence(text)), &resp); jsonErr != nil {
		return 0, fmt.Errorf("loop similarity parse: %w (raw=%q)", jsonErr, text)
	}
	if resp.Similarity < 0 {
		resp.Similarity = 0
	}
	if resp.Similarity > 1 {
		resp.Similarity = 1
	}
	return resp.Similarity, nil
}

// truncateForPrompt limits a text blob to maxBytes characters, adding
// a clear suffix when truncation happens so Haiku knows the input was
// clipped (and doesn't draw conclusions about empty trailing context).
func truncateForPrompt(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "\n... [truncated for prompt budget]"
}

// renderFileRefs formats a FileRef slice for the loop similarity prompt.
// One line per ref: `path:line — comment (reasoning)`.
func renderFileRefs(refs []verdict.FileRef) string {
	if len(refs) == 0 {
		return "(none — APPROVE or empty)"
	}
	var b strings.Builder
	for _, r := range refs {
		fmt.Fprintf(&b, "- %s", r.Path)
		if r.Line > 0 {
			fmt.Fprintf(&b, ":%d", r.Line)
		}
		fmt.Fprintf(&b, " — %s", r.Comment)
		if r.Reasoning != "" {
			fmt.Fprintf(&b, " (%s)", r.Reasoning)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
