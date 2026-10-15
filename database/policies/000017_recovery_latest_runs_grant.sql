BEGIN;

-- Only the runtime service role may read recovery runs across services
-- (migrations/000050). Guarded like 000015 for deployments that provision
-- scos_app elsewhere.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION scos_recovery_latest_runs() TO scos_app;
    END IF;
END
$$;

COMMIT;
