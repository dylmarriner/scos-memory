BEGIN;

-- Agent ownership: the principal that registered an agent is the one allowed
-- to heartbeat or deregister it (services/agent-registry). NULL marks a
-- legacy row registered before ownership was recorded. scos_app's DML
-- grants are table-wide (policies/000011_app_role.sql), so the new column
-- needs no grant of its own, and the existing tenant RLS policy
-- (policies/000006_agent_registry_rls.sql) already covers it.
ALTER TABLE agent_registrations
    ADD COLUMN IF NOT EXISTS registered_by_principal_id uuid NULL REFERENCES principals(id);

-- Staleness sweep. The sweep is cross-tenant background work, but
-- agent-registry connects as scos_app, which the tenant policy confines to
-- one tenant per transaction and which cannot even enumerate tenants. This
-- SECURITY DEFINER function is the single cross-tenant operation it may
-- perform. It takes no arguments: the window is fixed at the registry's 24h
-- staleness window (agentStalenessWindow in agent-registry), so a caller can
-- only expire agents that are already reported inactive, never pick a
-- shorter window to deactivate live agents. EXECUTE is revoked from PUBLIC
-- here and granted to scos_app alone in policies/000017 (scos_app does not
-- exist until policies/000011 runs, after every migration).
--
-- UPDATE ... WHERE status='active' ... RETURNING makes each stale transition
-- fire exactly once: a concurrent sweep blocks on the row lock, re-checks
-- the WHERE clause after the first commits, and no longer matches.
CREATE OR REPLACE FUNCTION agent_registrations_expire_stale()
RETURNS TABLE (agent_id uuid, tenant_id uuid, registered_by_principal_id uuid)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    UPDATE agent_registrations a
    SET status = 'inactive'
    WHERE a.status = 'active' AND a.last_seen_at < now() - interval '24 hours'
    RETURNING a.id, a.tenant_id, a.registered_by_principal_id
$$;
REVOKE EXECUTE ON FUNCTION agent_registrations_expire_stale() FROM PUBLIC;

-- FORCE ROW LEVEL SECURITY (policies/000012) binds the table owner too, so
-- unless the owner is a superuser the definer function would see no rows.
-- These policies let only the migration-owning role (the function's owner,
-- which already owns and can alter the table) read and update rows across
-- tenants -- exactly what the sweep's UPDATE ... RETURNING needs, and no
-- cross-tenant INSERT or DELETE. scos_app's access is unchanged.
DROP POLICY IF EXISTS agent_registrations_owner_sweep ON agent_registrations;
DROP POLICY IF EXISTS agent_registrations_owner_sweep_select ON agent_registrations;
DROP POLICY IF EXISTS agent_registrations_owner_sweep_update ON agent_registrations;
CREATE POLICY agent_registrations_owner_sweep_select ON agent_registrations
    FOR SELECT TO CURRENT_USER USING (true);
CREATE POLICY agent_registrations_owner_sweep_update ON agent_registrations
    FOR UPDATE TO CURRENT_USER USING (true) WITH CHECK (true);

COMMIT;
