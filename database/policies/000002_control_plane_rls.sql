BEGIN;

-- Control-plane state is tenant scoped and fails closed without transaction context.
ALTER TABLE orchestration_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE routing_decisions ENABLE ROW LEVEL SECURITY;

CREATE POLICY orchestration_runs_tenant_access ON orchestration_runs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY routing_decisions_run_access ON routing_decisions
    FOR ALL
    USING (EXISTS (
        SELECT 1 FROM orchestration_runs r
        WHERE r.id = routing_decisions.orchestration_run_id
          AND r.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM orchestration_runs r
        WHERE r.id = routing_decisions.orchestration_run_id
          AND r.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    ));

COMMIT;