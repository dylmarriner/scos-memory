BEGIN;

-- capabilities is enabled for RLS in migrations/000003_control_plane.sql
-- (alongside lifecycle_stages, routing_decisions, outbox_dead_letters) but
-- was never given a policy there -- a gap found while scoping P0-0 (make
-- RLS actually enforce). capabilities has no tenant_id column: it is
-- shared, non-tenant reference data (a capability registry read/written by
-- agent-registry, autonomous-control-plane, compatibility-gateway,
-- extension-manager, mcp-gateway, code-intelligence). Once FORCE ROW LEVEL
-- SECURITY lands on this table (000012_force_rls.sql), it would become
-- deny-all without this policy. Gated the same way the other non-tenant
-- shared tables already are (okf_bundles/okf_concepts/okf_governance_roles/
-- okf_proposals in migrations/000011 and 000019): any authenticated
-- service-context principal, full access.
CREATE POLICY capabilities_authenticated_access ON capabilities
    FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL)
    WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);

COMMIT;
