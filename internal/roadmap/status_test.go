package roadmap

import (
	"strings"
	"testing"
)

func TestSetPhaseStatus(t *testing.T) {
	md := "# Roadmap\n\n## 1. Foundation\n\n**Status:** ⬜ Not started — build it.\n\nBody line.\n\n## 2. Snapshot\n\n**Status:** ⬜ Not started.\n"
	out, changed := SetPhaseStatus(md, "1", "✅ Done — marked complete via Hive")
	if !changed {
		t.Fatal("expected changed=true")
	}
	if !strings.Contains(out, "**Status:** ✅ Done — marked complete via Hive") {
		t.Errorf("phase 1 Status not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "## 2. Snapshot\n\n**Status:** ⬜ Not started.") {
		t.Error("phase 2 Status must be untouched")
	}
	// Insert when absent.
	md2 := "## 3. Coordinator\n\nSome body without a status line.\n"
	out2, changed2 := SetPhaseStatus(md2, "3", "✅ Done")
	if !changed2 || !strings.Contains(out2, "**Status:** ✅ Done") {
		t.Errorf("expected an inserted Status line:\n%s", out2)
	}
	// Missing phase → no change.
	if _, changed3 := SetPhaseStatus(md, "9", "✅ Done"); changed3 {
		t.Error("missing phase should report changed=false")
	}

	// Non-bold "Status:" variant rewrites correctly, preserving the plain prefix.
	md4 := "## 4. Cleanup\n\nStatus: old status\n"
	out4, changed4 := SetPhaseStatus(md4, "4", "✅ Done")
	if !changed4 {
		t.Fatal("expected changed=true for non-bold Status: variant")
	}
	if !strings.Contains(out4, "Status: ✅ Done") {
		t.Errorf("non-bold Status: prefix not preserved:\n%s", out4)
	}
	if strings.Contains(out4, "old status") {
		t.Errorf("old status value should have been replaced:\n%s", out4)
	}

	// Prose line starting with "Status" (no colon immediately after) must NOT
	// be treated as the status line — SetPhaseStatus should INSERT instead.
	md5 := "## 5. Migration\n\nStatus of the migration is tracked elsewhere.\n"
	out5, changed5 := SetPhaseStatus(md5, "5", "✅ Done")
	if !changed5 {
		t.Fatal("expected changed=true (insert path) for phase with prose Status line")
	}
	if !strings.Contains(out5, "**Status:** ✅ Done") {
		t.Errorf("expected inserted **Status:** line:\n%s", out5)
	}
	if !strings.Contains(out5, "Status of the migration is tracked elsewhere.") {
		t.Errorf("prose line must be preserved:\n%s", out5)
	}

	// Phase A (no status) followed by phase B (with status) — completing A must
	// not touch B's status line.
	md6 := "## 6. Alpha\n\nBody of alpha.\n\n## 7. Beta\n\n**Status:** ⬜ Not started.\n"
	out6, changed6 := SetPhaseStatus(md6, "6", "✅ Done")
	if !changed6 {
		t.Fatal("expected changed=true when inserting into phase 6")
	}
	if !strings.Contains(out6, "**Status:** ✅ Done") {
		t.Errorf("phase 6 should have a new Status line:\n%s", out6)
	}
	if !strings.Contains(out6, "## 7. Beta\n\n**Status:** ⬜ Not started.") {
		t.Errorf("phase 7 Status must be untouched:\n%s", out6)
	}
}

func TestPhaseStatusIsDone(t *testing.T) {
	md := strings.Join([]string{
		"## 1. A", "", "**Status:** ✅ Done — marked complete via Hive", "",
		"## 2. B", "", "**Status:** ✅ Done — merged via PR #190", "",
		"## 3. C", "", "**Status:** ⬜ Not started — confirmed incomplete; build it", "",
		"## 4. D", "", "Some body, no status line.", "",
		"## 5. E", "", "**Status:** Complete", "",
		"## 6. F", "", "**Status:** ✅ Completed", "",
		"## 7. G", "", "**Status:** not complete yet", "",
	}, "\n")
	cases := []struct {
		phase string
		want  bool
	}{
		{"1", true},
		{"2", true},
		{"3", false},
		{"4", false},
		{"5", true},
		{"6", true},  // "Completed" — word-boundary variant
		{"7", false}, // "not complete" — negation wins over the "complete" word
		{"9", false},
	}
	for _, c := range cases {
		if got := PhaseStatusIsDone(md, c.phase); got != c.want {
			t.Errorf("PhaseStatusIsDone(phase %s)=%v, want %v", c.phase, got, c.want)
		}
	}
}

func TestSetPhaseStatusNoOpWhenUnchanged(t *testing.T) {
	md := "## 1. Foundation\n\n**Status:** ✅ Done\n\nbody\n"
	out, changed := SetPhaseStatus(md, "1", "✅ Done")
	if changed {
		t.Errorf("expected changed=false when Status already equals the target; got true\n%s", out)
	}
	if out != md {
		t.Errorf("markdown should be returned byte-identical when unchanged")
	}
	if _, changed2 := SetPhaseStatus(md, "1", "✅ Done — marked complete via Hive"); !changed2 {
		t.Error("a genuine status change must still return changed=true")
	}
	// A stored line with trailing whitespace but the same logical value is a no-op.
	mdTrail := "## 1. Foundation\n\n**Status:** ✅ Done  \n\nbody\n"
	if _, changed3 := SetPhaseStatus(mdTrail, "1", "✅ Done"); changed3 {
		t.Error("trailing whitespace on the stored line must not count as a change")
	}
}
