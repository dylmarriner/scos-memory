-- P6 provenance integrity: a snapshot's tenant is not an independent claim.
-- It must equal the tenant of its referenced repository, including on later
-- updates. The legacy repository_id FK alone permits cross-tenant snapshots.
-- This is a database invariant, not a substitute for forced RLS on code tables.
BEGIN;
SET LOCAL lock_timeout = '5s';

-- PostgreSQL requires an explicit unique key covering both referenced fields.
ALTER TABLE code_repositories
    ADD CONSTRAINT code_repositories_id_tenant_unique UNIQUE (id, tenant_id);

-- NOT VALID avoids an immediate full child-table scan while installing the
-- constraint. All new writes are checked from this point onward.
ALTER TABLE code_repository_snapshots
    ADD CONSTRAINT code_repository_snapshots_repo_tenant_fk
    FOREIGN KEY (repository_id, tenant_id)
    REFERENCES code_repositories(id, tenant_id)
    ON DELETE CASCADE
    NOT VALID;
COMMIT;

-- Existing rows must be checked too: reject drift instead of silently
-- grandfathering mismatched historical provenance.
BEGIN;
SET LOCAL lock_timeout = '5s';
ALTER TABLE code_repository_snapshots
    VALIDATE CONSTRAINT code_repository_snapshots_repo_tenant_fk;
COMMIT;
