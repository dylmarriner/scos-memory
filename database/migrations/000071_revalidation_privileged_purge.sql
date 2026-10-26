BEGIN;

-- Privileged tenant offboarding may need to remove decisions before their
-- parent events cascade. Ordinary reviews remain immutable. This ledger
-- deliberately has NO tenant foreign key so purge evidence survives removal
-- of the tenant it describes.
CREATE TABLE memory_revalidation_decision_purge_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    operator_name name NOT NULL,
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 16 AND 2000),
    decision_count bigint NOT NULL CHECK (decision_count >= 0),
    transaction_id bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, transaction_id)
);
CREATE INDEX memory_revalidation_decision_purge_audit_tenant_idx
    ON memory_revalidation_decision_purge_audit (tenant_id, created_at DESC);
REVOKE ALL ON memory_revalidation_decision_purge_audit FROM PUBLIC;
CREATE TRIGGER memory_revalidation_decision_purge_audit_append_only
    BEFORE UPDATE OR DELETE ON memory_revalidation_decision_purge_audit
    FOR EACH ROW EXECUTE FUNCTION reject_authoritative_history_mutation();

-- A decision may only be deleted by the migration owner in a transaction
-- that has already written a durable audit row for that tenant. A custom
-- GUC is NOT an authorization token: runtime clients can set arbitrary GUCs.
CREATE FUNCTION guard_revalidation_decision_purge()
RETURNS trigger
LANGUAGE plpgsql
AS $guard$
BEGIN
    IF TG_OP = 'DELETE'
       AND current_user = pg_get_userbyid(
           (SELECT c.relowner FROM pg_class c WHERE c.oid = TG_RELID))
       AND EXISTS (
           SELECT 1 FROM memory_revalidation_decision_purge_audit a
           WHERE a.tenant_id = OLD.tenant_id
             AND a.transaction_id = txid_current()
       ) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'revalidation decisions are append-only; use privileged audited tenant purge'
        USING ERRCODE = '55006';
END;
$guard$;

DROP TRIGGER memory_revalidation_decisions_append_only ON memory_revalidation_decisions;
CREATE TRIGGER memory_revalidation_decisions_append_only
    BEFORE UPDATE OR DELETE ON memory_revalidation_decisions
    FOR EACH ROW EXECUTE FUNCTION guard_revalidation_decision_purge();

-- FORCE RLS applies even to the migration owner. Only that trusted owner
-- receives the extra read/delete policies required by the purge function.
DO $owner_policies$
DECLARE
    migration_owner name := current_user;
BEGIN
    EXECUTE format(
        'CREATE POLICY revalidation_decision_owner_read ON memory_revalidation_decisions FOR SELECT TO %I USING (true)',
        migration_owner);
    EXECUTE format(
        'CREATE POLICY revalidation_decision_owner_purge ON memory_revalidation_decisions FOR DELETE TO %I USING (true)',
        migration_owner);
    -- The tenant row is locked FOR UPDATE to serialize FK-backed inserts.
    -- Forced RLS must not hide it from a non-superuser migration owner.
    EXECUTE format(
        'CREATE POLICY revalidation_purge_owner_tenant_read ON tenants FOR SELECT TO %I USING (true)',
        migration_owner);
    EXECUTE format(
        'CREATE POLICY revalidation_purge_owner_tenant_lock ON tenants FOR UPDATE TO %I USING (true) WITH CHECK (true)',
        migration_owner);
END;
$owner_policies$;

-- The procedure does NOT delete tenants. Offboarding must explicitly call
-- this as the migration owner in the SAME transaction as tenant deletion,
-- so any later FK failure rolls the decision purge and audit back together.
CREATE FUNCTION purge_memory_revalidation_decisions_for_tenant(
    p_tenant_id uuid, p_reason text
) RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $purge$
DECLARE
    expected_count bigint;
    deleted_count bigint;
BEGIN
    IF p_tenant_id IS NULL OR p_reason IS NULL
       OR char_length(btrim(p_reason)) NOT BETWEEN 16 AND 2000 THEN
        RAISE EXCEPTION 'tenant and 16-2000 character purge reason required'
            USING ERRCODE = '22023';
    END IF;

    -- FOR UPDATE blocks concurrent FK KEY SHARE locks from new decisions,
    -- making the recorded count stable until the transaction commits.
    PERFORM 1 FROM tenants WHERE id = p_tenant_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant does not exist' USING ERRCODE = '22023';
    END IF;

    SELECT count(*) INTO expected_count
    FROM memory_revalidation_decisions WHERE tenant_id = p_tenant_id;

    INSERT INTO memory_revalidation_decision_purge_audit
        (tenant_id, operator_name, reason, decision_count, transaction_id)
    VALUES (p_tenant_id, session_user, btrim(p_reason),
            expected_count, txid_current());

    DELETE FROM memory_revalidation_decisions WHERE tenant_id = p_tenant_id;
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    IF deleted_count <> expected_count THEN
        RAISE EXCEPTION 'decision purge count mismatch; rolling back'
            USING ERRCODE = '40001';
    END IF;
    RETURN deleted_count;
END;
$purge$;

REVOKE ALL ON FUNCTION purge_memory_revalidation_decisions_for_tenant(uuid,text)
    FROM PUBLIC;
DO $runtime_revokes$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        REVOKE ALL ON memory_revalidation_decision_purge_audit FROM scos_app;
        REVOKE ALL ON FUNCTION purge_memory_revalidation_decisions_for_tenant(uuid,text)
            FROM scos_app;
    END IF;
END;
$runtime_revokes$;

COMMIT;
