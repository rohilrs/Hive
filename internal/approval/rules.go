package approval

import (
	"regexp"
	"strconv"
	"strings"
)

// canonicalArg extracts the glob-matchable argument from a tool input:
// the shell command for Bash, the file path for edit/read tools, else "".
func canonicalArg(req ToolUseRequest) string {
	switch req.ToolName {
	case "Bash":
		if c, ok := req.ToolInput["command"].(string); ok {
			return c
		}
	case "Edit", "Write", "Read", "MultiEdit":
		if f, ok := req.ToolInput["file_path"].(string); ok {
			return f
		}
	}
	return ""
}

// ruleMatches reports whether a rule applies: scope match, tool-name
// match (exact or "*"), and arg glob (empty matcher = any). The glob is
// path.Match-style (*/?/[]); note '*' does not cross '/'.
func ruleMatches(r Rule, req ToolUseRequest, arg string) bool {
	if !scopeMatches(r.Scope, req) {
		return false
	}
	if r.ToolName != "*" && r.ToolName != req.ToolName {
		return false
	}
	if r.ArgMatcher == "" {
		return true
	}
	return globMatch(r.ArgMatcher, arg)
}

// globMatch translates a shell-style glob (* = any chars INCLUDING '/',
// ? = one char) to an anchored regexp and matches it. Unlike path.Match,
// '*' spans path separators — required for shell commands like
// "go build ./..." matching "go *". Mirrors Claude Code's permission glob.
func globMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

func scopeMatches(scope string, req ToolUseRequest) bool {
	switch {
	case scope == "" || scope == "global":
		return true
	case strings.HasPrefix(scope, "stage:"):
		return strings.TrimPrefix(scope, "stage:") == req.Stage
	case strings.HasPrefix(scope, "project:"):
		return strings.TrimPrefix(scope, "project:") == req.Project
	}
	return false
}

func ruleID(r Rule) string {
	if r.ID > 0 {
		return "rule:" + strconv.FormatInt(r.ID, 10)
	}
	return "default"
}
