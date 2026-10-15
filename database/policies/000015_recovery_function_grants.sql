BEGIN;

-- Only the runtime service role may call the recovery inspection functions
-- (migrations/000040). Granted here, after 000011 provisions scos_app; the
-- guard keeps this file applicable where that role is provisioned elsewhere
-- (the integration fixture creates its own per-test role).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION scos_recovery_findings() TO scos_app;
        GRANT EXECUTE ON FUNCTION scos_recovery_requeue_stale_outbox() TO scos_app;
    END IF;
END
$$;

COMMIT;
