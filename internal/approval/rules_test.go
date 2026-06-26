package approval

import "testing"

func TestGlobMatchSpansSlash(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"go *", "go build ./...", true},   // the path.Match bug: * must span '/'
		{"go *", "go test ./internal/x", true},
		{"go *", "gofmt -w x", false},      // prefix is "go " not "gofmt"
		{"cat *", "cat src/foo/bar.go", true},
		{"git diff*", "git diff internal/x.go", true},
		{"git diff*", "git difftool", true}, // trailing * matches anything
		{"ls*", "ls -la /home/x", true},
		{"rm *", "go build", false},
		{"npm run *", "npm run lint", true},
		{"npm run *", "npm test", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestRuleMatchesBashWithPaths(t *testing.T) {
	// Regression: a default "go *" allow rule must match a real go command
	// containing a path (the path.Match '/' limitation broke this).
	r := Rule{Scope: "global", ToolName: "Bash", ArgMatcher: "go *", Decision: "allow"}
	req := ToolUseRequest{ToolName: "Bash", ToolInput: map[string]any{"command": "go build ./cmd/hive"}}
	if !ruleMatches(r, req, canonicalArg(req)) {
		t.Error("'go *' should match 'go build ./cmd/hive'")
	}
}
