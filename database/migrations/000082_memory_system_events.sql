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
CREATE TRIGGER memory_system_events_append_only
    BEFORE UPDATE OR DELETE ON memory_system_events
    FOR EACH ROW EXECUTE FUNCTION reject_authoritative_history_mutation();

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
    END IF;
END
$runtime_grants$;

COMMIT;
