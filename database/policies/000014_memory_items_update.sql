BEGIN;

-- 000001_rls.sql gave memory_items an INSERT policy (memory_items_candidate_
-- write) but no UPDATE policy. With RLS forced, the UPDATE upsertMemory
-- issues to point a memory_items row at its (newly inserted) current
-- memory_versions row therefore matched zero rows -- not an error, just a
-- silent no-op, because Go's tx.ExecContext doesn't surface "0 rows
-- affected" as a failure and the caller wasn't checking RowsAffected
-- either (now fixed in store.go's upsertMemory). Net effect verified live:
-- every memory written after 000013 (which fixed the INSERT-into-
-- memory_versions half of this) still ended up with current_version_id
-- permanently NULL, so it existed in the table but could never be found by
-- searchMemories' `JOIN memory_versions mv ON mv.id = mi.current_version_id`
-- -- silently unrecallable forever, indistinguishable from a successful
-- write.
--
-- The check mirrors memory_items_candidate_write's INSERT check exactly,
-- extended to UPDATE.
CREATE POLICY memory_items_candidate_update ON memory_items
    FOR UPDATE USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = memory_items.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_write_candidates))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = memory_items.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_write_candidates));

COMMIT;
