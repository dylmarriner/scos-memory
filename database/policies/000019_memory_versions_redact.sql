BEGIN;

-- memory_versions had no UPDATE policy, so every update matched no row. A
-- version may now be updated by a principal that may write its memory (the
-- same check memory_items_candidate_update applies), and the
-- memory_versions_append_only trigger (migration 000062) admits only an
-- update that redacts secret spans.
DROP POLICY IF EXISTS memory_versions_redact ON memory_versions;
CREATE POLICY memory_versions_redact ON memory_versions
    FOR UPDATE USING (EXISTS (SELECT 1 FROM memory_items i
        WHERE i.id = memory_versions.memory_id
          AND i.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
          AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = i.namespace_id
                      AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_write_candidates)))
    WITH CHECK (EXISTS (SELECT 1 FROM memory_items i
        WHERE i.id = memory_versions.memory_id
          AND i.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
          AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = i.namespace_id
                      AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_write_candidates)));

COMMIT;
