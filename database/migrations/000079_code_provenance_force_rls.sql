-- P6 tenant isolation: enforce the runtime tenant boundary on code provenance.
-- A non-BYPASSRLS scos_app connection with no transaction-local tenant must
-- see no repository, snapshot, symbol or symbol-version rows.
-- No context-free policy is permitted. Owners of these tables are also
-- subject to the policies unless they are PostgreSQL superusers/BYPASSRLS.
--
-- This migration must land with tenant-scoped code index/read transactions
-- and the restricted-role qualification in P4 PostgreSQL CI.
BEGIN;

ALTER TABLE code_repositories ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_repository_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_symbols ENABLE ROW LEVEL SECURITY;
ALTER TABLE code_symbol_versions ENABLE ROW LEVEL SECURITY;

CREATE POLICY p6_code_repository_tenant
ON code_repositories
FOR ALL
USING (
    tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
)
WITH CHECK (
    tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
);

CREATE POLICY p6_code_snapshot_tenant
ON code_repository_snapshots
FOR ALL
USING (
    tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    AND EXISTS (
        SELECT 1
          FROM code_repositories repo
         WHERE repo.id = code_repository_snapshots.repository_id
           AND repo.tenant_id = code_repository_snapshots.tenant_id
    )
)
WITH CHECK (
    tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    AND EXISTS (
        SELECT 1
          FROM code_repositories repo
         WHERE repo.id = code_repository_snapshots.repository_id
           AND repo.tenant_id = code_repository_snapshots.tenant_id
    )
);

-- Stable symbols do not have a tenant_id column: resolve through their
-- repository, which is itself protected by forced RLS.
CREATE POLICY p6_code_symbol_tenant
ON code_symbols
FOR ALL
USING (
    EXISTS (
        SELECT 1 FROM code_repositories repo
         WHERE repo.id = code_symbols.repository_id
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1 FROM code_repositories repo
         WHERE repo.id = code_symbols.repository_id
    )
);

-- A symbol version must belong both to an accessible symbol and to a
-- snapshot in the same repository; an unrelated UUID is not sufficient.
CREATE POLICY p6_code_symbol_version_tenant
ON code_symbol_versions
FOR ALL
USING (
    EXISTS (
        SELECT 1
          FROM code_symbols sym
          JOIN code_repository_snapshots snap
            ON snap.repository_id = sym.repository_id
         WHERE sym.id = code_symbol_versions.symbol_id
           AND snap.id = code_symbol_versions.snapshot_id
    )
)
WITH CHECK (
    EXISTS (
        SELECT 1
          FROM code_symbols sym
          JOIN code_repository_snapshots snap
            ON snap.repository_id = sym.repository_id
         WHERE sym.id = code_symbol_versions.symbol_id
           AND snap.id = code_symbol_versions.snapshot_id
    )
);

ALTER TABLE code_repositories FORCE ROW LEVEL SECURITY;
ALTER TABLE code_repository_snapshots FORCE ROW LEVEL SECURITY;
ALTER TABLE code_symbols FORCE ROW LEVEL SECURITY;
ALTER TABLE code_symbol_versions FORCE ROW LEVEL SECURITY;

COMMIT;
