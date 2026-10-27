BEGIN;

-- Existing installations have the source-only memory_relations RLS policy.
-- Fresh installations apply the corrected definition from policies/000003
-- after migrations. Do not create a policy prematurely in that sequence.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_policies WHERE schemaname='public'
               AND tablename='memory_relations' AND policyname='memory_relations_item_access') THEN
        DROP POLICY memory_relations_item_access ON memory_relations;
        CREATE POLICY memory_relations_item_access ON memory_relations
            FOR ALL USING (EXISTS (
                SELECT 1 FROM memory_items source
                JOIN memory_items target ON target.id = memory_relations.target_memory_id
                WHERE source.id = memory_relations.source_memory_id
                  AND source.tenant_id = target.tenant_id
            ))
            WITH CHECK (EXISTS (
                SELECT 1 FROM memory_items source
                JOIN memory_items target ON target.id = memory_relations.target_memory_id
                WHERE source.id = memory_relations.source_memory_id
                  AND source.tenant_id = target.tenant_id
            ));
    END IF;
END $$;

COMMIT;
