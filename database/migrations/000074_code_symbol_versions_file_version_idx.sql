-- P3: the unchanged-file declaration-copy path selects by file_version_id.
-- A missing index turns each copy into a scan of historical symbol versions.
-- The migration runner invokes each file through psql -f without an outer
-- transaction; CONCURRENTLY avoids blocking production writes at rollout.
CREATE INDEX CONCURRENTLY IF NOT EXISTS code_symbol_versions_file_version_idx
    ON code_symbol_versions (file_version_id);
