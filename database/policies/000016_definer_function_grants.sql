BEGIN;

-- Execute grants for the SECURITY DEFINER functions services call as the
-- restricted scos_app role (migrations 000041 onward). Guarded so the file
-- also applies where that role is provisioned elsewhere.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION scos_security_token_lookup(text) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_cloud_create_tenant(text) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_cloud_user_principal(text) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_cloud_tenant_count() TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_claim_outbox_jobs(text, text[], integer, integer) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_synthesis_namespaces(integer) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_unembedded_versions(uuid, integer) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_store_embedding(uuid, uuid, text, text, text, integer, text, text) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_expire_memories(timestamptz) TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_embedding_backlog() TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_worker_policy_tenants() TO scos_app;
    END IF;
END
$$;

COMMIT;
