BEGIN;

-- The row-level UPDATE/DELETE guards in 000082 do not fire on TRUNCATE.
-- Even the migration owner must use the reasoned, count-checked tenant purge
-- function rather than silently discarding append-only history. This also
-- prevents TRUNCATE ... CASCADE from bypassing the ledger's deletion guard.
CREATE FUNCTION reject_memory_system_history_truncate() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $guard$
BEGIN
    RAISE EXCEPTION 'memory system history is append-only; TRUNCATE is forbidden'
        USING ERRCODE = '55006';
END;
$guard$;
REVOKE ALL ON FUNCTION reject_memory_system_history_truncate() FROM PUBLIC;

CREATE TRIGGER memory_system_events_reject_truncate
    BEFORE TRUNCATE ON memory_system_events
    FOR EACH STATEMENT EXECUTE FUNCTION reject_memory_system_history_truncate();

CREATE TRIGGER memory_system_event_purge_audit_reject_truncate
    BEFORE TRUNCATE ON memory_system_event_purge_audit
    FOR EACH STATEMENT EXECUTE FUNCTION reject_memory_system_history_truncate();

COMMIT;
