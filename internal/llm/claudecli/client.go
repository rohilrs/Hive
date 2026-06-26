// Package claudecli implements the Hive control-plane LLM client by
// shelling out to `claude -p`. This lets users on Claude Max (no
// ANTHROPIC_API_KEY) run the predictor and classifier-fallback paths
// using subscription auth via the claude binary.
//
// The package satisfies the existing consumer-owned interfaces
// structurally — predictor.SDK (PredictFiles) and the
// adapter/claudecode classify-fallback interface (ClassifyVerdict) —
// so consumers don't change.
package claudecli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
)

// Config configures a Client. Binary defaults to "claude" if empty.
// ExtraArgs are prepended to the subprocess argv so test fixtures
// (e.g. -fixture for fake-claude) can be injected without conflicting
// with the real claude flag set.
type Config struct {
	Binary    string
	ExtraArgs []string
	// Model, when set, is passed as `--model` to claude. Empty defers to the
	// claude CLI's configured default (the user's subscription model). The
	// OneshotToolRunner uses cfg.Model in preference to the per-turn
	// TurnInput.Model so the composition root can pin the decompose model.
	Model string
}

// Client is the claudecli LLM client. Satisfies predictor.SDK and the
// classifier-fallback consumer interface structurally.
type Client struct {
	cfg Config
}

// NewClient constructs a Client with sensible defaults.
func NewClient(cfg Config) *Client {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	return &Client{cfg: cfg}
}

// stripCodeFence removes a leading "```[language]\n" and trailing "```"
// from text if present. Haiku ignores "no markdown fences" instructions
// often enough that defensive stripping is worthwhile. Returns trimmed
// text either way.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Strip opening fence (may include a language tag like ```json)
	if idx := strings.Index(s, "\n"); idx > 0 {
		s = s[idx+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// runAndExtractText spawns `claude -p` and concatenates the text from
// every assistant content block. Returns empty string if the
// subprocess emits no assistant text.
func (c *Client) runAndExtractText(ctx context.Context, model, system, user string, timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	args := append([]string{}, c.cfg.ExtraArgs...)
	// --tools "" disables ALL tools (Read, Edit, Write, Bash, ...). This
	// is essential for the predictor + classifier: the system prompt
	// asks Haiku for a JSON-only response, but without this flag Haiku
	// can (and does!) invoke Edit/Write tools instead of returning text.
	// Because claude -p inherits the parent process's cwd (= wherever
	// the daemon was launched, usually the user's main repo), unguarded
	// tools result in Haiku silently mutating files in the user's repo
	// during dispatch. With --tools "" the only thing Haiku can do is
	// produce text — which is exactly what runAndExtractText needs.
	args = append(args,
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
		"--append-system-prompt", system,
		"--tools", "",
		// claude --print rejects an empty prompt (>= 2.1.161); the directive
		// lives in --append-system-prompt, so a minimal non-empty trigger is
		// enough when the caller passes no user text.
		"-p", nonEmptyCLIPrompt(user),
	)
	cmd := exec.CommandContext(ctx, c.cfg.Binary, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}

	var sb strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // skip non-JSON or unexpected shape
		}
		if ev.Type != "assistant" {
			continue
		}
		for _, blk := range ev.Message.Content {
			if blk.Type == "text" {
				sb.WriteString(blk.Text)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		// Wait error after we've read the stream isn't fatal — return
		// whatever text we got, plus the error for diagnostics.
		return sb.String(), fmt.Errorf("claude wait: %w", err)
	}
	return sb.String(), nil
}

const classifyModel = "claude-haiku-4-5"

const classifySystemPrompt = `You classify a code-review assistant's final output into one of:
- APPROVE — the reviewer is satisfied; the change is acceptable as-is
- CHANGES_REQUESTED — the reviewer wants modifications before accepting
- UNCLEAR — cannot tell which of the two the reviewer means

Return JSON only, no prose, no markdown fences:
{"verdict": "APPROVE" | "CHANGES_REQUESTED" | "UNCLEAR", "confidence": 0-100}`

// ClassifyVerdict satisfies the classifier-fallback consumer interface.
// Mirrors internal/anthropic.SDK.ClassifyVerdict's fail-safe semantics:
// UNCLEAR + low-confidence + unparseable all collapse to CHANGES_REQUESTED.
func (c *Client) ClassifyVerdict(ctx context.Context, reviewerText string) (*anthropic.VerdictResult, error) {
	text, err := c.runAndExtractText(ctx, classifyModel, classifySystemPrompt, reviewerText, 30*time.Second)
	if err != nil && text == "" {
		// Hard subprocess failure with no text — still fail-safe rather
		// than returning an error, because the caller (worker stage)
		// shouldn't crash on transient claude issues.
		return &anthropic.VerdictResult{Verdict: "CHANGES_REQUESTED", Confidence: 0}, nil
	}

	var v anthropic.VerdictResult
	if jsonErr := json.Unmarshal([]byte(stripCodeFence(text)), &v); jsonErr != nil {
		return &anthropic.VerdictResult{Verdict: "CHANGES_REQUESTED", Confidence: 0}, nil
	}
	if v.Verdict == "UNCLEAR" || v.Confidence < 70 {
		v.Verdict = "CHANGES_REQUESTED"
	}
	return &v, nil
}

const predictModel = "claude-haiku-4-5"

const predictSystemPrompt = `You are a code-search heuristic. Given a developer task description and a list of files in the repository, return a ranked list of files most likely to need editing or reading to complete the task.

Return ONLY raw JSON with this exact shape, no prose, no markdown fences:
{"candidates":[{"file":"<relative path>","symbol":"<optional name>","score":<0.0-1.0>,"reason":"<one sentence>"}]}

Each candidate's file must match an entry in the provided RepoFiles list. Rank by descending score. Return at most MaxCandidates entries.`

// PredictFiles satisfies predictor.SDK. Returns an empty slice (not
// error) on unparseable output, mirroring internal/anthropic.SDK.PredictFiles
// fail-safe semantics so the predictor gracefully degrades to "no
// prediction available."
func (c *Client) PredictFiles(ctx context.Context, req anthropic.PredictionRequest) ([]anthropic.Candidate, error) {
	userText := fmt.Sprintf("Task: %s\n\nMaxCandidates: %d\n\nRepoFiles:\n- %s",
		req.Task,
		req.MaxCandidates,
		strings.Join(req.RepoFiles, "\n- "),
	)

	text, err := c.runAndExtractText(ctx, predictModel, predictSystemPrompt, userText, 30*time.Second)
	if err != nil && text == "" {
		return []anthropic.Candidate{}, nil
	}

	var resp struct {
		Candidates []anthropic.Candidate `json:"candidates"`
	}
	if jsonErr := json.Unmarshal([]byte(stripCodeFence(text)), &resp); jsonErr != nil {
		return []anthropic.Candidate{}, nil
	}
	return resp.Candidates, nil
}

// nonEmptyCLIPrompt guards the `claude --print` positional prompt, which the
// CLI rejects when empty (>= 2.1.161). The real instruction is in the system
// prompt; this is a minimal non-empty fallback.
func nonEmptyCLIPrompt(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Respond per the system prompt."
	}
	return s
}
