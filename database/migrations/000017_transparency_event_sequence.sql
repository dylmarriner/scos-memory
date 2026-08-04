BEGIN;

-- A stream-wide cursor is required for resumable, tenant-scoped transparency
-- streams. Aggregate sequence numbers are not sufficient because they reset
-- for every aggregate.
CREATE SEQUENCE IF NOT EXISTS event_stream_sequence;

ALTER TABLE events ADD COLUMN IF NOT EXISTS stream_sequence bigint;

UPDATE events
SET stream_sequence = nextval('event_stream_sequence')
WHERE stream_sequence IS NULL;

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
