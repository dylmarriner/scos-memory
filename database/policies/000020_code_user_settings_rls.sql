BEGIN;

-- The code-intelligence preference table was introduced before the runtime
-- restricted-role boundary and had no RLS. It is read on every /v2/code
-- request, so the handler must set scos.tenant_id transaction-locally.
-- The policy also blocks direct scos_app reads/writes without that context.
ALTER TABLE code_intelligence_user_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_intelligence_user_settings FORCE ROW LEVEL SECURITY;

CREATE POLICY code_intelligence_user_settings_tenant_access
    ON code_intelligence_user_settings FOR ALL
    USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
