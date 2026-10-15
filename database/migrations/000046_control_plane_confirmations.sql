BEGIN;

-- The autonomous control plane used to tell the execution proxy a command
-- was confirmed whenever its decision did NOT require confirmation, and so
-- never recorded or waited for a real one. A decision that requires
-- confirmation now stops in orchestration_runs.status 'awaiting_confirmation'
-- and executes only after the requesting principal answers through an
-- authenticated POST /v1/control/confirm. That answer is recorded here.
--
-- One row per decision that required confirmation. requested_by is the
-- principal whose request produced the decision; only that principal may
-- resolve it, which the policy below and the resolved_by check both enforce.
-- 'superseded' closes a row whose interaction was re-routed before it was
-- answered.
CREATE TABLE IF NOT EXISTS control_confirmations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    orchestration_run_id uuid NOT NULL REFERENCES orchestration_runs(id) ON DELETE CASCADE,
    decision_id text NOT NULL UNIQUE,
    requested_by uuid NOT NULL REFERENCES principals(id),
    status text NOT NULL CHECK (status IN ('awaiting', 'confirmed', 'rejected', 'superseded')),
    resolved_by uuid REFERENCES principals(id),
    resolved_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (status NOT IN ('confirmed', 'rejected') OR (resolved_by IS NOT NULL AND resolved_at IS NOT NULL)),
    CHECK (resolved_by IS NULL OR resolved_by = requested_by)
);
CREATE INDEX IF NOT EXISTS control_confirmations_run_idx
    ON control_confirmations (orchestration_run_id, status);

-- route_request is the request frozen at ingress: a confirmation and a
-- recovery execute exactly it, never a caller's re-send. execution_attempt
-- fences stage progress writes to the executor that claimed the run, so a
-- recovery can take over a run whose executor died without that executor
-- overwriting it later.
ALTER TABLE orchestration_runs
    ADD COLUMN IF NOT EXISTS route_request jsonb,
    ADD COLUMN IF NOT EXISTS execution_attempt text;

-- Stage rows are rewritten on every transition; stage_order keeps them in
-- plan order (rows written in one transaction share created_at).
ALTER TABLE lifecycle_stages
    ADD COLUMN IF NOT EXISTS stage_order integer;
CREATE INDEX IF NOT EXISTS lifecycle_stages_interaction_idx
    ON lifecycle_stages (tenant_id, input_reference, stage_order);

ALTER TABLE control_confirmations ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_confirmations FORCE ROW LEVEL SECURITY;

-- Tenant data, scoped to the requesting principal: another principal in the
-- same tenant can neither see nor answer the confirmation.
CREATE POLICY control_confirmations_requester_access ON control_confirmations
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND requested_by = NULLIF(current_setting('scos.principal_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND requested_by = NULLIF(current_setting('scos.principal_id', true), '')::uuid
        AND (resolved_by IS NULL OR resolved_by = NULLIF(current_setting('scos.principal_id', true), '')::uuid)
    );

COMMIT;
