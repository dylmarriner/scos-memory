BEGIN;

-- P5 prerequisite: capture observable memory-state transitions from this
-- migration forward. This is NOT a historical belief reconstruction API:
-- transaction/clock timestamps are not commit-order timestamps, and facts
-- written before this migration have no reconstructible earlier state.
-- Hard deletion/offboarding is intentionally not instrumented yet: an AFTER
-- DELETE child insert could conflict with the tenant FK cascade and block
-- privacy offboarding. It requires separate recovery/purge qualification.
CREATE TABLE memory_system_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    memory_id uuid NOT NULL,
    namespace_id uuid NOT NULL,
    owner_principal_id uuid NOT NULL,
    memory_type memory_type NOT NULL,
    status memory_status NOT NULL,
    current_version_id uuid,
    superseded_by_id uuid,
    authority_level integer NOT NULL,
    confidence_score double precision NOT NULL,
    sensitivity sensitivity_class NOT NULL,
    valid_from timestamptz,
    valid_until timestamptz,
    expires_at timestamptz,
    operation text NOT NULL CHECK (operation IN ('baseline','insert','update')),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    transaction_id bigint NOT NULL DEFAULT txid_current()
);
CREATE INDEX memory_system_events_memory_idx
    ON memory_system_events (tenant_id, memory_id, id DESC);

ALTER TABLE memory_system_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_system_events FORCE ROW LEVEL SECURITY;

-- A historical event is readable only if the current memory is readable AND
-- the caller can read the event's historical namespace and sensitivity.
-- A deleted memory is not readable through this ledger.
CREATE POLICY memory_system_events_read ON memory_system_events FOR SELECT USING (
    tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
    AND EXISTS (
        SELECT 1 FROM memory_items i
        WHERE i.id = memory_system_events.memory_id
          AND i.tenant_id = memory_system_events.tenant_id
    )
    AND EXISTS (
        SELECT 1 FROM namespaces n
        JOIN namespace_memberships m ON m.namespace_id = n.id
        WHERE n.id = memory_system_events.namespace_id
          AND n.tenant_id = memory_system_events.tenant_id
          AND m.principal_id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
          AND (m.can_read OR m.can_administer)
          AND memory_system_events.sensitivity <= n.sensitivity_ceiling
    )
);

-- FORCE RLS also applies to the migration owner when not superuser. Only
-- that owner may write event rows; application principals cannot forge them.
DO $owner_policies$
DECLARE migration_owner name := current_user;
BEGIN
    EXECUTE format('CREATE POLICY memory_system_events_owner_insert ON memory_system_events FOR INSERT TO %I WITH CHECK (true)', migration_owner);
    EXECUTE format('CREATE POLICY memory_system_events_owner_read ON memory_system_events FOR SELECT TO %I USING (true)', migration_owner);
END
$owner_policies$;

REVOKE ALL ON memory_system_events FROM PUBLIC;
REVOKE ALL ON SEQUENCE memory_system_events_id_seq FROM PUBLIC;
-- Ordinary mutations remain forbidden. Privileged tenant offboarding must
-- first create an audited purge record in the SAME transaction.
CREATE TABLE memory_system_event_purge_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    operator_name name NOT NULL,
    reason text NOT NULL CHECK (char_length(btrim(reason)) BETWEEN 16 AND 2000),
    event_count bigint NOT NULL CHECK (event_count >= 0),
    transaction_id bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, transaction_id)
);
REVOKE ALL ON memory_system_event_purge_audit FROM PUBLIC;
CREATE TRIGGER memory_system_event_purge_audit_append_only
    BEFORE UPDATE OR DELETE ON memory_system_event_purge_audit
    FOR EACH ROW EXECUTE FUNCTION reject_authoritative_history_mutation();

CREATE FUNCTION guard_memory_system_event_purge() RETURNS trigger
LANGUAGE plpgsql AS $guard$
BEGIN
    IF TG_OP = 'DELETE'
       AND current_user = pg_get_userbyid(
           (SELECT c.relowner FROM pg_class c WHERE c.oid = TG_RELID))
       AND EXISTS (
           SELECT 1 FROM memory_system_event_purge_audit a
           WHERE a.tenant_id = OLD.tenant_id
             AND a.transaction_id = txid_current()
       ) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'memory system events are append-only; use privileged audited tenant purge'
        USING ERRCODE = '55006';
END;
$guard$;

CREATE TRIGGER memory_system_events_append_only
    BEFORE UPDATE OR DELETE ON memory_system_events
    FOR EACH ROW EXECUTE FUNCTION guard_memory_system_event_purge();

DO $purge_owner$
DECLARE migration_owner name := current_user;
BEGIN
    EXECUTE format('CREATE POLICY memory_system_events_owner_purge ON memory_system_events FOR DELETE TO %I USING (true)', migration_owner);
END
$purge_owner$;

