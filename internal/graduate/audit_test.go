package graduate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rohilrs/Hive/internal/anthropic"
)

func boolPtr(b bool) *bool { return &b }

func TestBlockingFindingsGate(t *testing.T) {
	cases := []struct {
		name string
		v    GraduationVerdict
		want bool // true = blocks (confirmed Critical or High)
	}{
		{"complete", GraduationVerdict{Status: "COMPLETE"}, false},
		{"minor only", GraduationVerdict{Status: "GAPS_FOUND", Findings: []Finding{{Severity: "Low"}, {Severity: "Medium"}}}, false},
		{"one high confirmed", GraduationVerdict{Status: "GAPS_FOUND", Findings: []Finding{{Severity: "High", Confirmed: boolPtr(true)}}}, true},
		{"one critical confirmed", GraduationVerdict{Status: "GAPS_FOUND", Findings: []Finding{{Severity: "Critical", Confirmed: boolPtr(true)}}}, true},
		{"one high unverified (nil)", GraduationVerdict{Status: "GAPS_FOUND", Findings: []Finding{{Severity: "High"}}}, false},
	}
	for _, c := range cases {
		if got := c.v.Blocks(); got != c.want {
			t.Errorf("%s: Blocks()=%v want %v", c.name, got, c.want)
		}
	}
}

type stubRunner struct{ verdict GraduationVerdict }

func (s stubRunner) RunRoamingTool(ctx context.Context, cwd, system, userPrompt string, tool anthropic.ToolDef, allowExtra []string, maxTurns int) (*anthropic.TurnOutput, error) {
	raw, _ := json.Marshal(s.verdict)
	return &anthropic.TurnOutput{StopReason: "tool_use", ToolCalls: []anthropic.ToolCall{{Name: tool.Name, Input: raw}}}, nil
}

func TestAuditParsesVerdict(t *testing.T) {
	confirmed := true
	want := GraduationVerdict{Status: "GAPS_FOUND", Summary: "x", Findings: []Finding{{Severity: "High", Title: "missing X", Confirmed: &confirmed}}}
	got, err := Audit(context.Background(), stubRunner{want}, t.TempDir(), "tgt", "feat", []string{"a.ts"}, "all gates passed")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "GAPS_FOUND" || !got.Blocks() {
		t.Errorf("got %+v", got)
	}
}

func TestBlocksConfirmedOnly(t *testing.T) {
	cases := []struct {
		name string
		v    GraduationVerdict
		want bool
	}{
		{"confirmed high blocks", GraduationVerdict{Findings: []Finding{
			{Severity: "High", Confirmed: boolPtr(true)}}}, true},
		{"confirmed critical blocks", GraduationVerdict{Findings: []Finding{
			{Severity: "Critical", Confirmed: boolPtr(true)}}}, true},
		{"refuted high does not block", GraduationVerdict{Findings: []Finding{
			{Severity: "High", Confirmed: boolPtr(false)}}}, false},
		{"unverified high (nil) does not block", GraduationVerdict{Findings: []Finding{
			{Severity: "High", Confirmed: nil}}}, false},
		{"medium never blocks even if confirmed", GraduationVerdict{Findings: []Finding{
			{Severity: "Medium", Confirmed: boolPtr(true)}}}, false},
		{"no findings", GraduationVerdict{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.v.Blocks(); got != c.want {
				t.Errorf("Blocks()=%v want %v", got, c.want)
			}
		})
	}
}

func TestConfirmLabel(t *testing.T) {
	if got := (Finding{Severity: "High", Confirmed: boolPtr(true)}).ConfirmLabel(); got != "confirmed" {
		t.Errorf("confirmed=%q", got)
	}
	if got := (Finding{Severity: "High", Confirmed: boolPtr(false)}).ConfirmLabel(); got != "refuted" {
		t.Errorf("refuted=%q", got)
	}
	if got := (Finding{Severity: "Medium"}).ConfirmLabel(); got != "unverified" {
		t.Errorf("unverified=%q", got)
	}
}

