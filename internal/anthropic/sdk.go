// Package anthropic wraps the Anthropic SDK. Shared by adapters that talk
// to the Anthropic API (currently: claudecode for Haiku verdict fallback;
// future: anthropicsdk adapter for native stage execution; chat agent in
// Phase 6).
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// SDKConfig configures the shared Anthropic SDK wrapper.
//
// BaseURL is optional; when empty the SDK uses the official Anthropic API
// endpoint. It exists primarily so tests can point the client at an
// httptest.Server.
type SDKConfig struct {
	APIKey  string
	BaseURL string
	Model   string
}

// SDK is the shared, reusable Anthropic SDK wrapper. It currently exposes
// a single control-plane operation (ClassifyVerdict); future operations
// for native stage execution and the chat agent will live here too.
type SDK struct {
	cfg    SDKConfig
	client anth.Client
}

// NewSDK constructs an SDK with the given config. The underlying
// anth.Client is a value type, not a pointer, so we hold it by value.
func NewSDK(cfg SDKConfig) *SDK {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &SDK{cfg: cfg, client: anth.NewClient(opts...)}
}

// VerdictResult is the parsed JSON output from the Haiku classifier.
type VerdictResult struct {
	Verdict    string `json:"verdict"`
	Confidence int    `json:"confidence"`
}

const verdictSystemPrompt = `You classify a code-review assistant's final output into one of:
- APPROVE — the reviewer is satisfied; the change is acceptable as-is
- CHANGES_REQUESTED — the reviewer wants modifications before accepting
- UNCLEAR — cannot tell which of the two the reviewer means

Return JSON only, no prose, no markdown fences:
{"verdict": "APPROVE" | "CHANGES_REQUESTED" | "UNCLEAR", "confidence": 0-100}`

// ClassifyVerdict asks Haiku to classify reviewer text. Per spec §5.6 this
// is a fail-safe classifier: UNCLEAR verdicts, low-confidence (<70)
// verdicts, and unparseable model output all collapse to
// CHANGES_REQUESTED so the orchestrator never auto-approves on
// ambiguous signal.
func (s *SDK) ClassifyVerdict(ctx context.Context, reviewerText string) (*VerdictResult, error) {
	msg, err := s.client.Messages.New(ctx, anth.MessageNewParams{
		Model:     anth.Model(s.cfg.Model),
		MaxTokens: 200,
		System: []anth.TextBlockParam{
			{Text: verdictSystemPrompt},
		},
		Messages: []anth.MessageParam{
			anth.NewUserMessage(anth.NewTextBlock(reviewerText)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic messages.new: %w", err)
	}

	text := joinTextBlocks(msg)
	var v VerdictResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &v); err != nil {
		// Fail-safe: unparseable output is treated as CHANGES_REQUESTED.
		return &VerdictResult{Verdict: "CHANGES_REQUESTED", Confidence: 0}, nil
	}
	if v.Verdict == "UNCLEAR" || v.Confidence < 70 {
		v.Verdict = "CHANGES_REQUESTED"
	}
	return &v, nil
}

// joinTextBlocks concatenates all text-type content blocks from a Message
// response. The SDK exposes Message.Content as []ContentBlockUnion where
// each union has a Type discriminator and a flat Text field on the text
// variant.
func joinTextBlocks(msg *anth.Message) string {
	var sb strings.Builder
	for _, blk := range msg.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}
	return sb.String()
}

// ToolDef is a provider-neutral tool definition the chat agent passes to
// RunTurn. RunTurn converts it into the SDK's tool union param.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// ToolCall is a single tool_use block requested by the assistant in a turn.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// TurnInput is the input to one tool-use-capable Messages turn. The caller
// owns and maintains the running conversation in Messages; RunTurn does not
// mutate it.
type TurnInput struct {
	Model     string              // overrides s.cfg.Model when set
	System    string              // cached prefix (tool defs + state snapshot)
	Messages  []anth.MessageParam // running conversation; caller maintains
	Tools     []ToolDef
	MaxTokens int64
}

// TurnOutput is the result of one Messages turn.
type TurnOutput struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string
	TokensIn   int64
	TokensOut  int64
	Assistant  anth.MessageParam // the assistant turn, to append before tool_results
}

const defaultTurnMaxTokens = 2048

