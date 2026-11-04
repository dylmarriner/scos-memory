BEGIN;

-- Revalidation decisions are append-only review evidence. A caller with INSERT
-- permission must not be able to forge its audit timestamp by supplying an
-- arbitrary created_at value. Existing rows are intentionally not rewritten:
-- this migration cannot infer their true insertion or commit time.
--
-- clock_timestamp() records the database time when the row is inserted. It is
-- NOT a transaction commit timestamp and does not enable as_of_system_time.
ALTER TABLE memory_revalidation_decisions
    ALTER COLUMN created_at SET DEFAULT clock_timestamp();

CREATE FUNCTION set_memory_revalidation_decision_recorded_at()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $record_time$
BEGIN
    NEW.created_at := clock_timestamp();
    RETURN NEW;
END;
$record_time$;

REVOKE ALL ON FUNCTION set_memory_revalidation_decision_recorded_at() FROM PUBLIC;

CREATE TRIGGER memory_revalidation_decisions_record_time_guard
    BEFORE INSERT ON memory_revalidation_decisions
    FOR EACH ROW
    EXECUTE FUNCTION set_memory_revalidation_decision_recorded_at();

COMMIT;
