BEGIN;

-- The gateway sets these transaction-local values after authenticating the request.
-- Policies deliberately fail closed when either value is absent.
CREATE POLICY namespaces_tenant_read ON namespaces
    FOR SELECT USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);
CREATE POLICY namespaces_member_write ON namespaces
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND (owner_principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
            OR EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = namespaces.id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_administer)))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND (owner_principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
            OR EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = namespaces.id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_administer)));

CREATE POLICY sessions_tenant_member ON sessions
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = sessions.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_read))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = sessions.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_read));

CREATE POLICY messages_tenant_access ON messages
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM sessions s JOIN namespace_memberships m ON m.namespace_id = s.namespace_id WHERE s.id = messages.session_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_read))
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY memory_items_namespace_access ON memory_items
    FOR SELECT USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = memory_items.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_read));
CREATE POLICY memory_items_candidate_write ON memory_items
    FOR INSERT WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = memory_items.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_write_candidates));

CREATE POLICY memory_versions_namespace_access ON memory_versions
    FOR SELECT USING (EXISTS (SELECT 1 FROM memory_items i WHERE i.id = memory_versions.memory_id));

CREATE POLICY tool_executions_operation_access ON tool_executions
    FOR ALL USING (EXISTS (SELECT 1 FROM operations o WHERE o.id = tool_executions.operation_id AND o.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid))
    WITH CHECK (EXISTS (SELECT 1 FROM operations o WHERE o.id = tool_executions.operation_id AND o.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid));

CREATE POLICY retrieval_runs_principal_access ON retrieval_runs
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid AND principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid);

CREATE POLICY retrieval_results_run_access ON retrieval_results
    FOR ALL USING (EXISTS (SELECT 1 FROM retrieval_runs r WHERE r.id = retrieval_results.retrieval_run_id))
    WITH CHECK (EXISTS (SELECT 1 FROM retrieval_runs r WHERE r.id = retrieval_results.retrieval_run_id));

ALTER TABLE turns ENABLE ROW LEVEL SECURITY;
ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_jobs ENABLE ROW LEVEL SECURITY;

CREATE POLICY turns_session_access ON turns
    FOR ALL USING (EXISTS (
        SELECT 1 FROM sessions s
        WHERE s.id = turns.session_id
          AND s.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
          AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = s.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_read)
    ))
    WITH CHECK (EXISTS (
        SELECT 1 FROM sessions s
        WHERE s.id = turns.session_id
          AND s.tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
          AND EXISTS (SELECT 1 FROM namespace_memberships m WHERE m.namespace_id = s.namespace_id AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid AND m.can_read)
    ));

CREATE POLICY events_tenant_access ON events
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY operations_tenant_access ON operations
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

CREATE POLICY outbox_jobs_tenant_access ON outbox_jobs
    FOR ALL USING (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid);

COMMIT;
