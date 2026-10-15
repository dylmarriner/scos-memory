BEGIN;

-- Hosted account flows under the restricted runtime role.
--
-- cloud-control-plane connects as scos_app. Two of its steps cannot be
-- expressed through tenant RLS at all:
--   * creating a tenant: tenants has only a SELECT policy (a tenant may see
--     itself), so scos_app can never insert one -- signup and team creation
--     always failed;
--   * resolving a signed-in user's principal: login and session checks run
--     before any tenant is known, but principals is tenant-scoped, so every
--     lookup saw no rows and every login answered "invalid email or
--     password".
-- These SECURITY DEFINER functions cover exactly those two steps. All other
-- provisioning (principal, workspace, namespace, membership) runs through
-- the ordinary policies with the new tenant/principal set in the same
-- transaction.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls)) THEN
        RAISE EXCEPTION 'cloud identity functions must be created by a role with SUPERUSER or BYPASSRLS (current role: %)', current_user;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION scos_cloud_create_tenant(p_name text)
RETURNS uuid
LANGUAGE plpgsql
VOLATILE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    created uuid;
BEGIN
    IF p_name IS NULL OR length(btrim(p_name)) = 0 OR length(p_name) > 200 THEN
        RAISE EXCEPTION 'tenant name must be 1-200 characters';
    END IF;
    INSERT INTO tenants (name) VALUES (btrim(p_name)) RETURNING id INTO created;
    RETURN created;
END
$$;

-- The principal a cloud user acts as in their personal tenant: the oldest
-- principal whose external_id is the user's id (team memberships use a
-- distinct "team:<team>:<user>" external id and are not returned).
CREATE OR REPLACE FUNCTION scos_cloud_user_principal(p_user_id text)
RETURNS TABLE (tenant_id uuid, principal_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT p.tenant_id, p.id
    FROM principals p
    JOIN tenants t ON t.id = p.tenant_id
    WHERE p.external_id = p_user_id AND p.disabled_at IS NULL
    ORDER BY p.created_at ASC, p.id ASC
    LIMIT 1
$$;

CREATE OR REPLACE FUNCTION scos_cloud_tenant_count()
RETURNS bigint
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT count(*) FROM tenants
$$;

REVOKE ALL ON FUNCTION scos_cloud_create_tenant(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_cloud_user_principal(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_cloud_tenant_count() FROM PUBLIC;

COMMIT;
