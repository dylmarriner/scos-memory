BEGIN;

ALTER TABLE context_envelopes ENABLE ROW LEVEL SECURITY;

CREATE POLICY context_envelopes_tenant_access ON context_envelopes
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
