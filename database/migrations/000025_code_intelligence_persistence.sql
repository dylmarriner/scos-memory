BEGIN;

-- Durable code-intelligence store (SCOS_MEMORY_TECHNICAL_BLUEPRINT §10, §33).
-- Replaces the in-memory maps / whole-state JSON file
-- (services/code-intelligence/cmd/server/main.go's codeStore) as the
-- canonical persistence layer. Two structural gaps that store cannot
-- express are the reason for the schema shape below, not just durability:
--
--   1. Symbol/file identity there is a hash of the *path*, so a rename
--      produces an unrelated ID with zero lineage to the old one (see
--      TestBaselineRenameLosesSymbolLineage). Here, code_symbols /
--      code_files hold the stable conceptual identity across renames;
--      code_symbol_versions / code_file_versions hold what that identity
--      looked like in one snapshot, linked by renamed_from_* when a rename
--      is detected.
--
--   2. A Repository has one mutable commit_id/index_generation, so indexing
--      a second commit destroys the first's state -- two branches or two
--      points in history can never coexist (see
--      TestBaselineReindexDestroysPriorCommitState). Here,
--      code_repository_snapshots is an immutable, append-only generation
--      per (branch, commit, extractor, config); code_repositories.
--      active_snapshot_id is the only mutable pointer, matching blueprint
--      §8's "an active snapshot is immutable; a new commit produces
--      another snapshot."
--
-- Chunks, embeddings, a durable job queue and impact-prediction training
-- data (blueprint §10's code_chunks/code_embeddings/indexing_jobs/
-- impact_predictions) are deliberately out of scope for this migration:
-- code_chunks is included because the existing CodeQueryResult API already
-- returns one per symbol, but the async job queue and embeddings are a
-- separate phase (P0-008, P1-015) with their own worker/model concerns.

CREATE TABLE IF NOT EXISTS code_repositories (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    canonical_path text NOT NULL,
    remote_identity text,
    default_branch text NOT NULL DEFAULT 'main',
    language_set text[] NOT NULL DEFAULT '{}',
    configuration_hash text,
    active_snapshot_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, canonical_path)
);

CREATE TYPE code_snapshot_status AS ENUM (
    'queued', 'scanning', 'parsing', 'indexing', 'validating', 'ready', 'active', 'failed', 'superseded'
);

-- An active snapshot is immutable: once status reaches 'active' no row here
-- is ever updated in place except to move a later snapshot to 'active' and
-- this one to 'superseded' (see activate_code_snapshot below). The unique
-- constraint is the actual fix for the Graphify-class staleness bug applied
-- to the primary store: the same (branch, commit, extractor, config) can
-- never silently produce two different generations.
CREATE TABLE IF NOT EXISTS code_repository_snapshots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES code_repositories(id) ON DELETE CASCADE,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    branch text NOT NULL,
    commit_id text NOT NULL,
    parent_snapshot_id uuid REFERENCES code_repository_snapshots(id),
    extractor_version text NOT NULL DEFAULT 'heuristic-v1',
    configuration_hash text NOT NULL DEFAULT '',
    status code_snapshot_status NOT NULL DEFAULT 'queued',
    file_count integer NOT NULL DEFAULT 0,
    symbol_count integer NOT NULL DEFAULT 0,
    error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    UNIQUE (repository_id, branch, commit_id, extractor_version, configuration_hash)
);
CREATE INDEX IF NOT EXISTS code_repository_snapshots_repo_branch_idx ON code_repository_snapshots (repository_id, branch, status);

ALTER TABLE code_repositories
    DROP CONSTRAINT IF EXISTS code_repositories_active_snapshot_fk;
ALTER TABLE code_repositories
    ADD CONSTRAINT code_repositories_active_snapshot_fk
    FOREIGN KEY (active_snapshot_id) REFERENCES code_repository_snapshots(id);

-- Stable conceptual file identity, independent of path.
CREATE TABLE IF NOT EXISTS code_files (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES code_repositories(id) ON DELETE CASCADE,
    first_snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS code_file_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id uuid NOT NULL REFERENCES code_files(id) ON DELETE CASCADE,
    snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id) ON DELETE CASCADE,
    path text NOT NULL,
    language text,
    content_hash text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    renamed_from_file_id uuid REFERENCES code_files(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (snapshot_id, path)
);
CREATE INDEX IF NOT EXISTS code_file_versions_snapshot_idx ON code_file_versions (snapshot_id);
CREATE INDEX IF NOT EXISTS code_file_versions_file_idx ON code_file_versions (file_id);
CREATE INDEX IF NOT EXISTS code_file_versions_content_hash_idx ON code_file_versions (content_hash);

