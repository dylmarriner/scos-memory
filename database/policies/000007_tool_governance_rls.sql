BEGIN;

ALTER TABLE tool_definitions ENABLE ROW LEVEL SECURITY;

CREATE POLICY tool_definitions_tenant_access ON tool_definitions
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
