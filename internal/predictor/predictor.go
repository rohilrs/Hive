// Package predictor orchestrates the Haiku candidate extraction and
// scavenger capsule fetch into a bundled context for the implement
// stage. Phase 2b.2 ships the orchestrator + a dry-run CLI consumer
// (cmd/hive/cmd_predict.go); dispatch integration (pre-fetch wiring
// into StageRequest) lands in 2b.3.
package predictor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

// SDK is the predictor-facing subset of internal/anthropic.SDK. Tests
// supply in-memory fakes.
type SDK interface {
	PredictFiles(ctx context.Context, req anthropic.PredictionRequest) ([]anthropic.Candidate, error)
}

// Config bounds the predictor's resource use. Mirrors config.Predictor
// fields converted to durations.
type Config struct {
	BundleTokenCap int
	MaxCandidates  int
	PerCallTimeout time.Duration
	HaikuTimeout   time.Duration
}

// Result is what Predict returns on success.
type Result struct {
	Files          []string              // for conflict guard (Phase 2b.4)
	InlineCapsules []capsule.Capsule     // top-K that fit under BundleTokenCap
	Overflow       []anthropic.Candidate // names only, surfaced as pointer list
	FullBundlePath string                // path to <bundleDir>/prefetch.md
	Metrics        Metrics
}

// Metrics is per-invocation accounting.
type Metrics struct {
	HaikuLatency   time.Duration
	FetchLatency   time.Duration
	CandidateCount int
	InlineCount    int
	OverflowCount  int
	Truncated      bool
	Error          string
}

// Predictor orchestrates Haiku + capsule fetch.
//
// The cache field is an optional in-memory LRU keyed on
// hash(task, repoRoot). When non-nil, identical re-dispatches (manual
// retry, hive_predict re-runs on the same task body) short-circuit the
// Haiku + capsule-fetch work and reuse the prior result. The bundle
// file is always rewritten for the per-call bundleDir.
//
// Production code should use NewPredictor to wire a cache; tests can
// construct &Predictor{...} directly to get the legacy (no-cache)
// behavior for back-compat.
type Predictor struct {
	SDK     SDK
	Fetcher capsule.Fetcher
	Cfg     Config

	cache *predictorCache // optional; nil = no cache
}

// NewPredictor returns a Predictor wired with an LRU cache (capacity 100).
// Use this in production composition; tests may instantiate the struct
// literal directly when they want no-cache behavior.
func NewPredictor(sdk SDK, fetcher capsule.Fetcher, cfg Config) *Predictor {
	return &Predictor{
		SDK:     sdk,
		Fetcher: fetcher,
		Cfg:     cfg,
		cache:   newPredictorCache(100),
	}
}

