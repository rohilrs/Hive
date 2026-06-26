package main

import (
	"log"
	"os"
	"time"

	"github.com/rohilrs/Hive/internal/adapter/claudecode"
	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/config"
	"github.com/rohilrs/Hive/internal/llm/claudecli"
	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

// pickPredictorSDK returns the configured LLM client wrapped as the
// predictor.SDK consumer interface (PredictFiles only). Picks
// claudecli.Client when cfg.LLM.Provider == "cli" (default; uses
// Claude Max subscription auth via the `claude` binary), or
// anthropic.SDK when "api" (requires ANTHROPIC_API_KEY).
func pickPredictorSDK(cfg *config.Config) predictor.SDK {
	if cfg.LLM.Provider == "api" {
		return anthropic.NewSDK(anthropic.SDKConfig{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  cfg.Predictor.HaikuModel,
		})
	}
	return claudecli.NewClient(claudecli.Config{
		Binary: cfg.ClaudeCLI.Binary,
	})
}

// pickPredictor constructs a ready-to-use *predictor.Predictor from the
// application config. Extracted from the cobra command so both cmd_predict.go
// and cmd_daemon.go can share the same wiring without duplication. Uses
// predictor.NewPredictor so the production Predictor gets the in-memory
// LRU cache (identical re-dispatches skip Haiku + scavenger calls).
func pickPredictor(cfg *config.Config) *predictor.Predictor {
	fetcher := capsule.NewMCPFetcher(capsule.Config{
		Binary:         cfg.Scavenger.Binary,
		PerCallTimeout: time.Duration(cfg.Predictor.PerCallTimeoutSeconds) * time.Second,
	})
	return predictor.NewPredictor(
		pickPredictorSDK(cfg),
		fetcher,
		predictor.Config{
			BundleTokenCap: cfg.Predictor.BundleTokenCap,
			MaxCandidates:  cfg.Predictor.MaxCandidates,
			PerCallTimeout: time.Duration(cfg.Predictor.PerCallTimeoutSeconds) * time.Second,
			HaikuTimeout:   time.Duration(cfg.Predictor.HaikuTimeoutSeconds) * time.Second,
		},
	)
}

// pickLoopDetector returns a pipeline.LoopDetector for the active LLM
// provider. cli → claudecli.Client (satisfies the interface via
// ClassifyLoopSimilarity). api currently has no impl → returns nil
// and L3 disabled with a log line. Returns nil to mean "L3 disabled."
func pickLoopDetector(cfg *config.Config) pipeline.LoopDetector {
	switch cfg.LLM.Provider {
	case "cli", "":
		return claudecli.NewClient(claudecli.Config{Binary: cfg.ClaudeCLI.Binary})
	case "api":
		log.Printf("cmd: LLM provider=api has no ClassifyLoopSimilarity impl; L3 disabled")
		return nil
	default:
		log.Printf("cmd: unknown LLM provider %q; L3 disabled", cfg.LLM.Provider)
		return nil
	}
}

// pickClassifierSDK returns the configured LLM client wrapped as the
// claudecode.ClassifierSDK consumer interface (ClassifyVerdict only).
// Same provider switch as pickPredictorSDK.
func pickClassifierSDK(cfg *config.Config) claudecode.ClassifierSDK {
	if cfg.LLM.Provider == "api" {
		return anthropic.NewSDK(anthropic.SDKConfig{
			APIKey: os.Getenv("ANTHROPIC_API_KEY"),
			Model:  cfg.Models.Classifier,
		})
	}
	return claudecli.NewClient(claudecli.Config{
		Binary: cfg.ClaudeCLI.Binary,
	})
}
