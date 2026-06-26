-- Sequenced-dispatcher Phase 1: persist the branch a run worked on and the
-- PR it opened. branch_name is set at worktree-create; pr_url/pr_number are
-- parsed from the finish-branch create-pr stage output. Needed by the
-- Phase 3 merge-poller to look up "what PR corresponds to this run" without
-- shelling into the worktree.
ALTER TABLE runs ADD COLUMN branch_name TEXT NULL;
ALTER TABLE runs ADD COLUMN pr_url TEXT NULL;
ALTER TABLE runs ADD COLUMN pr_number INTEGER NULL;
