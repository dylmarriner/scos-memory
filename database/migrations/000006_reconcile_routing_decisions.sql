BEGIN;

-- 000003 established the tenant-scoped routing_decisions table before the
-- autonomous control-plane run model was added in 000004.  000004's
-- IF NOT EXISTS definition cannot change that earlier table, so add the
-- control-plane identity columns explicitly and keep both models usable.
ALTER TABLE routing_decisions
    ADD COLUMN IF NOT EXISTS orchestration_run_id uuid REFERENCES orchestration_runs(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS decision_id text;

CREATE UNIQUE INDEX IF NOT EXISTS routing_decisions_decision_id_idx
    ON routing_decisions (decision_id)
    WHERE decision_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS routing_decisions_orchestration_run_idx
    ON routing_decisions (orchestration_run_id, created_at);

COMMIT;
