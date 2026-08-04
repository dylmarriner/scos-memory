BEGIN;

ALTER TABLE agent_registrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_handoffs ENABLE ROW LEVEL SECURITY;

CREATE POLICY agent_registrations_tenant_access ON agent_registrations FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY agent_handoffs_tenant_access ON agent_handoffs FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid) WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