func TestFindingFingerprintStable(t *testing.T) {
	a := Finding{Category: "Missing", Title: "Emit phase-5 metrics", Evidence: "internal/x.go:42"}
	// Line number differs, title whitespace/case differs → same fingerprint.
	b := Finding{Category: "missing", Title: "emit   phase-5   metrics", Evidence: "internal/x.go:99"}
	if FindingFingerprint(a) != FindingFingerprint(b) {
		t.Errorf("fingerprints differ: %s vs %s", FindingFingerprint(a), FindingFingerprint(b))
	}
	// Different category → different fingerprint.
	c := Finding{Category: "Incorrect", Title: "Emit phase-5 metrics", Evidence: "internal/x.go:42"}
	if FindingFingerprint(a) == FindingFingerprint(c) {
		t.Error("category change should change fingerprint")
	}
	// Different evidence file → different fingerprint.
	d := Finding{Category: "Missing", Title: "Emit phase-5 metrics", Evidence: "internal/y.go:42"}
	if FindingFingerprint(a) == FindingFingerprint(d) {
		t.Error("evidence file change should change fingerprint")
	}
}

func TestEvidenceFile(t *testing.T) {
	cases := map[string]string{
		"internal/x.go:42":    "internal/x.go",
		"internal/x.go:42:10": "internal/x.go:42", // strips last :int only
		"internal/x.go":       "internal/x.go",
		"  spec: roadmap.md ": "spec: roadmap.md", // no trailing :int → trimmed whole
	}
	for in, want := range cases {
		if got := evidenceFile(in); got != want {
			t.Errorf("evidenceFile(%q)=%q want %q", in, got, want)
		}
	}
}

// toolStubRunner dispatches on the requested tool name so one stub can serve
// both audit and verify calls. The func returns the JSON the tool was "called"
// with.
type toolStubRunner struct {
	fn func(toolName string, userPrompt string) (json.RawMessage, error)
}

func (r toolStubRunner) RunRoamingTool(_ context.Context, _, _, userPrompt string, tool anthropic.ToolDef, _ []string, _ int) (*anthropic.TurnOutput, error) {
	raw, err := r.fn(tool.Name, userPrompt)
	if err != nil {
		return nil, err
	}
	return &anthropic.TurnOutput{
		StopReason: "tool_use",
		ToolCalls:  []anthropic.ToolCall{{ID: "stub", Name: tool.Name, Input: raw}},
	}, nil
}

func TestVerifyFindingConfirm(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		return json.RawMessage(`{"confirmed":true,"reason":"function absent at cited path"}`), nil
	}}
	confirmed, reason, err := VerifyFinding(context.Background(), runner, "/wt", "main", "feat",
		Finding{Severity: "High", Title: "x"})
	if err != nil || !confirmed || reason == "" {
		t.Fatalf("confirmed=%v reason=%q err=%v", confirmed, reason, err)
	}
}

func TestVerifyFindingRefute(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		return json.RawMessage(`{"confirmed":false,"reason":"already implemented"}`), nil
	}}
	confirmed, _, err := VerifyFinding(context.Background(), runner, "/wt", "main", "feat",
		Finding{Severity: "Critical", Title: "x"})
	if err != nil || confirmed {
		t.Fatalf("confirmed=%v err=%v want false,nil", confirmed, err)
	}
}

func TestVerifyFindingErrorIsNotConfirmed(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		return nil, fmt.Errorf("claude timed out")
	}}
	confirmed, _, err := VerifyFinding(context.Background(), runner, "/wt", "main", "feat",
		Finding{Severity: "High", Title: "x"})
	if confirmed {
		t.Error("error must yield confirmed=false")
	}
	if err == nil {
		t.Error("error must be surfaced to caller for logging")
	}
}

// zeroCallRunner returns a successful turn with NO tool call, exercising the
// count != 1 branch of VerifyFinding (the model returned no verdict).
type zeroCallRunner struct{}

func (zeroCallRunner) RunRoamingTool(_ context.Context, _, _, _ string, _ anthropic.ToolDef, _ []string, _ int) (*anthropic.TurnOutput, error) {
	return &anthropic.TurnOutput{StopReason: "end_turn", ToolCalls: nil}, nil
}

func TestVerifyFindingNoToolCallIsNotConfirmed(t *testing.T) {
	confirmed, _, err := VerifyFinding(context.Background(), zeroCallRunner{}, "/wt", "main", "feat",
		Finding{Severity: "High", Title: "x"})
	if confirmed {
		t.Error("a malformed verify (no tool call) must yield confirmed=false")
	}
	if err == nil {
		t.Error("count != 1 must surface an error to the caller")
	}
}

