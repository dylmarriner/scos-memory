BEGIN;

-- Recovery step 4 ("Load latest checkpoints") only counted checkpoint rows
-- and reported them "loaded". This function loads the latest checkpoint of
-- every session and returns, for each, the facts recovery needs to decide
-- whether that checkpoint is a sound resume point:
--
--   * the turn it checkpoints still exists, and its status;
--   * how many of the turn's events 1..last_event_sequence are present
--     (a resume point is only sound if its event history is complete);
--   * whether the turn's turn.completed event names this checkpoint (the
--     journal writes both in one transaction, so a mismatch means the
--     checkpoint and the event log disagree);
--
-- and, for every interrupted turn, the latest checkpoint of its session,
-- which is where recovery of that turn resumes. Like the functions in
-- 000040 and 000050 it returns identifiers and counts, never content, and
-- must be created by a role that bypasses RLS, or FORCE RLS would bind the
-- definer and it would silently see nothing.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls)) THEN
        RAISE EXCEPTION 'recovery functions must be created by a role with SUPERUSER or BYPASSRLS (current role: %)', current_user;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION scos_recovery_checkpoints()
RETURNS jsonb
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH latest AS (
        SELECT DISTINCT ON (c.session_id) c.id, c.session_id, c.turn_id, c.last_event_sequence, c.created_at
        FROM checkpoints c
        ORDER BY c.session_id, c.created_at DESC, c.id DESC
    )
    SELECT jsonb_build_object(
        'checkpoints', COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                'checkpoint_id', l.id::text,
                'session_id', l.session_id::text,
                'turn_id', l.turn_id::text,
                'last_event_sequence', l.last_event_sequence,
                'created_at', to_char(l.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
                'turn_status', (SELECT t.status::text FROM turns t WHERE t.id = l.turn_id),
                'events_present', (SELECT count(*) FROM events e
                                    WHERE e.aggregate_id = l.turn_id
                                      AND e.aggregate_sequence BETWEEN 1 AND l.last_event_sequence),
                'completion_event_matches', EXISTS (SELECT 1 FROM events e
                                    WHERE e.aggregate_id = l.turn_id
                                      AND e.event_type = 'turn.completed'
                                      AND e.data->>'checkpoint_id' = l.id::text)
            ) ORDER BY l.session_id)
            FROM latest l), '[]'::jsonb),
        'resume_points', COALESCE((
            SELECT jsonb_agg(jsonb_build_object(
                'turn_id', t.id::text,
                'session_id', t.session_id::text,
                'checkpoint_id', (SELECT l.id::text FROM latest l WHERE l.session_id = t.session_id)
            ) ORDER BY t.id)
            FROM turns t WHERE t.status IN ('running', 'interrupted')), '[]'::jsonb)
    )
$$;

REVOKE ALL ON FUNCTION scos_recovery_checkpoints() FROM PUBLIC;

-- Only the runtime service role may run it (guarded like policies/000015
-- for deployments that provision scos_app elsewhere).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION scos_recovery_checkpoints() TO scos_app;
    END IF;
END
$$;

COMMIT;
