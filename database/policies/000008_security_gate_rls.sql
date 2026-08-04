BEGIN;

ALTER TABLE security_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE secret_references ENABLE ROW LEVEL SECURITY;

CREATE POLICY security_tokens_tenant_access ON security_tokens
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY secret_references_tenant_access ON secret_references
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
