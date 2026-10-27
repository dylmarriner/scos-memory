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
-- okf_bundles/okf_concepts are enabled and policed by
-- migrations/000011_okf_projections.sql, which supersedes the tenant/bundle
-- relational shape this file originally targeted (see that migration's
-- header comment). Re-declaring RLS/policies here against columns that no
-- longer exist on those tables would break this policies file.
ALTER TABLE object_references ENABLE ROW LEVEL SECURITY;
ALTER TABLE retention_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE service_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE extension_manager_state ENABLE ROW LEVEL SECURITY;

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
-- A readable source does not grant visibility into a hidden target. Both
-- endpoints must be visible to the same scoped principal and tenant.
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
CREATE POLICY object_references_tenant_access ON object_references
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY retention_rules_tenant_access ON retention_rules
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY audit_entries_tenant_access ON audit_entries
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY service_state_service_access ON service_state
    FOR ALL USING (service = NULLIF(current_setting('scos.service_id', true), ''))
    WITH CHECK (service = NULLIF(current_setting('scos.service_id', true), ''));
CREATE POLICY extension_manager_service_access ON extension_manager_state
    FOR ALL USING (NULLIF(current_setting('scos.service_id', true), '') = 'extension-manager')
    WITH CHECK (NULLIF(current_setting('scos.service_id', true), '') = 'extension-manager');

COMMIT;
