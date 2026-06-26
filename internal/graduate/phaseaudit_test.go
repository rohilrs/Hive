package graduate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/internal/roadmap"
)

func TestAuditPhase_UnmetDeliverableBecomesFinding(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		if name != "submit_phase_audit" {
			t.Fatalf("expected submit_phase_audit, got %s", name)
		}
		return json.RawMessage(`{"criteria":[
			{"description":"Acceptance-metrics dashboard operational","met":false,"severity":"High","category":"Incomplete","evidence":"sink never wired","recommendation":"wire the sink"},
			{"description":"10 Tier-3 scenarios","met":true}
		]}`), nil
	}}
	phase := roadmap.Phase{Number: "5", Title: "E2E + metrics", Body: "deliverables..."}
	v, sum, err := AuditPhase(context.Background(), runner, "/wt", phase)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Findings) != 1 {
		t.Fatalf("want 1 finding (the unmet deliverable), got %d: %+v", len(v.Findings), v.Findings)
	}
	f := v.Findings[0]
	if f.Severity != "High" {
		t.Errorf("severity=%s want High", f.Severity)
	}
	if !strings.Contains(f.Title, "Phase 5") || !strings.Contains(f.Title, "dashboard operational") {
		t.Errorf("title=%q must name the phase + deliverable", f.Title)
	}
	if sum.Met != 1 || sum.Total != 2 || sum.Phase != "5" {
		t.Errorf("summary=%+v want Met1 Total2 Phase5", sum)
	}
}

func TestAuditPhase_DefaultSeverityHigh(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		return json.RawMessage(`{"criteria":[{"description":"x","met":false}]}`), nil
	}}
	v, _, err := AuditPhase(context.Background(), runner, "/wt", roadmap.Phase{Number: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Findings[0].Severity != "High" || v.Findings[0].Category != "Incomplete" {
		t.Errorf("unmet w/o severity must default High/Incomplete, got %s/%s", v.Findings[0].Severity, v.Findings[0].Category)
	}
}

func TestAuditPhase_WrongToolCallCountErrors(t *testing.T) {
	_, _, err := AuditPhase(context.Background(), zeroCallRunner{}, "/wt", roadmap.Phase{Number: "1"})
	if err == nil {
		t.Error("a phase audit with no tool call must error (so AuditPhases skips it)")
	}
}

func TestLoadRoadmapPhases(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"docs/superpowers/roadmaps/r.md":    "## Phase 1 — Foundation\n**Status:** Done\nbuild the base\n\n## Phase 5 — Metrics\nacceptance metrics dashboard\n",
		"docs/superpowers/roadmaps/junk.md": "no phase headings here\n",
	})
	phases := loadRoadmapPhases(dir)
	if len(phases) != 2 {
		t.Fatalf("want 2 phases from the valid roadmap (junk.md skipped), got %d", len(phases))
	}
	if phases[0].Number != "1" || phases[1].Number != "5" {
		t.Errorf("phase numbers = %q,%q want 1,5", phases[0].Number, phases[1].Number)
	}
}

func TestLoadRoadmapPhases_NoneFound(t *testing.T) {
	dir := writeFixture(t, map[string]string{"src/x.go": "package x"})
	if got := loadRoadmapPhases(dir); len(got) != 0 {
		t.Errorf("no roadmap dir → empty, got %d", len(got))
	}
}

func TestAuditPhases_SkipsErroringPhase(t *testing.T) {
	runner := toolStubRunner{fn: func(name, prompt string) (json.RawMessage, error) {
		if strings.Contains(prompt, "Phase 2") {
			return nil, fmt.Errorf("boom")
		}
		return json.RawMessage(`{"criteria":[{"description":"d","met":true}]}`), nil
	}}
	phases := []roadmap.Phase{{Number: "1", Title: "a"}, {Number: "2", Title: "b"}}
	verdicts, summaries := AuditPhases(context.Background(), runner, "/wt", phases)
	if len(verdicts) != 1 || len(summaries) != 1 {
		t.Fatalf("erroring phase must be skipped: verdicts=%d summaries=%d want 1/1", len(verdicts), len(summaries))
	}
	if summaries[0].Phase != "1" {
		t.Errorf("surviving summary phase=%s want 1", summaries[0].Phase)
	}
}