CREATE FUNCTION purge_memory_system_events_for_tenant(
    p_tenant_id uuid, p_reason text
) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER
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
    -- FK-backed event INSERTs take KEY SHARE on the tenant. FOR UPDATE
    -- serializes them with the audit count and the subsequent deletion.
    PERFORM 1 FROM tenants WHERE id = p_tenant_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'tenant does not exist' USING ERRCODE = '22023';
    END IF;
    SELECT count(*) INTO expected_count
    FROM memory_system_events WHERE tenant_id = p_tenant_id;
    INSERT INTO memory_system_event_purge_audit
        (tenant_id, operator_name, reason, event_count, transaction_id)
    VALUES (p_tenant_id, session_user, btrim(p_reason),
            expected_count, txid_current());
    DELETE FROM memory_system_events WHERE tenant_id = p_tenant_id;
    GET DIAGNOSTICS deleted_count = ROW_COUNT;
    IF deleted_count <> expected_count THEN
        RAISE EXCEPTION 'memory event purge count mismatch; rolling back'
            USING ERRCODE = '40001';
    END IF;
    RETURN deleted_count;
END;
$purge$;

REVOKE ALL ON FUNCTION purge_memory_system_events_for_tenant(uuid,text)
    FROM PUBLIC;


-- Earlier ordered migrations already grant the non-superuser migration
-- owner context-free FORCE-RLS SELECT on memory_items (000064:
-- revalidation_owner_read_facts), and SELECT/UPDATE on tenants (000071:
-- revalidation_purge_owner_tenant_read/lock). Both policies are required
-- for the baseline and the privileged purge's tenant FOR UPDATE lock.
-- They remain owner-only; do not grant these bypasses to scos_app.
--
-- A concurrent writer must not commit between the baseline snapshot and
-- trigger installation. SHARE ROW EXCLUSIVE conflicts with DML's ROW
-- EXCLUSIVE lock, and is held through the end of this transaction.
LOCK TABLE memory_items IN SHARE ROW EXCLUSIVE MODE;

-- Explicitly mark pre-existing memory rows as a baseline at migration time.
-- Never backdate the event to memory_items.created_at: that would fabricate
-- historical status/approval evidence that was not recorded at creation.
INSERT INTO memory_system_events (
    tenant_id, memory_id, namespace_id, owner_principal_id, memory_type, status,
    current_version_id, superseded_by_id, authority_level,
    confidence_score, sensitivity, valid_from, valid_until, expires_at,
    operation
)
SELECT tenant_id, id, namespace_id, owner_principal_id, type, status, current_version_id,
       superseded_by_id, authority_level, confidence_score, sensitivity,
       valid_from, valid_until, expires_at, 'baseline'
FROM memory_items;

CREATE FUNCTION record_memory_system_event() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $event$
DECLARE
    item_row record;
    event_op text;
BEGIN
    IF TG_OP = 'UPDATE'
       AND ROW(NEW.tenant_id, NEW.namespace_id, NEW.owner_principal_id, NEW.type, NEW.status, NEW.current_version_id,
               NEW.superseded_by_id, NEW.authority_level, NEW.confidence_score,
               NEW.sensitivity, NEW.valid_from, NEW.valid_until, NEW.expires_at)
           IS NOT DISTINCT FROM
           ROW(OLD.tenant_id, OLD.namespace_id, OLD.owner_principal_id, OLD.type, OLD.status, OLD.current_version_id,
               OLD.superseded_by_id, OLD.authority_level, OLD.confidence_score,
               OLD.sensitivity, OLD.valid_from, OLD.valid_until, OLD.expires_at)
    THEN
        RETURN NEW;
    END IF;

    item_row := NEW;
    event_op := lower(TG_OP);

    -- Schema-qualify dynamically so isolated integration schemas work too.
    -- %I is an identifier escape; data remains bound through USING.
    EXECUTE format(
        'INSERT INTO %I.memory_system_events (
            tenant_id, memory_id, namespace_id, owner_principal_id, memory_type, status,
            current_version_id, superseded_by_id, authority_level,
            confidence_score, sensitivity, valid_from, valid_until, expires_at,
            operation
         ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)',
        TG_TABLE_SCHEMA
    ) USING item_row.tenant_id, item_row.id, item_row.namespace_id,
            item_row.owner_principal_id, item_row.type, item_row.status, item_row.current_version_id,
            item_row.superseded_by_id, item_row.authority_level,
            item_row.confidence_score, item_row.sensitivity,
            item_row.valid_from, item_row.valid_until, item_row.expires_at,
            event_op;
    RETURN NEW;
END;
$event$;

REVOKE ALL ON FUNCTION record_memory_system_event() FROM PUBLIC;
CREATE TRIGGER memory_items_system_event
    AFTER INSERT OR UPDATE ON memory_items
    FOR EACH ROW EXECUTE FUNCTION record_memory_system_event();

DO $runtime_grants$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname='scos_app') THEN
        GRANT SELECT ON memory_system_events TO scos_app;
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON memory_system_events FROM scos_app;
        REVOKE ALL ON memory_system_event_purge_audit FROM scos_app;
        REVOKE ALL ON FUNCTION purge_memory_system_events_for_tenant(uuid,text) FROM scos_app;
    END IF;
END
$runtime_grants$;

COMMIT;
