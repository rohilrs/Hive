package worktree

import "testing"

func TestBranchName(t *testing.T) {
	cases := []struct {
		runID    string
		taskSlug string
		want     string
	}{
		{"run-3f2a", "fix-login", "hive/run-3f2a/fix-login"},
		{"run-1", "", "hive/run-1"},
		{"run-1", "Fix Login Bug!", "hive/run-1/fix-login-bug"},
		{"run-1", "this is a very very very long task title that should get truncated cleanly", "hive/run-1/this-is-a-very-very-very-long-task"},
	}
	for _, tc := range cases {
		if got := BranchName(tc.runID, tc.taskSlug); got != tc.want {
			t.Errorf("BranchName(%q, %q) = %q, want %q", tc.runID, tc.taskSlug, got, tc.want)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Fix Login Bug!":      "fix-login-bug",
		"  whitespace  ":      "whitespace",
		"Multiple---dashes":   "multiple-dashes",
		"ümlauts and spëcial": "mlauts-and-spcial", // ASCII-only
		"":                    "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
