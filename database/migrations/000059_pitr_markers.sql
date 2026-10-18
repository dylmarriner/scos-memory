BEGIN;

-- Point-in-time recovery drills (services/backup-manager/cmd/server/pitr.go)
-- write one marker before and one after a named restore point, replay
-- archived WAL into a scratch cluster up to that point, and pass only if the
-- first marker survived and the second did not. The markers live outside the
-- public schema so they never enter a logical backup's data fingerprint, and
-- each drill deletes its own once it finishes.
CREATE SCHEMA IF NOT EXISTS scos_ops;
REVOKE ALL ON SCHEMA scos_ops FROM PUBLIC;

CREATE TABLE IF NOT EXISTS scos_ops.pitr_markers (
    run_id     text        NOT NULL,
    phase      text        NOT NULL CHECK (phase IN ('before', 'after')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, phase)
);

ALTER TABLE scos_ops.pitr_markers ENABLE ROW LEVEL SECURITY;
ALTER TABLE scos_ops.pitr_markers FORCE ROW LEVEL SECURITY;

-- Only the backup operator's connection (the database owner, a superuser,
-- which bypasses row security) reads or writes markers; every other role,
-- the scos_app runtime role included, sees and writes nothing.
DROP POLICY IF EXISTS pitr_markers_operator_only ON scos_ops.pitr_markers;
CREATE POLICY pitr_markers_operator_only ON scos_ops.pitr_markers
    FOR ALL USING (false) WITH CHECK (false);
REVOKE ALL ON scos_ops.pitr_markers FROM PUBLIC;

COMMIT;
