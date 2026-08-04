BEGIN;

-- Root identity tables (tenants, principals, agents, projects, workspaces)
-- previously relied on application-layer checks only. They are exactly the
-- multi-tenant surface RLS is meant to protect, so enforce tenant isolation
-- at the database layer as well, consistent with the rest of this policy
-- baseline.

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE principals ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;

-- `tenants` has no tenant_id column: it is the tenant. A session may only
-- see the single tenant row matching its own authenticated tenant context.
CREATE POLICY tenants_self_access ON tenants
    FOR SELECT USING (id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY principals_tenant_access ON principals
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

-- `agents` has no tenant_id column of its own; it extends `principals` by id.
CREATE POLICY agents_tenant_access ON agents
    FOR ALL USING (EXISTS (
        SELECT 1 FROM principals p
        WHERE p.id = agents.id
          AND p.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM principals p
        WHERE p.id = agents.id
          AND p.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    ));

CREATE POLICY projects_tenant_access ON projects
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY workspaces_tenant_access ON workspaces
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
