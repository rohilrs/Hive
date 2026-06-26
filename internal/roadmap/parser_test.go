package roadmap

import (
	"strings"
	"testing"
)

func TestParsePhases(t *testing.T) {
	md := strings.TrimLeft(`
# planner-smoke roadmap

## Phase 1: bootstrap

Get the project scaffolding in place.

Spec: [docs/superpowers/specs/2026-05-31-bootstrap.md](docs/superpowers/specs/2026-05-31-bootstrap.md)

Task hints:
1. Initialize repo
2. Set up CI

## Phase 2: features

Build the actual product.

Spec: [./specs/2026-06-02-features.md](./specs/2026-06-02-features.md)
`, "\n")
	rm, err := Parse([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if len(rm.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(rm.Phases))
	}
	p1 := rm.Phases[0]
	if p1.Number != "1" {
		t.Errorf("phase 1 number: %q", p1.Number)
	}
	if p1.Title != "bootstrap" {
		t.Errorf("phase 1 title: %q", p1.Title)
	}
	if !strings.Contains(p1.Body, "scaffolding") {
		t.Errorf("phase 1 body missing scaffolding mention: %q", p1.Body)
	}
	if !strings.Contains(p1.Body, "Task hints") {
		t.Errorf("phase 1 body should include task hints: %q", p1.Body)
	}
	if len(p1.SpecPaths) != 1 || p1.SpecPaths[0] != "docs/superpowers/specs/2026-05-31-bootstrap.md" {
		t.Errorf("phase 1 specs: %v", p1.SpecPaths)
	}
	p2 := rm.Phases[1]
	if p2.Number != "2" || p2.Title != "features" {
		t.Errorf("phase 2 head: %q / %q", p2.Number, p2.Title)
	}
	// Relative-path normalization: ./specs/foo.md → docs/superpowers/specs/foo.md.
	// BUT we cannot infer the prefix from the roadmap content alone — that's
	// the caller's job. The parser only extracts the raw href.
	if len(p2.SpecPaths) != 1 || p2.SpecPaths[0] != "./specs/2026-06-02-features.md" {
		t.Errorf("phase 2 specs: %v", p2.SpecPaths)
	}
}

func TestParseSubPhases(t *testing.T) {
	md := strings.TrimLeft(`
# x roadmap

## Phase 1a: setup

Setup sub-phase.

## Phase 1b: scaffold

Scaffold sub-phase.
`, "\n")
	rm, err := Parse([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if len(rm.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(rm.Phases))
	}
	if rm.Phases[0].Number != "1a" || rm.Phases[1].Number != "1b" {
		t.Errorf("sub-phase numbers: %q %q", rm.Phases[0].Number, rm.Phases[1].Number)
	}
}

func TestFindPhaseByNumber(t *testing.T) {
	md := strings.TrimLeft(`
# x roadmap

## Phase 1: a
## Phase 1a: a1
## Phase 2: b
`, "\n")
	rm, _ := Parse([]byte(md))
	if p, ok := rm.FindPhase("1a"); !ok || p.Title != "a1" {
		t.Errorf("FindPhase 1a: %+v ok=%v", p, ok)
	}
	if _, ok := rm.FindPhase("99"); ok {
		t.Error("FindPhase 99 should miss")
	}
}

func TestParseRejectsEmptyOrNoPhases(t *testing.T) {
	if _, err := Parse([]byte("# title only\n\nno phases here")); err == nil {
		t.Error("expected error on no-phases markdown")
	}
}

// TestParsePreservesExistingAnnotations guards that blockquote lines written by
// the planner ("> Existing: <ref> ...") are retained verbatim in the phase Body
// so that decompose/humans can read them. Regression for existing-work reconciliation.
func TestParsePreservesExistingAnnotations(t *testing.T) {
	md := []byte("# demo roadmap\n\n## Phase 1 — Foundation\n> Existing: linear:u1 (HBA-1) — Stand up harness\n\nDo the foundational work.\n")
	rm, err := Parse(md)
	if err != nil {
		t.Fatal(err)
	}
	ph, ok := rm.FindPhase("1")
	if !ok {
		t.Fatal("phase 1 not found")
	}
	if !strings.Contains(ph.Body, "> Existing: linear:u1") {
		t.Errorf("phase body dropped the existing annotation:\n%s", ph.Body)
	}
}

// TestParseEmDashSeparator: the planner (and operators) naturally write
// "## Phase N — Title" with an em-dash, not a colon. The parser must accept
// em-dash / en-dash / hyphen as well as colon (it claims to be "tolerant").
// Regression for the dogfood "no phase headings found" parse error.
func TestParseEmDashSeparator(t *testing.T) {
	md := strings.TrimLeft(`
# conv-rework roadmap

## Phase 0 — Reconcile Shipped Work

Audit-only phase.

Spec: [docs/superpowers/specs/2026-06-04-phase0.md](docs/superpowers/specs/2026-06-04-phase0.md)

## Phase 1 — Foundations: Test Infra & Snapshot

The title itself contains a colon, which must survive.

Spec: [docs/superpowers/specs/2026-06-04-phase1.md](docs/superpowers/specs/2026-06-04-phase1.md)
`, "\n")
	rm, err := Parse([]byte(md))
	if err != nil {
		t.Fatalf("em-dash roadmap should parse, got: %v", err)
	}
	if len(rm.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(rm.Phases))
	}
	if rm.Phases[0].Number != "0" || rm.Phases[0].Title != "Reconcile Shipped Work" {
		t.Errorf("phase 0: number=%q title=%q", rm.Phases[0].Number, rm.Phases[0].Title)
	}
	if rm.Phases[1].Number != "1" || rm.Phases[1].Title != "Foundations: Test Infra & Snapshot" {
		t.Errorf("phase 1: number=%q title=%q", rm.Phases[1].Number, rm.Phases[1].Title)
	}
	if len(rm.Phases[1].SpecPaths) != 1 {
		t.Errorf("phase 1 specs: %v", rm.Phases[1].SpecPaths)
	}
}

// TestParseNumberDotHeadings: the planner sometimes writes "## 1. Title" /
// "## 2a. Title" (number-dot, no literal "Phase" word, period separator) rather
// than "## Phase 1 — Title". Both must parse, and non-phase H2s like
// "## Progress snapshot" must be ignored (regression for the conv-rework
// "no phase headings" parse error).
func TestParseNumberDotHeadings(t *testing.T) {
	md := strings.TrimLeft(`
# conv-rework roadmap

## Progress snapshot (verified 2026-06-05)

This H2 is NOT a phase and must be ignored.

## 1. Foundation — FieldState + precedence

Body of phase 1.

## 1a. Test infrastructure (parallel with Phase 1)

Body of phase 1a.

## 2a. Capture + listener expansion (Track A)

Title contains an em-dash inside (Track A) — must survive.
`, "\n")
	rm, err := Parse([]byte(md))
	if err != nil {
		t.Fatalf("number-dot roadmap should parse, got: %v", err)
	}
	if len(rm.Phases) != 3 {
		t.Fatalf("expected 3 phases (snapshot excluded), got %d: %+v", len(rm.Phases), rm.Phases)
	}
	want := []struct{ num, title string }{
		{"1", "Foundation — FieldState + precedence"},
		{"1a", "Test infrastructure (parallel with Phase 1)"},
		{"2a", "Capture + listener expansion (Track A)"},
	}
	for i, w := range want {
		if rm.Phases[i].Number != w.num || rm.Phases[i].Title != w.title {
			t.Errorf("phase %d: got num=%q title=%q want num=%q title=%q",
				i, rm.Phases[i].Number, rm.Phases[i].Title, w.num, w.title)
		}
	}
	// "Progress snapshot" must not have become a phase.
	if _, ok := rm.FindPhase(""); ok {
		t.Error("an empty-numbered phase leaked in (non-phase H2 was parsed)")
	}
}