func TestUnionFindings(t *testing.T) {
	mk := func(title string) Finding {
		return Finding{Severity: "High", Category: "Missing", Title: title, Evidence: "a.go:1"}
	}
	v1 := &GraduationVerdict{Findings: []Finding{mk("alpha"), mk("beta")}}
	v2 := &GraduationVerdict{Findings: []Finding{mk("alpha"), mk("gamma")}} // alpha repeats
	v3 := &GraduationVerdict{Findings: []Finding{mk("alpha")}}              // alpha thrice
	got := unionFindings([]*GraduationVerdict{v1, v2, v3})
	if len(got) != 3 {
		t.Fatalf("want 3 unique findings, got %d", len(got))
	}
	// Order is first-seen: alpha, beta, gamma.
	if got[0].Title != "alpha" || got[1].Title != "beta" || got[2].Title != "gamma" {
		t.Errorf("order: %q %q %q", got[0].Title, got[1].Title, got[2].Title)
	}
	if got[0].SeenCount != 3 {
		t.Errorf("alpha SeenCount=%d want 3", got[0].SeenCount)
	}
	if got[1].SeenCount != 1 {
		t.Errorf("beta SeenCount=%d want 1", got[1].SeenCount)
	}
}

func TestAuditEnsembleUnionAndVerify(t *testing.T) {
	// 3 audit runs surface a partly-overlapping set; one High is real, one is phantom.
	// auditN is accessed from concurrent goroutines (runBounded fans out k audits in
	// parallel), so it must be updated atomically.
	var auditN atomic.Int32
	runner := toolStubRunner{fn: func(name, prompt string) (json.RawMessage, error) {
		if name == "submit_graduation_verdict" {
			n := auditN.Add(1)
			switch n {
			case 1:
				return json.RawMessage(`{"status":"GAPS_FOUND","summary":"s","findings":[
					{"severity":"High","category":"Missing","title":"real gap","evidence":"a.go:1"},
					{"severity":"High","category":"Missing","title":"phantom gap","evidence":"b.go:1"}]}`), nil
			case 2:
				return json.RawMessage(`{"status":"GAPS_FOUND","summary":"s","findings":[
					{"severity":"High","category":"Missing","title":"real gap","evidence":"a.go:9"}]}`), nil
			default:
				return json.RawMessage(`{"status":"COMPLETE","summary":"s","findings":[]}`), nil
			}
		}
		// verify: confirm "real gap", refute "phantom gap" (prompt carries the title).
		if strings.Contains(prompt, "real gap") {
			return json.RawMessage(`{"confirmed":true,"reason":"absent"}`), nil
		}
		return json.RawMessage(`{"confirmed":false,"reason":"present"}`), nil
	}}

	v, err := AuditEnsemble(context.Background(), runner, "/wt", "main", "feat", []string{"a.go"}, "gates passed", EnsembleOptions{K: 3, SeamAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Findings) != 2 {
		t.Fatalf("want 2 unique findings, got %d", len(v.Findings))
	}
	var real, phantom *Finding
	for i := range v.Findings {
		switch v.Findings[i].Title {
		case "real gap":
			real = &v.Findings[i]
		case "phantom gap":
			phantom = &v.Findings[i]
		}
	}
	if real == nil || real.SeenCount != 2 || real.Confirmed == nil || !*real.Confirmed {
		t.Errorf("real gap = %+v", real)
	}
	if phantom == nil || phantom.Confirmed == nil || *phantom.Confirmed {
		t.Errorf("phantom gap = %+v", phantom)
	}
	if !v.Blocks() {
		t.Error("a confirmed High must block")
	}
	if v.Status != "GAPS_FOUND" {
		t.Errorf("status=%q want GAPS_FOUND", v.Status)
	}
}

func TestAuditEnsembleAllPhantomDoesNotBlock(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		if name == "submit_graduation_verdict" {
			return json.RawMessage(`{"status":"GAPS_FOUND","summary":"s","findings":[
				{"severity":"High","category":"Missing","title":"ghost","evidence":"a.go:1"}]}`), nil
		}
		return json.RawMessage(`{"confirmed":false,"reason":"already done"}`), nil
	}}
	v, err := AuditEnsemble(context.Background(), runner, "/wt", "main", "feat", nil, "ok", EnsembleOptions{K: 3, SeamAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Blocks() {
		t.Error("all-refuted ensemble must not block")
	}
	if v.Status != "COMPLETE" {
		t.Errorf("status=%q want COMPLETE", v.Status)
	}
}

func TestAuditEnsembleAllAuditsFail(t *testing.T) {
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		return nil, fmt.Errorf("claude crashed")
	}}
	_, err := AuditEnsemble(context.Background(), runner, "/wt", "main", "feat", nil, "ok", EnsembleOptions{K: 3, SeamAudit: true})
	if err == nil {
		t.Error("all audits failing must return an error")
	}
}

