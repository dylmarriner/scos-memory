-- P3 committed indexing evidence; inherits snapshot tenant RLS and lifecycle.
BEGIN;
ALTER TABLE code_repository_snapshots
    ADD COLUMN parsed_file_count integer NOT NULL DEFAULT 0 CHECK (parsed_file_count >= 0),
    ADD COLUMN reused_file_count integer NOT NULL DEFAULT 0 CHECK (reused_file_count >= 0),
    ADD COLUMN unparsed_file_count integer NOT NULL DEFAULT 0 CHECK (unparsed_file_count >= 0),
    ADD COLUMN parsed_declaration_count integer NOT NULL DEFAULT 0 CHECK (parsed_declaration_count >= 0),
    ADD COLUMN reused_declaration_count integer NOT NULL DEFAULT 0 CHECK (reused_declaration_count >= 0),
    ADD COLUMN cache_hit_ratio double precision NOT NULL DEFAULT 0 CHECK (cache_hit_ratio BETWEEN 0 AND 1),
    ADD COLUMN index_elapsed_ms bigint CHECK (index_elapsed_ms >= 0),
    ADD COLUMN extractor_tier_counts jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(extractor_tier_counts) = 'object'),
    ADD CONSTRAINT code_snapshot_file_metrics_consistent
        CHECK (parsed_file_count + reused_file_count + unparsed_file_count <= file_count);
COMMIT;
