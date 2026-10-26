-- P3: match the immutable snapshot-event API's bytewise keyset pagination.
-- The default database collation is not guaranteed to be C.
BEGIN;
CREATE INDEX IF NOT EXISTS code_snapshot_change_events_snapshot_path_c_idx
    ON code_snapshot_change_events (snapshot_id, path COLLATE "C");
COMMIT;
