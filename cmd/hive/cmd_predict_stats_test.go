package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/store"
)

func TestComputeStatsBasic(t *testing.T) {
	rows := []*store.PredictorMetric{
		{HaikuLatencyMS: 100, FetchLatencyMS: 10, CandidateCount: 5, InlineCount: 3, OverflowCount: 2},
		{HaikuLatencyMS: 200, FetchLatencyMS: 20, CandidateCount: 7, InlineCount: 4, OverflowCount: 3, Truncated: true},
		{HaikuLatencyMS: 300, FetchLatencyMS: 30, CandidateCount: 9, InlineCount: 5, OverflowCount: 4, Error: "haiku timeout"},
	}
	s := computeStats(rows, nil)
	if s.Count != 3 {
		t.Errorf("Count=%d want 3", s.Count)
	}
	if s.HaikuLatencyP50MS != 200 {
		t.Errorf("HaikuLatencyP50MS=%d want 200", s.HaikuLatencyP50MS)
	}
	// p95 of [100, 200, 300] should round-trip to 300 (nearest-rank).
	if s.HaikuLatencyP95MS != 300 {
		t.Errorf("HaikuLatencyP95MS=%d want 300", s.HaikuLatencyP95MS)
	}
	if s.MeanCandidates != 7.0 {
		t.Errorf("MeanCandidates=%v want 7.0", s.MeanCandidates)
	}
	if s.TruncationRate != 1.0/3.0 {
		t.Errorf("TruncationRate=%v want 0.333...", s.TruncationRate)
	}
	if s.ErrorRate != 1.0/3.0 {
		t.Errorf("ErrorRate=%v want 0.333...", s.ErrorRate)
	}
}

func TestComputeStatsEmpty(t *testing.T) {
	s := computeStats(nil, nil)
	if s.Count != 0 {
		t.Errorf("Count=%d want 0", s.Count)
	}
	// All other fields should be zero-valued.
}

func TestStatsHumanFormat(t *testing.T) {
	s := stats{
		Count:             5,
		HaikuLatencyP50MS: 1200,
		HaikuLatencyP95MS: 2400,
		MeanCandidates:    6.4,
		TruncationRate:    0.2,
		ErrorRate:         0.0,
	}
	var buf bytes.Buffer
	writeStatsHuman(&buf, s)
	out := buf.String()
	for _, want := range []string{"count: 5", "p50: 1200ms", "p95: 2400ms", "mean candidates: 6.40", "truncation rate: 20.0%", "error rate: 0.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q: %s", want, out)
		}
	}
}

func TestStatsJSONFormat(t *testing.T) {
	s := stats{Count: 3, HaikuLatencyP50MS: 150}
	var buf bytes.Buffer
	if err := writeStatsJSON(&buf, s); err != nil {
		t.Fatal(err)
	}
	var got stats
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Count != 3 || got.HaikuLatencyP50MS != 150 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestRunPredictStatsEndToEnd(t *testing.T) {
	// Open a real store, insert metric rows, run runPredictStats, verify output.
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = s.InsertPredictorMetrics(ctx, &store.PredictorMetric{
			RunID:          "r" + string(rune('A'+i)),
			ProjectID:      "p",
			HaikuLatencyMS: int64((i + 1) * 100),
			CandidateCount: i + 1,
		})
	}
	var buf bytes.Buffer
	hadRows, err := runPredictStats(ctx, &buf, s, "", time.Time{}, "json")
	if err != nil {
		t.Fatal(err)
	}
	if !hadRows {
		t.Errorf("hadRows=false, want true (3 metric rows seeded)")
	}
	var got stats
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if got.Count != 3 {
		t.Errorf("Count=%d want 3", got.Count)
	}
	if got.HaikuLatencyP50MS != 200 {
		t.Errorf("HaikuLatencyP50MS=%d want 200", got.HaikuLatencyP50MS)
	}
}

