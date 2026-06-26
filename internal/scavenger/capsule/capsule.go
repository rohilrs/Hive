// Package capsule wraps the scavenger CLI's `capsule` subcommand as a
// typed Go interface. The wrapper shells out per request; for v1 this
// is acceptable because predictor calls are bounded (max ~10 capsules
// per dispatch). A persistent stdio MCP client may replace this in a
// later phase if subprocess overhead becomes a bottleneck.
package capsule

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Capsule is the parsed result of one `scavenger capsule` invocation.
// Section fields are empty when the corresponding [SECTION] header is
// absent from the CLI output.
type Capsule struct {
	Target        string
	Callers       string
	Callees       string
	Context       string
	Body          string
	Annotations   string
	TokenEstimate int    // length(Raw) / 4, used by predictor's bundle cap
	Raw           string // full CLI stdout, for prefetch.md spill
}

// Req describes one capsule lookup.
type Req struct {
	File    string // required
	Symbol  string // optional
	Query   string // optional — passed as --query for intent detection
	Budget  int    // optional — passed as --budget
	Cwd     string // working dir for `scavenger`; should be the repo root
}

// Config configures a CLI-backed Fetcher.
type Config struct {
	Binary         string        // path to scavenger executable
	PerCallTimeout time.Duration // bounds a single Fetch call
}

// Fetcher is the predictor-facing interface. Production uses CLIFetcher;
// tests use in-memory fakes.
type Fetcher interface {
	Fetch(ctx context.Context, req Req) (*Capsule, error)
}

// CLIFetcher implements Fetcher by shelling out to `scavenger capsule`.
type CLIFetcher struct {
	cfg Config
}

// NewCLIFetcher constructs a CLIFetcher with sensible defaults.
func NewCLIFetcher(cfg Config) *CLIFetcher {
	if cfg.Binary == "" {
		cfg.Binary = "scavenger"
	}
	if cfg.PerCallTimeout == 0 {
		cfg.PerCallTimeout = 5 * time.Second
	}
	return &CLIFetcher{cfg: cfg}
}

// Fetch invokes `scavenger capsule <file> [symbol] [--query ...] [--budget ...]`
// with the configured per-call timeout and parses the section-formatted
// stdout. An empty result ("(0 tokens, 0 items)") is returned as a zero
// Capsule with Raw populated, not as an error.
func (f *CLIFetcher) Fetch(ctx context.Context, req Req) (*Capsule, error) {
	if req.File == "" {
		return nil, fmt.Errorf("capsule.Fetch: File is required")
	}
	ctx, cancel := context.WithTimeout(ctx, f.cfg.PerCallTimeout)
	defer cancel()

	args := []string{"capsule", req.File}
	if req.Symbol != "" {
		args = append(args, req.Symbol)
	}
	if req.Query != "" {
		args = append(args, "--query", req.Query)
	}
	if req.Budget > 0 {
		args = append(args, "--budget", fmt.Sprintf("%d", req.Budget))
	}

	cmd := exec.CommandContext(ctx, f.cfg.Binary, args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("scavenger capsule %s: %w", req.File, err)
	}
	raw := string(out)
	c := parseCapsule(raw)
	return c, nil
}

// parseCapsule splits section-formatted text into a Capsule. Sections
// are introduced by lines like `[TARGET]` or `[BODY] symbol-name`; the
// content belongs to that section until the next section header.
//
// The empty-result shape `(0 tokens, 0 items)` parses to a Capsule with
// all section fields empty and Raw set to the input.
func parseCapsule(raw string) *Capsule {
	c := &Capsule{
		Raw:           raw,
		TokenEstimate: len(raw) / 4,
	}
	if strings.TrimSpace(raw) == "(0 tokens, 0 items)" {
		c.TokenEstimate = 0
		return c
	}

	var (
		curSection string
		curBuf     strings.Builder
	)
	flush := func() {
		if curSection == "" {
			return
		}
		val := strings.TrimSpace(curBuf.String())
		switch curSection {
		case "TARGET":
			c.Target = val
		case "CALLERS":
			c.Callers = val
		case "CALLEES":
			c.Callees = val
		case "CONTEXT":
			c.Context = val
		case "BODY":
			c.Body = val
		case "ANNOTATIONS":
			c.Annotations = val
		}
		curBuf.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.Contains(trim, "]") {
			// Section header — flush the previous, start fresh.
			flush()
			end := strings.Index(trim, "]")
			curSection = trim[1:end]
			continue
		}
		if curSection != "" {
			curBuf.WriteString(line)
			curBuf.WriteString("\n")
		}
	}
	flush()
	return c
}
