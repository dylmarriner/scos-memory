BEGIN;

ALTER TABLE hierarchy_nodes ENABLE ROW LEVEL SECURITY;

CREATE POLICY hierarchy_nodes_tenant_access ON hierarchy_nodes FOR ALL
    USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
