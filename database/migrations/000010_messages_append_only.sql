BEGIN;

-- `messages` was previously mutable in place: an unfinalized draft assistant row was
-- created by start_response and later overwritten by complete_turn. That made
-- it possible to silently replace committed content on a retried completion,
-- contradicting the append-only / correction-not-mutation principle already
-- enforced for events, message_chunks and memory_versions.
--
-- The draft-to-final transition (a single UPDATE while the row has
-- never been finalized) remains allowed, since the draft row was never a
-- settled fact. Once a message has been finalized, any further content
-- change must supersede it with a new row rather than mutate history.

ALTER TABLE messages
    ADD COLUMN finalized_at timestamptz,
    ADD COLUMN supersedes_message_id uuid REFERENCES messages(id);

CREATE OR REPLACE FUNCTION reject_finalized_message_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF OLD.finalized_at IS NOT NULL THEN
        RAISE EXCEPTION 'message % is finalized and is append-only; corrections must supersede it with a new row',
            OLD.id
            USING ERRCODE = '55006';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS messages_finalized_append_only ON messages;
CREATE TRIGGER messages_finalized_append_only
    BEFORE UPDATE ON messages
    FOR EACH ROW
    EXECUTE FUNCTION reject_finalized_message_mutation();

REVOKE DELETE ON messages FROM PUBLIC;

DROP TRIGGER IF EXISTS messages_no_delete ON messages;
CREATE TRIGGER messages_no_delete
    BEFORE DELETE ON messages
    FOR EACH ROW
    EXECUTE FUNCTION reject_authoritative_history_mutation();

CREATE INDEX messages_supersedes_idx
    ON messages (supersedes_message_id)
    WHERE supersedes_message_id IS NOT NULL;

COMMIT;
