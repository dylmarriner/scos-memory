BEGIN;

-- Only the runtime service role may load recovery checkpoints
-- (migrations/000057). That migration's own grant is skipped on a fresh
-- database, where scos_app does not exist until policies/000011 runs after
-- every migration, so it is granted here as well. Guarded like 000015 for
-- deployments that provision scos_app elsewhere.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION scos_recovery_checkpoints() TO scos_app;
    END IF;
END
$$;

COMMIT;