-- Stable conceptual symbol identity, independent of path/FQN, so a rename
-- or move can be linked to what it used to be rather than looking like an
-- unrelated deletion plus an unrelated addition.
CREATE TABLE IF NOT EXISTS code_symbols (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES code_repositories(id) ON DELETE CASCADE,
    stable_key text NOT NULL,
    kind text NOT NULL,
    language text,
    first_snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repository_id, stable_key)
);

CREATE TABLE IF NOT EXISTS code_symbol_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol_id uuid NOT NULL REFERENCES code_symbols(id) ON DELETE CASCADE,
    snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id) ON DELETE CASCADE,
    file_version_id uuid REFERENCES code_file_versions(id),
    fully_qualified_name text NOT NULL,
    path text NOT NULL,
    kind text NOT NULL,
    range text,
    signature text,
    content_hash text NOT NULL,
    commit_id text,
    status text NOT NULL DEFAULT 'active',
    renamed_from_symbol_id uuid REFERENCES code_symbols(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (snapshot_id, symbol_id)
);
CREATE INDEX IF NOT EXISTS code_symbol_versions_snapshot_idx ON code_symbol_versions (snapshot_id);
CREATE INDEX IF NOT EXISTS code_symbol_versions_fqn_idx ON code_symbol_versions (snapshot_id, fully_qualified_name);
CREATE INDEX IF NOT EXISTS code_symbol_versions_path_idx ON code_symbol_versions (snapshot_id, path);
CREATE INDEX IF NOT EXISTS code_symbol_versions_content_hash_idx ON code_symbol_versions (content_hash);

CREATE TABLE IF NOT EXISTS code_chunks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id) ON DELETE CASCADE,
    content text,
    path text,
    range text,
    content_hash text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (symbol_version_id)
);

-- Confidence class distinguishes deterministic parser output from
-- heuristic/model-inferred edges (blueprint §2.4, §9.3 in the competitive
-- audit); the current heuristic extractor only ever produces 'heuristic'
-- edges, but the column exists now so a Tier 1/2 semantic indexer (SCIP/LSP)
-- can populate 'deterministic'/'static_analysis' rows into the same table
-- without a later migration.
CREATE TABLE IF NOT EXISTS code_edges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id) ON DELETE CASCADE,
    source_symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id) ON DELETE CASCADE,
    target_symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id) ON DELETE CASCADE,
    relation text NOT NULL,
    confidence_class text NOT NULL DEFAULT 'heuristic',
    confidence_score double precision NOT NULL DEFAULT 1.0,
    extractor text,
    source_location text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS code_edges_snapshot_idx ON code_edges (snapshot_id);
CREATE INDEX IF NOT EXISTS code_edges_source_idx ON code_edges (source_symbol_version_id);
CREATE INDEX IF NOT EXISTS code_edges_target_idx ON code_edges (target_symbol_version_id);

CREATE TABLE IF NOT EXISTS code_impact_reconciliations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repository_id uuid NOT NULL REFERENCES code_repositories(id) ON DELETE CASCADE,
    snapshot_id uuid REFERENCES code_repository_snapshots(id),
    commit_id text,
    predicted_symbols text[] NOT NULL DEFAULT '{}',
    actual_symbols text[] NOT NULL DEFAULT '{}',
    outcome text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS code_impact_reconciliations_repo_idx ON code_impact_reconciliations (repository_id);

-- Activates a snapshot atomically: the previous active snapshot for the
-- same (repository, branch) is marked 'superseded' and the new one becomes
-- both 'active' and the repository's active_snapshot_id, in one
-- transaction-scoped statement so no reader can ever observe two snapshots
-- active for the same branch, or the pointer updated without the row it
-- points to actually being active yet (blueprint §7's "no partially built
-- snapshot may become active").
CREATE OR REPLACE FUNCTION activate_code_snapshot(p_snapshot_id uuid) RETURNS void AS $$
DECLARE
    v_repository_id uuid;
    v_branch text;
BEGIN
    SELECT repository_id, branch INTO v_repository_id, v_branch
        FROM code_repository_snapshots WHERE id = p_snapshot_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'snapshot % does not exist', p_snapshot_id;
    END IF;

    UPDATE code_repository_snapshots
        SET status = 'superseded'
        WHERE repository_id = v_repository_id AND branch = v_branch AND status = 'active' AND id != p_snapshot_id;

    UPDATE code_repository_snapshots
        SET status = 'active', activated_at = now()
        WHERE id = p_snapshot_id;

    UPDATE code_repositories
        SET active_snapshot_id = p_snapshot_id, updated_at = now()
        WHERE id = v_repository_id;
END;
$$ LANGUAGE plpgsql;

COMMIT;
