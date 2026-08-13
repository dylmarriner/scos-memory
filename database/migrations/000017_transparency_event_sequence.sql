BEGIN;

-- A stream-wide cursor is required for resumable, tenant-scoped transparency
-- streams. Aggregate sequence numbers are not sufficient because they reset
-- for every aggregate.
CREATE SEQUENCE IF NOT EXISTS event_stream_sequence;

ALTER TABLE events ADD COLUMN IF NOT EXISTS stream_sequence bigint;

-- events_append_only (000007_append_only_authoritative_history.sql) rejects
-- any UPDATE on this table, including this one-time backfill of a brand new
-- column on rows that predate it. Disabling the trigger only for the
-- duration of this migration's own transaction is what actually needs the
-- bypass; it does not touch the guard's behaviour for any application
-- traffic before or after.
ALTER TABLE events DISABLE TRIGGER events_append_only;
UPDATE events
SET stream_sequence = nextval('event_stream_sequence')
WHERE stream_sequence IS NULL;
ALTER TABLE events ENABLE TRIGGER events_append_only;

SELECT setval(
    'event_stream_sequence',
    GREATEST(COALESCE((SELECT MAX(stream_sequence) FROM events), 1), 1),
    (SELECT COUNT(*) FROM events) > 0
);

ALTER TABLE events
    ALTER COLUMN stream_sequence SET DEFAULT nextval('event_stream_sequence'),
    ALTER COLUMN stream_sequence SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS events_stream_sequence_uidx
    ON events (stream_sequence);

COMMIT;
