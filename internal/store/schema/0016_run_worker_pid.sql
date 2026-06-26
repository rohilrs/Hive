-- Phase 7 restart-recovery hardening: track the OS PID of the worker
-- subprocess (claude -p) servicing each run. Set on subprocess start,
-- cleared on exit. After a daemon crash this is the survivor list
-- that recoverOrphanedWorkers reads to SIGKILL leftover processes.
ALTER TABLE runs ADD COLUMN worker_pid INTEGER NULL;

CREATE INDEX IF NOT EXISTS runs_worker_pid_idx
  ON runs(worker_pid)
  WHERE worker_pid IS NOT NULL;
