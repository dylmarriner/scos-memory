BEGIN;

-- 000001_rls.sql gave memory_versions a SELECT policy only. With RLS forced
-- (000012_force_rls.sql) and no policy covering INSERT, every write to
-- memory_versions -- including the one upsertMemory makes for every new or
-- corrected memory -- was denied outright:
--   ERROR: new row violates row-level security policy for table
--   "memory_versions" (SQLSTATE 42501)
-- hermes-provider's Stop-hook-driven persistence (POST /v1/provider/
-- memory_write) rolls the whole transaction back on this error, so no
-- memory_items row survives either; the caller (scos-auto-persist.py) then
-- swallows the exception silently. Net effect verified live: every
-- automatic post-turn write has been failing since RLS was forced, and
-- scos_recall has been searching an empty table.
--
-- The check mirrors memory_versions_namespace_access (000001_rls.sql):
-- a version may be written for a memory_items row that exists and is
-- visible under the caller's own RLS-scoped view of memory_items, which
-- transitively enforces the same tenant/namespace/write-permission check
-- memory_items_candidate_write already applies to the parent row.
CREATE POLICY memory_versions_insert ON memory_versions
    FOR INSERT WITH CHECK (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_versions.memory_id));

COMMIT;