func TestRunPredictStatsReturnsFalseHadRowsOnEmpty(t *testing.T) {
	// Empty store → hadRows=false. --strict callers exit 2 on this.
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var buf bytes.Buffer
	hadRows, err := runPredictStats(context.Background(), &buf, s, "", time.Time{}, "human")
	if err != nil {
		t.Fatalf("runPredictStats: %v", err)
	}
	if hadRows {
		t.Errorf("hadRows=true, want false (empty store)")
	}
}

func TestComputeStatsIncludesAccuracyWhenPresent(t *testing.T) {
	// Three runs with metrics + accuracy: two computed, one skipped.
	metrics := []*store.PredictorMetric{
		{RunID: "r1", HaikuLatencyMS: 100, CandidateCount: 3},
		{RunID: "r2", HaikuLatencyMS: 200, CandidateCount: 4},
		{RunID: "r3", HaikuLatencyMS: 300, CandidateCount: 5},
	}
	p1, p2 := 0.5, 0.75
	r1, r2 := 0.4, 0.6
	accuracy := []*store.PredictorAccuracy{
		{RunID: "r1", Precision: &p1, Recall: &r1, PredictedCount: 2, TouchedCount: 5, IntersectCount: 1},
		{RunID: "r2", Precision: &p2, Recall: &r2, PredictedCount: 4, TouchedCount: 5, IntersectCount: 3},
		{RunID: "r3", SkippedReason: "no_edits", PredictedCount: 5},
	}

	s := computeStats(metrics, accuracy)
	if s.Count != 3 {
		t.Errorf("Count=%d want 3", s.Count)
	}
	if s.AccuracyCoverage < 0.65 || s.AccuracyCoverage > 0.68 {
		t.Errorf("AccuracyCoverage=%v want ~0.667 (2/3 computed)", s.AccuracyCoverage)
	}
	// nearest-rank with p=0.50, n=2: rank = int(0.5*2 + 0.5) = 1, sorted[0] = 0.5.
	if s.PrecisionP50 < 0.49 || s.PrecisionP50 > 0.51 {
		t.Errorf("PrecisionP50=%v want ~0.5", s.PrecisionP50)
	}
	if s.RecallP50 < 0.39 || s.RecallP50 > 0.41 {
		t.Errorf("RecallP50=%v want ~0.4", s.RecallP50)
	}
}

func TestComputeStatsAccuracyAllSkipped(t *testing.T) {
	metrics := []*store.PredictorMetric{{RunID: "r1", HaikuLatencyMS: 100, CandidateCount: 1}}
	accuracy := []*store.PredictorAccuracy{{RunID: "r1", SkippedReason: "no_edits", PredictedCount: 1}}
	s := computeStats(metrics, accuracy)
	if s.AccuracyCoverage != 0 {
		t.Errorf("AccuracyCoverage=%v want 0 (all skipped)", s.AccuracyCoverage)
	}
	if s.PrecisionP50 != 0 || s.RecallP50 != 0 {
		t.Errorf("when no computed accuracy, P/R should be 0, got %v/%v", s.PrecisionP50, s.RecallP50)
	}
}

func TestStatsHumanFormatIncludesAccuracy(t *testing.T) {
	s := stats{
		Count:            5,
		PrecisionP50:     0.65,
		PrecisionP95:     0.92,
		RecallP50:        0.5,
		RecallP95:        0.85,
		AccuracyCoverage: 0.8,
	}
	var buf bytes.Buffer
	writeStatsHuman(&buf, s)
	out := buf.String()
	for _, want := range []string{"precision p50: 0.650", "precision p95: 0.920", "recall p50: 0.500", "recall p95: 0.850", "accuracy coverage: 80.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestParseSinceDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"30m", 30 * time.Minute},
	}
	for _, c := range cases {
		got, err := parseSinceDuration(c.in)
		if err != nil {
			t.Errorf("parseSinceDuration(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSinceDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	if _, err := parseSinceDuration("bogus"); err == nil {
		t.Error("expected error on bogus input")
	}
	if _, err := parseSinceDuration("xd"); err == nil {
		t.Error("expected error on non-numeric days")
	}
}
