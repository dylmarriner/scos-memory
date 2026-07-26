BEGIN;

-- Complete the fail-closed policy boundary for journal, memory, review and
-- audit tables. Services must set transaction-local tenant/principal context
-- before accessing these relations.
ALTER TABLE message_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_relations ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_reviews ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_concepts ENABLE ROW LEVEL SECURITY;
ALTER TABLE object_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entries ENABLE ROW LEVEL SECURITY;

CREATE POLICY message_chunks_session_access ON message_chunks
    FOR ALL USING (EXISTS (
        SELECT 1 FROM messages m
        JOIN sessions s ON s.id = m.session_id
        WHERE m.id = message_chunks.message_id
          AND s.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
          AND EXISTS (SELECT 1 FROM namespace_memberships n
                      WHERE n.namespace_id = s.namespace_id
                        AND n.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                        AND n.can_read)
    )) WITH CHECK (EXISTS (
        SELECT 1 FROM messages m
        JOIN sessions s ON s.id = m.session_id
        WHERE m.id = message_chunks.message_id
          AND s.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
          AND EXISTS (SELECT 1 FROM namespace_memberships n
                      WHERE n.namespace_id = s.namespace_id
                        AND n.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                        AND n.can_read)
    ));

CREATE POLICY checkpoints_session_access ON checkpoints
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships n
                    JOIN sessions s ON s.namespace_id = n.namespace_id
                    WHERE s.id = checkpoints.session_id
                      AND n.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                      AND n.can_read))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY idempotency_principal_access ON idempotency_keys
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid);

CREATE POLICY memory_candidates_namespace_access ON memory_candidates
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships n
                    WHERE n.namespace_id = memory_candidates.namespace_id
                      AND n.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                      AND (n.can_read OR n.can_write_candidates)))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships n
                    WHERE n.namespace_id = memory_candidates.namespace_id
                      AND n.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
                      AND n.can_write_candidates));

CREATE POLICY memory_evidence_item_access ON memory_evidence
    FOR ALL USING (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_evidence.memory_id))
    WITH CHECK (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_evidence.memory_id));
CREATE POLICY memory_relations_item_access ON memory_relations
    FOR ALL USING (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_relations.source_memory_id))
    WITH CHECK (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_relations.source_memory_id));
CREATE POLICY memory_embeddings_item_access ON memory_embeddings
    FOR ALL USING (EXISTS (SELECT 1 FROM memory_versions v WHERE v.id = memory_embeddings.memory_version_id))
    WITH CHECK (EXISTS (SELECT 1 FROM memory_versions v WHERE v.id = memory_embeddings.memory_version_id));

CREATE POLICY memory_reviews_candidate_access ON memory_reviews
    FOR ALL USING (EXISTS (SELECT 1 FROM memory_candidates c
                           WHERE c.id = memory_reviews.candidate_id
                             AND c.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid))
    WITH CHECK (EXISTS (SELECT 1 FROM memory_candidates c
                       WHERE c.id = memory_reviews.candidate_id
                         AND c.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid));
CREATE POLICY okf_bundles_tenant_access ON okf_bundles
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY okf_concepts_bundle_access ON okf_concepts
    FOR ALL USING (EXISTS (SELECT 1 FROM okf_bundles b
                           WHERE b.id = okf_concepts.bundle_id
                             AND b.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid))
    WITH CHECK (EXISTS (SELECT 1 FROM okf_bundles b
                       WHERE b.id = okf_concepts.bundle_id
                         AND b.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid));
CREATE POLICY object_references_tenant_access ON object_references
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY retention_rules_tenant_access ON retention_rules
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY audit_entries_tenant_access ON audit_entries
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