func TestAuditEnsembleInjectsSeamFindings(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"client.ts": "this.post('/unwired', body);",
	})
	runner := toolStubRunner{fn: func(name, prompt string) (json.RawMessage, error) {
		if name == "submit_graduation_verdict" {
			return json.RawMessage(`{"status":"COMPLETE","summary":"s","findings":[]}`), nil
		}
		return json.RawMessage(`{"confirmed":true,"reason":"no route registered"}`), nil
	}}
	v, err := AuditEnsemble(context.Background(), runner, dir, "main", "feat", nil, "ok", EnsembleOptions{K: 2, SeamAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range v.Findings {
		if strings.Contains(f.Title, "/unwired") {
			found = true
		}
	}
	if !found {
		t.Fatalf("seam finding for /unwired must be in the verdict; got %+v", v.Findings)
	}
	if !v.Blocks() {
		t.Error("a confirmed unwired-seam High must block")
	}
}

func TestAuditEnsembleSeamDisabled(t *testing.T) {
	dir := writeFixture(t, map[string]string{"client.ts": "this.post('/unwired', body);"})
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		if name == "submit_graduation_verdict" {
			return json.RawMessage(`{"status":"COMPLETE","summary":"s","findings":[]}`), nil
		}
		return json.RawMessage(`{"confirmed":true,"reason":"x"}`), nil
	}}
	v, err := AuditEnsemble(context.Background(), runner, dir, "main", "feat", nil, "ok", EnsembleOptions{K: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Findings {
		if strings.Contains(f.Title, "/unwired") {
			t.Error("seam_audit=false must not inject seam findings")
		}
	}
}

func TestPromptsIncludeSeverityRubric(t *testing.T) {
	// The rubric must be injected into BOTH prompts — it is the calibration lever
	// that keeps the gate stable across runs.
	anchor := "an EXTRA element not required by the spec is at most Medium"
	if !strings.Contains(auditSystemPrompt, anchor) {
		t.Error("audit prompt missing severity rubric anchor rule")
	}
	if !strings.Contains(verifySystemPrompt, anchor) {
		t.Error("verify prompt missing severity rubric anchor rule")
	}
	if !strings.Contains(phaseAuditSystemPrompt, anchor) {
		t.Error("phase-audit prompt missing severity rubric anchor rule")
	}
	// The verify prompt must require BOTH existence AND warranted C/H severity.
	if !strings.Contains(verifySystemPrompt, "BOTH") {
		t.Error("verify prompt must state the existence-AND-severity contract")
	}
}

func TestAuditEnsembleInjectsPhaseFindings(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"docs/superpowers/roadmaps/r.md": "## Phase 5 — Metrics\nacceptance metrics dashboard operational\n",
	})
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		switch name {
		case "submit_graduation_verdict":
			return json.RawMessage(`{"status":"COMPLETE","summary":"s","findings":[]}`), nil
		case "submit_phase_audit":
			return json.RawMessage(`{"criteria":[{"description":"dashboard operational","met":false,"severity":"High"}]}`), nil
		default: // submit_finding_verification
			return json.RawMessage(`{"confirmed":true,"reason":"unwired"}`), nil
		}
	}}
	v, err := AuditEnsemble(context.Background(), runner, dir, "main", "feat", nil, "ok",
		EnsembleOptions{K: 1, PhaseAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range v.Findings {
		if strings.Contains(f.Title, "Phase 5") {
			found = true
		}
	}
	if !found {
		t.Fatalf("phase-5 deliverable finding must be injected; got %+v", v.Findings)
	}
	if !v.Blocks() {
		t.Error("a confirmed unmet High deliverable must block")
	}
}

func TestAuditEnsemblePhaseDisabled(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"docs/superpowers/roadmaps/r.md": "## Phase 5 — Metrics\ndashboard\n",
	})
	runner := toolStubRunner{fn: func(name, _ string) (json.RawMessage, error) {
		if name == "submit_graduation_verdict" {
			return json.RawMessage(`{"status":"COMPLETE","summary":"s","findings":[]}`), nil
		}
		return json.RawMessage(`{"criteria":[{"description":"x","met":false,"severity":"High"}]}`), nil
	}}
	v, err := AuditEnsemble(context.Background(), runner, dir, "main", "feat", nil, "ok",
		EnsembleOptions{K: 1, PhaseAudit: false})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Findings {
		if strings.Contains(f.Title, "Phase 5") {
			t.Error("phase_audit=false must not inject phase findings")
		}
	}
}
