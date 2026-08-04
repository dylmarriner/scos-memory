BEGIN;

-- These relations are authoritative history.  Application roles must append
-- new rows rather than rewrite or remove previously committed facts.
CREATE OR REPLACE FUNCTION reject_authoritative_history_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'authoritative history table % is append-only; % is not permitted',
        TG_TABLE_NAME, TG_OP
        USING ERRCODE = '55006';
END;
$$;

REVOKE UPDATE, DELETE ON events, message_chunks, memory_versions FROM PUBLIC;

DROP TRIGGER IF EXISTS events_append_only ON events;
CREATE TRIGGER events_append_only
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW
    EXECUTE FUNCTION reject_authoritative_history_mutation();

DROP TRIGGER IF EXISTS message_chunks_append_only ON message_chunks;
CREATE TRIGGER message_chunks_append_only
    BEFORE UPDATE OR DELETE ON message_chunks
    FOR EACH ROW
    EXECUTE FUNCTION reject_authoritative_history_mutation();

DROP TRIGGER IF EXISTS memory_versions_append_only ON memory_versions;
CREATE TRIGGER memory_versions_append_only
    BEFORE UPDATE OR DELETE ON memory_versions
    FOR EACH ROW
    EXECUTE FUNCTION reject_authoritative_history_mutation();

ALTER TABLE memory_versions
    ADD CONSTRAINT memory_versions_version_number_positive
    CHECK (version_number > 0),
    ADD CONSTRAINT memory_versions_previous_version_not_self
    CHECK (previous_version_id IS NULL OR previous_version_id <> id);

CREATE INDEX memory_versions_previous_version_idx
    ON memory_versions (previous_version_id)
    WHERE previous_version_id IS NOT NULL;

COMMIT;
