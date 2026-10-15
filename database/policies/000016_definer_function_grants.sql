BEGIN;

-- Execute grants for the SECURITY DEFINER functions services call as the
-- restricted scos_app role (migrations 000041 onward). Guarded so the file
-- also applies where that role is provisioned elsewhere.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION scos_security_token_lookup(text) TO scos_app;
    END IF;
END
$$;

COMMIT;
