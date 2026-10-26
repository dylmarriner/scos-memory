-- P3: online index matching immutable event keyset pagination.
-- The migration runner applies each SQL file via psql -f, without an outer
-- transaction; CONCURRENTLY avoids blocking writes on populated ledgers.
-- Never wrap this migration in BEGIN/COMMIT.
CREATE INDEX CONCURRENTLY IF NOT EXISTS code_snapshot_change_events_snapshot_path_c_idx
    ON code_snapshot_change_events (snapshot_id, path COLLATE "C");
