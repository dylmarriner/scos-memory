BEGIN;

-- capability_registry (000004) is now written at runtime: the autonomous
-- control plane persists each platform capability's observed success rate
-- and quality there after every executed route stage, and restores them at
-- startup. Like the shared capabilities table (policies/000010), it is
-- non-tenant reference data, so it gets the same gate: any authenticated
-- service-context principal, with RLS forced so the restricted scos_app role
-- is bound by it.
ALTER TABLE capability_registry ENABLE ROW LEVEL SECURITY;
ALTER TABLE capability_registry FORCE ROW LEVEL SECURITY;

CREATE POLICY capability_registry_authenticated_access ON capability_registry
    FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL)
    WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);

COMMIT;
