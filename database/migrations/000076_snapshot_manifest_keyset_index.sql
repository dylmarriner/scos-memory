-- P3: bounded stored-source manifest readback uses bytewise C ordering.
-- The existing locale-dependent (snapshot_id,path) unique index is not
-- guaranteed to support the C-collated keyset query. Build concurrently
-- to avoid blocking source indexing on existing installations.
-- The migration runner executes this file without an outer transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS code_file_versions_snapshot_path_c_idx
    ON code_file_versions (snapshot_id, path COLLATE "C");