// RunTurn runs a single tool-use-capable Messages turn. Given the running
// conversation and tool definitions, it calls the Messages API once and
// returns the assistant's text, any requested tool calls, the stop reason,
// token usage, and the assistant MessageParam the caller must append before
// sending tool_result blocks back in the next turn.
//
// The System prefix is sent as a prompt-cached (ephemeral) system block so
// repeated turns with a stable tool-def + state-snapshot prefix hit the
// prompt cache.
func (s *SDK) RunTurn(ctx context.Context, in TurnInput) (*TurnOutput, error) {
	model := in.Model
	if model == "" {
		model = s.cfg.Model
	}
	maxTokens := in.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultTurnMaxTokens
	}

	params := anth.MessageNewParams{
		Model:     anth.Model(model),
		MaxTokens: maxTokens,
		Messages:  in.Messages,
	}

	if in.System != "" {
		// Prompt-cache the system prefix: a non-zero CacheControl on the
		// block marks an ephemeral cache breakpoint.
		params.System = []anth.TextBlockParam{
			{
				Text:         in.System,
				CacheControl: anth.NewCacheControlEphemeralParam(),
			},
		}
	}

	if len(in.Tools) > 0 {
		tools := make([]anth.ToolUnionParam, 0, len(in.Tools))
		for _, td := range in.Tools {
			schema := anth.ToolInputSchemaParam{}
			if props, ok := td.InputSchema["properties"].(map[string]any); ok {
				schema.Properties = props
			}
			// A schema built in Go may use []string; one built by
			// JSON-unmarshaling (the canonical shape) yields []any. Handle
			// both, or the required fields are silently dropped and the
			// model sees every field as optional.
			switch req := td.InputSchema["required"].(type) {
			case []string:
				schema.Required = req
			case []any:
				ss := make([]string, 0, len(req))
				for _, v := range req {
					if s, ok := v.(string); ok {
						ss = append(ss, s)
					}
				}
				schema.Required = ss
			}
			tp := anth.ToolParam{
				Name:        td.Name,
				InputSchema: schema,
			}
			if td.Description != "" {
				tp.Description = anth.String(td.Description)
			}
			tools = append(tools, anth.ToolUnionParam{OfTool: &tp})
		}
		params.Tools = tools
	}

	msg, err := s.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages.new: %w", err)
	}

	out := &TurnOutput{
		StopReason: string(msg.StopReason),
		TokensIn:   msg.Usage.InputTokens,
		TokensOut:  msg.Usage.OutputTokens,
		Assistant:  msg.ToParam(),
	}

	var sb strings.Builder
	for _, blk := range msg.Content {
		switch blk.Type {
		case "text":
			sb.WriteString(blk.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:    blk.ID,
				Name:  blk.Name,
				Input: blk.Input,
			})
		}
	}
	out.Text = sb.String()

	return out, nil
}

// Candidate is one Haiku-ranked likely-relevant file/symbol pair.
type Candidate struct {
	File   string  `json:"file"`
	Symbol string  `json:"symbol,omitempty"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// PredictionRequest is the input to PredictFiles. RepoFiles should be
// truncated to a reasonable size by the caller (the SDK passes them as
// system-prompt context; very large lists waste tokens).
type PredictionRequest struct {
	Task          string
	RepoFiles     []string
	MaxCandidates int
}

const predictSystemPrompt = `You are a code-search heuristic. Given a developer task description and a list of files in the repository, return a ranked list of files most likely to need editing or reading to complete the task.

Return ONLY a tool call to submit_candidates. Each candidate must include:
- file: relative path from repo root, matching one of the provided RepoFiles entries
- symbol: optional specific function/type name within the file (omit if file-level)
- score: float 0.0-1.0, higher = more likely relevant
- reason: one-sentence explanation

Rank by descending score. Return at most MaxCandidates entries.`

// PredictFiles asks Haiku to rank likely-relevant files+symbols for the
// task. Returns an empty slice (not error) on unparseable output, so
// the predictor degrades to "no prediction available" gracefully.
func (s *SDK) PredictFiles(ctx context.Context, req PredictionRequest) ([]Candidate, error) {
	userText := fmt.Sprintf("Task: %s\n\nMaxCandidates: %d\n\nRepoFiles:\n- %s",
		req.Task,
		req.MaxCandidates,
		strings.Join(req.RepoFiles, "\n- "),
	)

	toolInputSchema := anth.ToolInputSchemaParam{
		Properties: map[string]any{
			"candidates": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file":   map[string]any{"type": "string"},
						"symbol": map[string]any{"type": "string"},
						"score":  map[string]any{"type": "number"},
						"reason": map[string]any{"type": "string"},
					},
					"required": []string{"file", "score", "reason"},
				},
			},
		},
		Required: []string{"candidates"},
	}

	msg, err := s.client.Messages.New(ctx, anth.MessageNewParams{
		Model:     anth.Model(s.cfg.Model),
		MaxTokens: 1024,
		System: []anth.TextBlockParam{
			{Text: predictSystemPrompt},
		},
		Messages: []anth.MessageParam{
			anth.NewUserMessage(anth.NewTextBlock(userText)),
		},
		Tools: []anth.ToolUnionParam{
			{
				OfTool: &anth.ToolParam{
					Name:        "submit_candidates",
					Description: anth.String("Submit the ranked candidate list."),
					InputSchema: toolInputSchema,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic messages.new: %w", err)
	}

	for _, blk := range msg.Content {
		if blk.Type != "tool_use" {
			continue
		}
		// ContentBlockUnion.Input is json.RawMessage — unmarshal directly.
		var inp struct {
			Candidates []Candidate `json:"candidates"`
		}
		if err := json.Unmarshal(blk.Input, &inp); err != nil {
			return []Candidate{}, nil // fail-safe
		}
		return inp.Candidates, nil
	}
	return []Candidate{}, nil // no tool_use block -> fail-safe
}
