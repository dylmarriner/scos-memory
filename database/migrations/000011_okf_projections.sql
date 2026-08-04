BEGIN;

-- GOVERNANCE.md §26 requires PostgreSQL to store indexed OKF projections
-- (commit hash, path, metadata, content hash, import status) rather than a
-- local file. §13 lists `okf_bundles` and `okf_concepts` as required tables.
-- OKF governing knowledge is shared platform-level reference content
-- (system/governance, public/reference scopes per §9), not tenant-partitioned,
-- so these tables are gated by authenticated-principal presence rather than
-- tenant identity.
--
-- 000002_governance_tables.sql already created an earlier `okf_concepts`
-- and `okf_bundles` shape (uuid-keyed, tenant/bundle-relational) that
-- were never populated by any running service -- services/okf-synchroniser
-- was built against the richer, text-id, commit/status-tracking shape
-- defined below. Since no deployment has ever written through the 000002
-- shape, this migration supersedes it here rather than editing the
-- already-applied 000002 file in place, keeping migrations forward-only
-- per GOVERNANCE.md §12.
DROP TABLE IF EXISTS okf_concepts;
DROP TABLE IF EXISTS okf_bundles;

CREATE TABLE okf_concepts (
    id text PRIMARY KEY,
    path text NOT NULL UNIQUE,
    category text NOT NULL DEFAULT 'general',
    title text NOT NULL DEFAULT '',
    content text NOT NULL DEFAULT '',
    content_hash text NOT NULL,
    commit_hash text NOT NULL DEFAULT '',
    import_status text NOT NULL DEFAULT 'synced',
    status text NOT NULL DEFAULT 'current',
    imported_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX okf_concepts_category_idx ON okf_concepts (category);
CREATE INDEX okf_concepts_status_idx ON okf_concepts (status);

CREATE TABLE okf_bundles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    commit_hash text NOT NULL,
    synced_at timestamptz NOT NULL DEFAULT now(),
    added_count integer NOT NULL DEFAULT 0,
    updated_count integer NOT NULL DEFAULT 0,
    removed_count integer NOT NULL DEFAULT 0,
    diverged_count integer NOT NULL DEFAULT 0,
    added_paths jsonb NOT NULL DEFAULT '[]'::jsonb,
    updated_paths jsonb NOT NULL DEFAULT '[]'::jsonb,
    removed_paths jsonb NOT NULL DEFAULT '[]'::jsonb,
    diverged_paths jsonb NOT NULL DEFAULT '[]'::jsonb,
    errors jsonb NOT NULL DEFAULT '[]'::jsonb
);

CREATE INDEX okf_bundles_commit_hash_idx ON okf_bundles (commit_hash);
CREATE INDEX okf_bundles_synced_at_idx ON okf_bundles (synced_at);

ALTER TABLE okf_concepts ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf_bundles ENABLE ROW LEVEL SECURITY;

CREATE POLICY okf_concepts_authenticated_access ON okf_concepts
    FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL)
    WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);

CREATE POLICY okf_bundles_authenticated_access ON okf_bundles
    FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL)
    WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);

-- okf_bundles is an append-only sync-run audit log.
REVOKE UPDATE, DELETE ON okf_bundles FROM PUBLIC;

DROP TRIGGER IF EXISTS okf_bundles_append_only ON okf_bundles;
CREATE TRIGGER okf_bundles_append_only
    BEFORE UPDATE OR DELETE ON okf_bundles
    FOR EACH ROW
    EXECUTE FUNCTION reject_authoritative_history_mutation();

COMMIT;
