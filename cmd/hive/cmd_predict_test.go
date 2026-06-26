package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rohilrs/Hive/internal/anthropic"
	"github.com/rohilrs/Hive/internal/predictor"
	"github.com/rohilrs/Hive/internal/scavenger/capsule"
)

type fakePredictor struct {
	res *predictor.Result
	err error
}

func (f *fakePredictor) Predict(_ context.Context, _ string, _ string, _ string) (*predictor.Result, error) {
	return f.res, f.err
}

func TestRunPredictHumanFormat(t *testing.T) {
	p := &fakePredictor{res: &predictor.Result{
		Files:          []string{"a.go", "b.go"},
		InlineCapsules: []capsule.Capsule{{Target: "func A()", Raw: "[TARGET]\nfunc A()", TokenEstimate: 50}},
		Overflow:       []anthropic.Candidate{{File: "b.go", Score: 0.6, Reason: "adjacent"}},
		FullBundlePath: "/tmp/prefetch.md",
		Metrics:        predictor.Metrics{CandidateCount: 2, InlineCount: 1, OverflowCount: 1, HaikuLatency: 800 * time.Millisecond},
	}}
	var out bytes.Buffer
	err := runPredict(context.Background(), &out, p, "fix bug", "/tmp/repo", "human")
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"a.go", "b.go", "func A()", "0.60", "adjacent", "/tmp/prefetch.md", "candidates: 2"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\nFULL:\n%s", want, s)
		}
	}
}

func TestRunPredictJSONFormat(t *testing.T) {
	p := &fakePredictor{res: &predictor.Result{
		Files: []string{"a.go"},
	}}
	var out bytes.Buffer
	err := runPredict(context.Background(), &out, p, "fix bug", "/tmp/repo", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if got["files"] == nil {
		t.Errorf("JSON missing 'files'; got=%+v", got)
	}
}

func TestRunPredictGracefulDegrade(t *testing.T) {
	p := &fakePredictor{res: nil, err: nil} // predictor returned nil/nil
	var out bytes.Buffer
	err := runPredict(context.Background(), &out, p, "x", "/tmp/repo", "human")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no prediction available") {
		t.Errorf("expected degrade message; got %q", out.String())
	}
}
