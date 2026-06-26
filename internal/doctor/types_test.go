package doctor

import "testing"

func TestSummaryAggregation(t *testing.T) {
	r := Report{
		Checks: []Check{
			{Status: StatusOK},
			{Status: StatusOK},
			{Status: StatusWarn},
			{Status: StatusError},
			{Status: StatusError},
			{Status: StatusSkip},
		},
	}
	r.Summary = r.computeSummary()
	if r.Summary.OK != 2 {
		t.Errorf("OK=%d, want 2", r.Summary.OK)
	}
	if r.Summary.Warnings != 1 {
		t.Errorf("Warnings=%d, want 1", r.Summary.Warnings)
	}
	if r.Summary.Errors != 2 {
		t.Errorf("Errors=%d, want 2", r.Summary.Errors)
	}
	if r.Summary.Skipped != 1 {
		t.Errorf("Skipped=%d, want 1", r.Summary.Skipped)
	}
}
