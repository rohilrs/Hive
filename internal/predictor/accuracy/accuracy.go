// Package accuracy implements the per-run precision/recall computation
// for the Hive predictor. Inputs are two slices of file paths
// (predicted by Haiku vs. touched per `git diff`); output is a Score
// with precision, recall, and the underlying counts.
//
// This package is pure compute — no I/O, no logging. The daemon adapter
// in internal/daemon/accuracy_compute.go wires it to the real prediction
// JSON + git diff and handles persistence.
package accuracy

import "path/filepath"

// Score is the result of Compute. Precision = |predicted ∩ touched| /
// |predicted|; Recall = |predicted ∩ touched| / |touched|. When either
// denominator is 0, the corresponding metric is 0 (caller should
// interpret based on the count fields). All counts are de-duplicated
// (set semantics).
type Score struct {
	Precision      float64
	Recall         float64
	PredictedCount int
	TouchedCount   int
	IntersectCount int
}

// Compute returns the Score for the given predicted + touched slices.
// Paths are normalized via filepath.Clean before comparison so leading
// "./" or trailing "/" don't cause spurious mismatches. Duplicates in
// either input slice are deduplicated.
func Compute(predicted, touched []string) Score {
	pSet := toSet(predicted)
	tSet := toSet(touched)
	intersect := 0
	for f := range pSet {
		if _, ok := tSet[f]; ok {
			intersect++
		}
	}
	s := Score{
		PredictedCount: len(pSet),
		TouchedCount:   len(tSet),
		IntersectCount: intersect,
	}
	if s.PredictedCount > 0 {
		s.Precision = float64(intersect) / float64(s.PredictedCount)
	}
	if s.TouchedCount > 0 {
		s.Recall = float64(intersect) / float64(s.TouchedCount)
	}
	return s
}

// toSet normalizes paths via filepath.Clean and deduplicates.
func toSet(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		out[filepath.Clean(p)] = struct{}{}
	}
	return out
}
