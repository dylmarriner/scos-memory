BEGIN;

-- Fabric-wide recovery inspection for the recovery orchestrator.
--
-- turns, operations, outbox_jobs, checkpoints and okf_bundles are all under
-- FORCE ROW LEVEL SECURITY keyed on a tenant or principal context. The
-- recovery orchestrator is a fabric operator, not a tenant: connected as the
-- restricted scos_app role with no tenant context, every direct query it ran
-- returned zero rows, so recovery always reported "nothing interrupted"
-- whatever the real state was. These SECURITY DEFINER functions expose
-- exactly the cross-tenant facts recovery needs (identifiers and counts, no
-- content) and the one corrective action it takes, and nothing else.
--
-- The functions run as the migration-owning role, which must bypass RLS
-- (superuser or BYPASSRLS, as POSTGRES_USER is in the reference deployment);
-- otherwise FORCE RLS would bind the definer too and the functions would
-- silently see nothing, so this migration fails loudly instead.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = current_user AND (rolsuper OR rolbypassrls)) THEN
        RAISE EXCEPTION 'recovery functions must be created by a role with SUPERUSER or BYPASSRLS (current role: %)', current_user;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION scos_recovery_findings()
RETURNS jsonb
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT jsonb_build_object(
        'interrupted_turns', COALESCE((SELECT jsonb_agg(id::text ORDER BY id) FROM turns WHERE status IN ('running', 'interrupted')), '[]'::jsonb),
        'open_operations', COALESCE((SELECT jsonb_agg(id::text ORDER BY id) FROM operations WHERE status IN ('executing', 'unknown', 'reconciling')), '[]'::jsonb),
        'pending_outbox', (SELECT count(*) FROM outbox_jobs WHERE status IN ('pending', 'retryable') AND available_at <= now()),
        'stale_outbox_leases', (SELECT count(*) FROM outbox_jobs WHERE status = 'processing' AND lease_expires_at IS NOT NULL AND lease_expires_at < now()),
        'dead_letters', (SELECT count(*) FROM outbox_jobs WHERE status = 'dead_letter'),
        'checkpoint_count', (SELECT count(*) FROM checkpoints),
        'latest_checkpoint', (SELECT max(created_at) FROM checkpoints),
        'okf_latest', (SELECT jsonb_build_object('commit_hash', commit_hash, 'errors', errors, 'synced_at', synced_at)
                       FROM okf_bundles ORDER BY synced_at DESC LIMIT 1)
    )
$$;

-- Requeue outbox jobs whose worker lease expired while processing: the
-- worker crashed or was stopped mid-job. They become retryable immediately
-- with the lease cleared, so the workers resume them. Returns the count.
CREATE OR REPLACE FUNCTION scos_recovery_requeue_stale_outbox()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    requeued integer;
BEGIN
    UPDATE outbox_jobs
       SET status = 'retryable',
           lease_owner = NULL,
           lease_expires_at = NULL,
           available_at = now(),
           last_error = COALESCE(last_error || E'\n', '') || 'requeued by recovery: worker lease expired while processing',
           updated_at = now()
     WHERE status = 'processing'
       AND lease_expires_at IS NOT NULL
       AND lease_expires_at < now();
    GET DIAGNOSTICS requeued = ROW_COUNT;
    RETURN requeued;
END
$$;

REVOKE ALL ON FUNCTION scos_recovery_findings() FROM PUBLIC;
REVOKE ALL ON FUNCTION scos_recovery_requeue_stale_outbox() FROM PUBLIC;

COMMIT;
