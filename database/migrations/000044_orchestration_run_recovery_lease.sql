BEGIN;

-- Recovery lease for orchestration runs (autonomous-control-plane).
--
-- A recovery claims a run by setting the owner and expiry in one conditional
-- UPDATE that only matches when no live lease exists; progress writes renew
-- the lease, and a run whose lease expired (its recoverer crashed) can be
-- claimed again. Attempt counts and permanent-failure markers live in the
-- run's checkpoint JSON alongside the rest of its recovery state.
--
-- The columns inherit orchestration_runs' existing row-level security
-- (orchestration_runs_tenant_access, scos_worker_access) and the table-wide
-- scos_app / scos_worker grants (000011, 000015); no new privileges are needed.
ALTER TABLE orchestration_runs
    ADD COLUMN IF NOT EXISTS recovery_lease_owner text,
    ADD COLUMN IF NOT EXISTS recovery_lease_expires_at timestamptz;

-- The recovery sweeper scans by status for runs without a live lease.
CREATE INDEX IF NOT EXISTS orchestration_runs_recovery_lease_idx
    ON orchestration_runs (status, recovery_lease_expires_at);

COMMIT;
