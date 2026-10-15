BEGIN;

-- Bearer-token validation happens before the issuing tenant is known: the
-- token carries only its random public identifier. security_tokens is under
-- FORCE RLS keyed on scos.tenant_id, so security-gate (connected as the
-- restricted scos_app role) could never find any token it had issued and
-- every validation answered "not found". This SECURITY DEFINER function
-- performs exactly the keyed, single-row lookup validation needs and
-- nothing broader; revocation then runs inside the token's own tenant
-- context through the ordinary RLS path.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls)) THEN
        RAISE EXCEPTION 'security token lookup must be created by a role with SUPERUSER or BYPASSRLS (current role: %)', current_user;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION scos_security_token_lookup(p_token_id text)
RETURNS TABLE (
    tenant_id uuid,
    principal_id uuid,
    agent_id text,
    session_id text,
    max_data_class text,
    expires_at timestamptz,
    revoked_at timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT t.tenant_id, t.principal_id, t.agent_id, t.session_id, t.max_data_class, t.expires_at, t.revoked_at
    FROM security_tokens t
    WHERE t.id = p_token_id
$$;

REVOKE ALL ON FUNCTION scos_security_token_lookup(text) FROM PUBLIC;

COMMIT;
