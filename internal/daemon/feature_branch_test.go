package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/rohilrs/Hive/internal/pipeline"
	"github.com/rohilrs/Hive/internal/store"
)

func TestEffectiveFeatureBranchForProject(t *testing.T) {
	d := newTestDaemon(t)
	// Default config has no feature branch; an unknown project has no per-project
	// override → resolves to empty.
	if got := d.scheduler.effectiveFeatureBranchForProject("nope"); got != "" {
		t.Errorf("unset feature branch = %q, want empty", got)
	}
	if d.scheduler.taskAutoIntegrateForProject("nope") {
		t.Error("taskAutoIntegrate should default false")
	}
}

func TestMergeMethodForProject_Default(t *testing.T) {
	d := newTestDaemon(t)
	if got := d.scheduler.mergeMethodForProject("nope"); got != "merge" {
		t.Errorf("default merge method = %q, want merge", got)
	}
}

func TestIsAlreadyMergedErr(t *testing.T) {
	alreadyMerged := []string{
		"gh pr merge https://…: exit status 1: Pull request #5 is already merged",
		"gh pr merge https://…: exit status 1: ... is not open (it was merged)",
		"gh pr merge https://…: exit status 1: GraphQL: Pull request is closed (addPullRequestReview)",
	}
	for _, s := range alreadyMerged {
		if !isAlreadyMergedErr(errors.New(s)) {
			t.Errorf("should classify as already-merged: %q", s)
		}
	}
	if isAlreadyMergedErr(errors.New("Pull Request is not mergeable: the merge commit cannot be cleanly created")) {
		t.Error("a content conflict must NOT classify as already-merged")
	}
	// The tightened substring must NOT match a bare "not open" in an unrelated error.
	if isAlreadyMergedErr(errors.New("dial tcp: connection not open")) {
		t.Error("an unrelated 'not open' error must NOT classify as already-merged")
	}
	if isAlreadyMergedErr(nil) {
		t.Error("nil must not classify as already-merged")
	}
	// Verify mutual exclusion with isMergeConflictErr on every already-merged string.
	for _, s := range alreadyMerged {
		if isMergeConflictErr(errors.New(s)) {
			t.Errorf("already-merged string must NOT classify as conflict: %q", s)
		}
	}
}

func TestMaybeAutoIntegrate_GatesOnConfig(t *testing.T) {
	d := newTestDaemon(t)
	proj := &store.Project{ID: "p1", Slug: "demo", Name: "Demo", Status: "active"}
	_ = d.store.InsertProject(context.Background(), proj)
	run := &store.Run{ID: "r1", Pipeline: "build"}
	task := &store.Task{ID: "t1", ProjectID: "p1"}
	res := &pipeline.Result{Status: "done"}
	transferred := false
	// No feature branch / auto-integrate off → must NOT chain.
	if d.scheduler.maybeAutoIntegrate(context.Background(), run, task, proj, res, "/tmp/wt", "br", &transferred) {
		t.Error("must not auto-integrate without feature_branch + task_auto_integrate")
	}
	if transferred {
		t.Error("transferred must stay false when not chaining")
	}
}
