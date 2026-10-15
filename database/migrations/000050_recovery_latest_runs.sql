BEGIN;

-- Recovery status survives an orchestrator restart.
--
-- recovery_runs and recovery_events are scoped by scos.service_id, so the
-- orchestrator can only read a service's runs once it knows that service's
-- id -- which after a restart it does not: every status it had reported was
-- held in memory and lost. This function returns the latest run of every
-- service with the payload of that run's latest event, and nothing else.
-- Like the functions in 000040 it must be created by a role that bypasses
-- RLS, or FORCE RLS would bind the definer and it would silently see nothing.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls)) THEN
        RAISE EXCEPTION 'recovery functions must be created by a role with SUPERUSER or BYPASSRLS (current role: %)', current_user;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION scos_recovery_latest_runs()
RETURNS jsonb
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT COALESCE(jsonb_agg(jsonb_build_object(
        'recovery_run_id', r.id::text,
        'service_id', r.service_id,
        'phase', r.phase,
        'checklist', r.checklist,
        'pending_outbox_count', r.pending_outbox_count,
        'recovery_started', to_char(r.recovery_started AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
        'recovery_ended', CASE WHEN r.recovery_ended IS NULL THEN '' ELSE to_char(r.recovery_ended AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') END,
        'latest_event', (SELECT e.payload FROM recovery_events e WHERE e.recovery_run_id = r.id ORDER BY e.sequence DESC LIMIT 1)
    ) ORDER BY r.service_id), '[]'::jsonb)
    FROM (
        SELECT DISTINCT ON (service_id) *
        FROM recovery_runs
        ORDER BY service_id, recovery_started DESC
    ) r
$$;

REVOKE ALL ON FUNCTION scos_recovery_latest_runs() FROM PUBLIC;

COMMIT;
