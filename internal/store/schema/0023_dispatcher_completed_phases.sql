-- completed_phases: comma-joined roadmap phase numbers the operator has marked
-- complete (shipped/empty phases), so sequence.Derive can advance past them.
ALTER TABLE sequence_dispatchers ADD COLUMN completed_phases TEXT NOT NULL DEFAULT '';
