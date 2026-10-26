BEGIN;

-- P7 human review assessments are append-only evidence, not permission to
-- rewrite memory facts or to change the original snapshot-activation event.
-- Exactly one decision per event; a correction/reopen lifecycle is separate.
CREATE TABLE memory_revalidation_decisions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL UNIQUE REFERENCES memory_revalidation_events(id),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    reviewer_id uuid NOT NULL REFERENCES principals(id),
    outcome text NOT NULL CHECK (outcome IN ('still_valid','superseded','invalidated','unresolved')),
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 16 AND 2000),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX memory_revalidation_decisions_tenant_idx
    ON memory_revalidation_decisions (tenant_id, created_at DESC);

ALTER TABLE memory_revalidation_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_revalidation_decisions FORCE ROW LEVEL SECURITY;

-- Only authorized namespace reviewers can see or insert decisions. The
-- caller-supplied reviewer/tenant IDs must match transaction-local claims.
CREATE POLICY memory_revalidation_decisions_read ON memory_revalidation_decisions
    FOR SELECT USING (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (
            SELECT 1 FROM memory_revalidation_events e
            JOIN memory_items item ON item.id=e.fact_id AND item.tenant_id=e.tenant_id
            JOIN namespaces ns ON ns.id=item.namespace_id
            JOIN namespace_memberships m ON m.namespace_id=ns.id
            WHERE e.id=memory_revalidation_decisions.event_id AND e.tenant_id=memory_revalidation_decisions.tenant_id
              AND m.principal_id=NULLIF(current_setting('scos.principal_id', true), '')::uuid
              AND m.can_read AND item.sensitivity<=ns.sensitivity_ceiling
        )
    );
CREATE POLICY memory_revalidation_decisions_write ON memory_revalidation_decisions
    FOR INSERT WITH CHECK (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND reviewer_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
        AND EXISTS (
            SELECT 1 FROM memory_revalidation_events e
            JOIN memory_items item ON item.id=e.fact_id AND item.tenant_id=e.tenant_id
            JOIN namespaces ns ON ns.id=item.namespace_id
            JOIN namespace_memberships m ON m.namespace_id=ns.id
            WHERE e.id=memory_revalidation_decisions.event_id AND e.tenant_id=memory_revalidation_decisions.tenant_id
              AND e.review_status='needs_review'
              AND m.principal_id=reviewer_id AND m.can_read
              AND (m.can_promote OR m.can_administer)
              AND item.sensitivity<=ns.sensitivity_ceiling
        )
    );

REVOKE UPDATE, DELETE, TRUNCATE ON memory_revalidation_decisions FROM PUBLIC;
CREATE TRIGGER memory_revalidation_decisions_append_only
    BEFORE UPDATE OR DELETE ON memory_revalidation_decisions
    FOR EACH ROW EXECUTE FUNCTION reject_authoritative_history_mutation();

DO $runtime_grants$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='scos_app') THEN
        GRANT SELECT, INSERT ON memory_revalidation_decisions TO scos_app;
        REVOKE UPDATE, DELETE, TRUNCATE ON memory_revalidation_decisions FROM scos_app;
    END IF;
END
$runtime_grants$;

COMMIT;
