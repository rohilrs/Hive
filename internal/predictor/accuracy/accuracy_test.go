package accuracy

import (
	"math"
	"testing"
)

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestComputeNoOverlap(t *testing.T) {
	s := Compute([]string{"a.go", "b.go"}, []string{"c.go", "d.go"})
	if !approxEqual(s.Precision, 0.0) || !approxEqual(s.Recall, 0.0) {
		t.Errorf("no-overlap: P=%v R=%v want 0.0/0.0", s.Precision, s.Recall)
	}
	if s.PredictedCount != 2 || s.TouchedCount != 2 || s.IntersectCount != 0 {
		t.Errorf("counts: %+v want 2/2/0", s)
	}
}

func TestComputeFullOverlap(t *testing.T) {
	s := Compute([]string{"a.go", "b.go"}, []string{"a.go", "b.go"})
	if !approxEqual(s.Precision, 1.0) || !approxEqual(s.Recall, 1.0) {
		t.Errorf("full-overlap: P=%v R=%v want 1.0/1.0", s.Precision, s.Recall)
	}
}

func TestComputePartialOverlap(t *testing.T) {
	// Predicted: a, b, c (3). Touched: b, c, d (3). Intersection: b, c (2).
	// Precision = 2/3, Recall = 2/3.
	s := Compute([]string{"a.go", "b.go", "c.go"}, []string{"b.go", "c.go", "d.go"})
	if !approxEqual(s.Precision, 2.0/3.0) || !approxEqual(s.Recall, 2.0/3.0) {
		t.Errorf("partial: P=%v R=%v want 0.667/0.667", s.Precision, s.Recall)
	}
	if s.PredictedCount != 3 || s.TouchedCount != 3 || s.IntersectCount != 2 {
		t.Errorf("counts: %+v want 3/3/2", s)
	}
}

func TestComputePredictedSubsetOfTouched(t *testing.T) {
	// Predicted: a, b (2). Touched: a, b, c, d (4). Intersection: a, b (2).
	// Precision = 2/2 = 1.0 (all predictions correct). Recall = 2/4 = 0.5 (missed half).
	s := Compute([]string{"a.go", "b.go"}, []string{"a.go", "b.go", "c.go", "d.go"})
	if !approxEqual(s.Precision, 1.0) || !approxEqual(s.Recall, 0.5) {
		t.Errorf("subset: P=%v R=%v want 1.0/0.5", s.Precision, s.Recall)
	}
}

func TestComputeDuplicatesDedupedBeforeMath(t *testing.T) {
	// Duplicates in predicted or touched should not inflate counts.
	s := Compute([]string{"a.go", "a.go", "b.go"}, []string{"a.go", "a.go"})
	if s.PredictedCount != 2 || s.TouchedCount != 1 || s.IntersectCount != 1 {
		t.Errorf("dedup: %+v want 2/1/1", s)
	}
	if !approxEqual(s.Precision, 0.5) || !approxEqual(s.Recall, 1.0) {
		t.Errorf("dedup: P=%v R=%v want 0.5/1.0", s.Precision, s.Recall)
	}
}

func TestComputeNormalizesPaths(t *testing.T) {
	// "./a.go" should compare equal to "a.go".
	s := Compute([]string{"./a.go", "b.go"}, []string{"a.go", "./b.go"})
	if !approxEqual(s.Precision, 1.0) || !approxEqual(s.Recall, 1.0) {
		t.Errorf("normalize: P=%v R=%v want 1.0/1.0", s.Precision, s.Recall)
	}
}

func TestComputeEmptyPredicted(t *testing.T) {
	// Empty predicted → precision undefined. Caller is responsible for
	// detecting this case and persisting as skipped; Compute returns
	// zero-valued Score for the float fields.
	s := Compute(nil, []string{"a.go"})
	if !approxEqual(s.Precision, 0.0) || !approxEqual(s.Recall, 0.0) {
		t.Errorf("empty-predicted: P=%v R=%v want 0.0/0.0", s.Precision, s.Recall)
	}
	if s.PredictedCount != 0 || s.TouchedCount != 1 {
		t.Errorf("empty-predicted counts: %+v", s)
	}
}

func TestComputeEmptyTouched(t *testing.T) {
	s := Compute([]string{"a.go"}, nil)
	if !approxEqual(s.Precision, 0.0) || !approxEqual(s.Recall, 0.0) {
		t.Errorf("empty-touched: P=%v R=%v want 0.0/0.0", s.Precision, s.Recall)
	}
	if s.PredictedCount != 1 || s.TouchedCount != 0 {
		t.Errorf("empty-touched counts: %+v", s)
	}
}
