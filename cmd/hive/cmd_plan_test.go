package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// TestPlanCmdRequiresProjectArg verifies cobra rejects the command when
// no project arg is supplied. (Args = cobra.ExactArgs(1).)
func TestPlanCmdRequiresProjectArg(t *testing.T) {
	cmd := newPlanCmd()
	cmd.SetArgs([]string{})
	// Silence cobra's usage output to keep the test log clean.
	cmd.SetOut(discardWriter{})
	cmd.SetErr(discardWriter{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no project given")
	}
}

// TestPlanCmdRequiresKnownProject pins the user-facing error when the
// project slug isn't registered. runPlan must surface a "not found"
// message (so the operator knows to register the project), not just
// pass the lookup error through opaque.
func TestPlanCmdRequiresKnownProject(t *testing.T) {
	deps := &planDeps{
		lookupProject: func(slug string) (rpc.ProjectView, error) {
			return rpc.ProjectView{}, errors.New("no such project")
		},
		seedSession: func(slug string) (string, error) {
			t.Fatal("seedSession should not be called when lookup fails")
			return "", nil
		},
		runREPL: func(sessionID string) error {
			t.Fatal("runREPL should not be called when lookup fails")
			return nil
		},
	}
	err := runPlan("ghost", "", deps)
	if err == nil {
		t.Fatal("expected error for unknown project")
	}
	if !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should name the slug and say not found, got %q", err.Error())
	}
}

// TestPlanCmdRequiresProjectRepoPath pins the user-facing error when a
// project exists but has no repo_path (planner reads + writes into the
// project's docs/superpowers/ tree, so repo_path is mandatory). The error
// must include the `hive project edit ... --repo` hint.
func TestPlanCmdRequiresProjectRepoPath(t *testing.T) {
	deps := &planDeps{
		lookupProject: func(slug string) (rpc.ProjectView, error) {
			return rpc.ProjectView{Slug: slug, Name: "Repoless"}, nil // RepoPath empty
		},
		seedSession: func(slug string) (string, error) {
			t.Fatal("seedSession should not be called when repo_path missing")
			return "", nil
		},
		runREPL: func(sessionID string) error {
			t.Fatal("runREPL should not be called when repo_path missing")
			return nil
		},
	}
	err := runPlan("repoless", "", deps)
	if err == nil {
		t.Fatal("expected error when repo_path empty")
	}
	if !strings.Contains(err.Error(), "repo_path") {
		t.Errorf("error should mention repo_path, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "hive project edit") {
		t.Errorf("error should hint at `hive project edit`, got %q", err.Error())
	}
}

// TestPlanCmdSeedsSessionWithKindPlanAndSlug pins the happy path:
// project resolves OK, seedSession is called with the slug, and the
// returned session id is handed off to the REPL.
func TestPlanCmdSeedsSessionWithKindPlanAndSlug(t *testing.T) {
	var seedSlug string
	var replSessionID string

	deps := &planDeps{
		lookupProject: func(slug string) (rpc.ProjectView, error) {
			return rpc.ProjectView{Slug: slug, Name: "App", RepoPath: "/repos/app"}, nil
		},
		seedSession: func(slug string) (string, error) {
			seedSlug = slug
			return "sess-fresh", nil
		},
		runREPL: func(sessionID string) error {
			replSessionID = sessionID
			return nil
		},
		setupFeatureBranch: func(slug, fb string) (planSetupResult, error) {
			return planSetupResult{Active: false}, nil
		},
		pushFeatureBranch: func(slug string) error { return nil },
		confirm:           func(prompt string) bool { return true },
	}
	if err := runPlan("my-app", "", deps); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if seedSlug != "my-app" {
		t.Errorf("seedSession got slug %q, want %q", seedSlug, "my-app")
	}
	if replSessionID != "sess-fresh" {
		t.Errorf("REPL got session %q, want %q", replSessionID, "sess-fresh")
	}
}

// TestPlanCmdResumeFlagSkipsSessionCreate pins that --resume <sess-id>
// short-circuits straight to the REPL with the supplied id, without
// dialing the daemon for a fresh chat.send. (The daemon's streamChat
// gates the planner-kind validation, so the CLI trusts the user's id.)
func TestPlanCmdResumeFlagSkipsSessionCreate(t *testing.T) {
	var replSessionID string

	deps := &planDeps{
		lookupProject: func(slug string) (rpc.ProjectView, error) {
			return rpc.ProjectView{Slug: slug, Name: "App", RepoPath: "/repos/app"}, nil
		},
		seedSession: func(slug string) (string, error) {
			t.Fatal("seedSession must NOT be called when --resume is set")
			return "", nil
		},
		runREPL: func(sessionID string) error {
			replSessionID = sessionID
			return nil
		},
	}
	if err := runPlan("my-app", "sess-123", deps); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if replSessionID != "sess-123" {
		t.Errorf("REPL got session %q, want %q", replSessionID, "sess-123")
	}
}

// TestRunPlan_InertFeatureBranchSkipsPrompt pins backward-compat: when
// plan_setup reports Active=false (no feature branch requested or configured),
// runPlan must NOT prompt the operator (clean-check or push) and must NOT push,
// while still seeding the session and running the REPL.
func TestRunPlan_InertFeatureBranchSkipsPrompt(t *testing.T) {
	var ranREPL bool
	deps := &planDeps{
		lookupProject: func(slug string) (rpc.ProjectView, error) {
			return rpc.ProjectView{Slug: slug, Name: "App", RepoPath: "/repos/app"}, nil
		},
		seedSession: func(slug string) (string, error) { return "sess-x", nil },
		runREPL: func(sessionID string) error {
			ranREPL = true
			return nil
		},
		setupFeatureBranch: func(slug, fb string) (planSetupResult, error) {
			return planSetupResult{Active: false}, nil
		},
		pushFeatureBranch: func(slug string) error {
			t.Fatal("pushFeatureBranch must NOT be called when setup is inert")
			return nil
		},
		confirm: func(prompt string) bool {
			t.Fatalf("confirm must NOT be called when setup is inert; prompt=%q", prompt)
			return false
		},
	}
	if err := runPlan("my-app", "", deps); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if !ranREPL {
		t.Error("REPL should still run when feature branch is inert")
	}
}

// discardWriter is a tiny io.Writer used to silence cobra's usage
// output during the no-args test. (We don't import io/ioutil here.)
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