// Predict runs Haiku on (task, truncated repo files) to get a ranked
// candidate list, then fetches capsules for top candidates in order
// until BundleTokenCap is hit. Remaining candidates become overflow.
// Full bundle written to <bundleDir>/prefetch.md.
//
// bundleDir is where prefetch.md is written; production dispatch uses
// Run.RuntimeDir, the dry-run CLI uses a tempdir.
//
// Returns (nil, nil) on Haiku failure (graceful degrade — scheduler
// dispatches without prediction). Returns an error only on hard
// failures the caller should surface loudly (e.g., bundleDir not
// writable).
func (p *Predictor) Predict(ctx context.Context, task, repoRoot, bundleDir string) (*Result, error) {
	// Cache lookup: same (task, repoRoot) → reuse last prediction's
	// candidates + capsules. Only the prefetch.md bundle is rewritten
	// to the per-call bundleDir. Metrics report HaikuLatency=0 and
	// FetchLatency=0 on hit so accuracy compute doesn't double-count.
	key := cacheKey(task, repoRoot)
	if p.cache != nil {
		if entry, ok := p.cache.Get(key); ok {
			bundlePath := filepath.Join(bundleDir, "prefetch.md")
			// Bundle-write failure on hit (rare, transient) falls through
			// to a full predict — don't poison the cache and don't error
			// out the caller when the underlying work is still doable.
			if werr := writeBundle(bundlePath, entry.InlineCandidates, entry.InlineCapsules); werr == nil {
				return &Result{
					Files:          entry.Files,
					InlineCapsules: entry.InlineCapsules,
					Overflow:       entry.Overflow,
					FullBundlePath: bundlePath,
					Metrics: Metrics{
						HaikuLatency:   0,
						FetchLatency:   0,
						CandidateCount: entry.CandidateCount,
						InlineCount:    entry.InlineCount,
						OverflowCount:  entry.OverflowCount,
						Truncated:      entry.Truncated,
					},
				}, nil
			}
			// Fall through to full predict on write error.
		}
	}

	files, err := listRepoFiles(repoRoot, 2000) // hard cap; smarter pre-filter is 2c work
	if err != nil {
		return nil, fmt.Errorf("list repo files: %w", err)
	}

	haikuCtx, cancel := context.WithTimeout(ctx, p.Cfg.HaikuTimeout)
	defer cancel()
	haikuStart := time.Now()
	candidates, err := p.SDK.PredictFiles(haikuCtx, anthropic.PredictionRequest{
		Task:          task,
		RepoFiles:     files,
		MaxCandidates: p.Cfg.MaxCandidates,
	})
	haikuLatency := time.Since(haikuStart)
	if err != nil {
		// Graceful degrade: scheduler skips conflict guard + pre-fetch.
		return nil, nil
	}

	// Defensive cap on candidate count — Haiku may return more than
	// MaxCandidates despite the prompt instruction. Cap after fetch.
	if p.Cfg.MaxCandidates > 0 && len(candidates) > p.Cfg.MaxCandidates {
		candidates = candidates[:p.Cfg.MaxCandidates]
	}

	// Ranked descending by score (Haiku is asked to do this, but enforce).
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	// Fetch capsules in score order until cap.
	fetchStart := time.Now()
	res := &Result{
		Metrics: Metrics{
			HaikuLatency:   haikuLatency,
			CandidateCount: len(candidates),
		},
	}
	// inlineCands tracks the candidate that produced each InlineCapsule,
	// in lock-step. This is necessary because fetch failures and token-cap
	// overflows cause the inline list to diverge from the full candidates
	// slice — writeBundle needs the correct metadata for each capsule.
	var inlineCands []anthropic.Candidate
	usedTokens := 0
	for _, cand := range candidates {
		res.Files = append(res.Files, cand.File)
		c, ferr := p.Fetcher.Fetch(ctx, capsule.Req{
			File:   cand.File,
			Symbol: cand.Symbol,
			Query:  task,
			Cwd:    repoRoot,
		})
		if ferr != nil {
			// Skip this capsule; do not abort. The candidate still
			// shows up in Files for conflict-guard purposes.
			continue
		}
		if usedTokens+c.TokenEstimate > p.Cfg.BundleTokenCap {
			res.Overflow = append(res.Overflow, cand)
			res.Metrics.Truncated = true
			continue
		}
		res.InlineCapsules = append(res.InlineCapsules, *c)
		inlineCands = append(inlineCands, cand)
		usedTokens += c.TokenEstimate
	}
	res.Metrics.FetchLatency = time.Since(fetchStart)
	res.Metrics.InlineCount = len(res.InlineCapsules)
	res.Metrics.OverflowCount = len(res.Overflow)

	// Write the full bundle (all fetched capsules' Raw, plus a header
	// per capsule with the candidate metadata) to prefetch.md.
	bundlePath := filepath.Join(bundleDir, "prefetch.md")
	if err := writeBundle(bundlePath, inlineCands, res.InlineCapsules); err != nil {
		return nil, fmt.Errorf("write prefetch.md: %w", err)
	}
	res.FullBundlePath = bundlePath

	// Populate cache only on a SUCCESSFUL prediction (we got past Haiku
	// without a degrade and produced at least one candidate). Haiku-degrade
	// returns (nil,nil) before reaching here, so this branch never caches
	// a degraded result; the len(res.Files)>0 guard further skips the
	// (unusual) "Haiku returned empty list" case so a one-off empty Haiku
	// response doesn't poison the cache.
	if p.cache != nil && len(res.Files) > 0 {
		p.cache.Put(key, &cacheEntry{
			Files:            res.Files,
			InlineCapsules:   res.InlineCapsules,
			InlineCandidates: inlineCands,
			Overflow:         res.Overflow,
			HaikuLatency:     res.Metrics.HaikuLatency,
			CandidateCount:   res.Metrics.CandidateCount,
			InlineCount:      res.Metrics.InlineCount,
			OverflowCount:    res.Metrics.OverflowCount,
			Truncated:        res.Metrics.Truncated,
		})
	}
	return res, nil
}

// listRepoFiles returns up to limit files under repoRoot, sorted by
// path. Skips directories and files whose name starts with `.`, plus
// known compiled binaries at the repo root. A smarter pre-filter
// (recency, fuzzy match, broad binary detection) is deferred to Phase 2c.
func listRepoFiles(repoRoot string, limit int) ([]string, error) {
	var files []string
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable; don't abort the walk
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") && path != repoRoot {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, ".") || name == "hive" || name == "fake-claude" {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

// writeBundle renders the prefetch.md document. Each inline capsule
// gets a section with the candidate metadata header and the raw
// scavenger output. inlineCands must be in 1:1 correspondence with
// inline (same index = same candidate); callers build it in lock-step
// during the fetch loop so skipped/overflow candidates are absent.
func writeBundle(path string, inlineCands []anthropic.Candidate, inline []capsule.Capsule) error {
	var sb strings.Builder
	sb.WriteString("# Pre-fetched context (Hive predictor)\n\n")
	for i, c := range inline {
		var meta anthropic.Candidate
		if i < len(inlineCands) {
			meta = inlineCands[i]
		}
		sb.WriteString("## ")
		sb.WriteString(meta.File)
		if meta.Symbol != "" {
			sb.WriteString(":")
			sb.WriteString(meta.Symbol)
		}
		sb.WriteString(" (score=")
		sb.WriteString(fmt.Sprintf("%.2f", meta.Score))
		sb.WriteString(")\n\n")
		if meta.Reason != "" {
			sb.WriteString("_")
			sb.WriteString(meta.Reason)
			sb.WriteString("_\n\n")
		}
		sb.WriteString("```\n")
		sb.WriteString(c.Raw)
		sb.WriteString("\n```\n\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
