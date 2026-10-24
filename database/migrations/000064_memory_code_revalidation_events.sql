BEGIN;

-- P7: preserve evidence that a code change may invalidate linked facts.
-- No memory_items or memory_versions rows are modified by this subsystem.
-- Every event identifies immutable old/new snapshots and code versions.
CREATE TABLE memory_revalidation_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    fact_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    link_id uuid NOT NULL REFERENCES memory_code_links(id) ON DELETE CASCADE,
    old_snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id),
    new_snapshot_id uuid NOT NULL REFERENCES code_repository_snapshots(id),
    old_symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id),
    new_symbol_version_id uuid REFERENCES code_symbol_versions(id),
    continuity text NOT NULL CHECK (continuity IN ('renamed', 'changed', 'deleted', 'ambiguous')),
    review_status text NOT NULL CHECK (review_status IN (
        'still_valid', 'needs_review', 'superseded', 'invalidated', 'unresolved'
    )),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (link_id, new_snapshot_id),
    CHECK (old_snapshot_id <> new_snapshot_id)
);
CREATE INDEX memory_revalidation_events_fact_idx
    ON memory_revalidation_events (tenant_id, fact_id, created_at DESC);
CREATE INDEX memory_revalidation_events_review_idx
    ON memory_revalidation_events (tenant_id, review_status, created_at DESC);
CREATE INDEX memory_revalidation_events_snapshot_idx
    ON memory_revalidation_events (new_snapshot_id);

ALTER TABLE memory_revalidation_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_revalidation_events FORCE ROW LEVEL SECURITY;
-- No INSERT/UPDATE/DELETE policy is exposed to the application role.
-- Event records are produced only by the privileged schema-owned trigger.
-- SELECT also respects existing item/namespace read-access policies.
CREATE POLICY memory_revalidation_events_read ON memory_revalidation_events
    FOR SELECT USING (
        tenant_id = NULLIF(current_setting('scos.tenant_id', true), '')::uuid
        AND EXISTS (SELECT 1 FROM memory_items i WHERE i.id = fact_id)
    );

-- Called in the exact transaction that changes an immutable snapshot to
-- active. Failure rolls back activation: a code change never silently loses
-- its associated review work. This trigger observes the branch-local parent
-- snapshot, not code_repositories.active_snapshot_id, which is global.
CREATE FUNCTION record_memory_code_revalidation()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
    IF NEW.parent_snapshot_id IS NULL THEN
        RETURN NEW;
    END IF;
    -- Walk parent ancestry: a fact may be linked to the symbol version
    -- from several unchanged commits ago. Only ancestors of this branch are
    -- eligible. The new snapshot's immediate parent is not enough.
    WITH RECURSIVE ancestors AS (
        SELECT s.id, s.parent_snapshot_id
        FROM code_repository_snapshots s
        WHERE s.id = NEW.parent_snapshot_id
          AND s.repository_id = NEW.repository_id
          AND s.branch = NEW.branch
        UNION ALL
        SELECT parent.id, parent.parent_snapshot_id
        FROM code_repository_snapshots parent
        JOIN ancestors a ON parent.id = a.parent_snapshot_id
        WHERE parent.repository_id = NEW.repository_id
          AND parent.branch = NEW.branch
    )
    INSERT INTO memory_revalidation_events (
        tenant_id, fact_id, link_id,
        old_snapshot_id, new_snapshot_id,
        old_symbol_version_id, new_symbol_version_id,
        continuity, review_status
    )
    SELECT
        l.tenant_id, l.fact_id, l.id,
        oldv.snapshot_id, NEW.id,
        oldv.id, kind.new_symbol_version_id,
        kind.continuity,
        CASE WHEN kind.continuity = 'renamed'
             THEN 'still_valid' ELSE 'needs_review' END
    FROM memory_code_links AS l
    JOIN memory_items AS item ON item.id = l.fact_id AND item.tenant_id = NEW.tenant_id
    JOIN code_symbol_versions AS oldv ON oldv.id = l.code_symbol_version_id
    -- An exact matching file/path is authoritative for path continuity.
    -- Content-derived stable symbols can be reassigned when duplicates exist;
    -- they must never override a changed file that still occupies oldv.path.
    LEFT JOIN LATERAL (
        SELECT v.id, v.path, v.content_hash
        FROM code_symbol_versions v
        WHERE v.snapshot_id = NEW.id
          AND v.path = oldv.path
          AND v.kind = oldv.kind
          AND v.fully_qualified_name = oldv.fully_qualified_name
        LIMIT 1
    ) AS location ON TRUE
    CROSS JOIN LATERAL (
        SELECT count(*) AS n
        FROM code_symbol_versions v
        WHERE v.snapshot_id = oldv.snapshot_id
          AND v.kind = oldv.kind AND v.content_hash = oldv.content_hash
    ) AS old_duplicates
    CROSS JOIN LATERAL (
        SELECT count(*) AS n
        FROM code_symbol_versions v
        WHERE v.snapshot_id = NEW.id
          AND v.path = oldv.path AND v.kind = oldv.kind
    ) AS same_path_count
    LEFT JOIN LATERAL (
        SELECT v.id, v.path, v.content_hash
        FROM code_symbol_versions AS v
        WHERE v.snapshot_id = NEW.id AND v.symbol_id = oldv.symbol_id
        LIMIT 1
    ) AS stable ON TRUE
    LEFT JOIN LATERAL (
        SELECT v.id, v.path, v.content_hash
        FROM code_symbol_versions AS v
        WHERE v.snapshot_id = NEW.id
          AND v.fully_qualified_name = oldv.fully_qualified_name
          AND v.kind = oldv.kind
        ORDER BY v.id
        LIMIT 1
    ) AS named ON TRUE
    CROSS JOIN LATERAL (
        SELECT count(*) AS n
        FROM code_symbol_versions AS v
        WHERE v.snapshot_id = NEW.id
          AND v.fully_qualified_name = oldv.fully_qualified_name
          AND v.kind = oldv.kind
    ) AS named_count
    CROSS JOIN LATERAL (
        SELECT
            CASE
              WHEN location.id IS NOT NULL
                AND location.content_hash = oldv.content_hash THEN 'unchanged'
              WHEN location.id IS NOT NULL THEN 'changed'
              WHEN stable.id IS NOT NULL
                AND stable.content_hash = oldv.content_hash
                AND stable.path = oldv.path THEN 'unchanged'
              WHEN stable.id IS NOT NULL
                AND stable.content_hash = oldv.content_hash
                AND stable.path <> oldv.path
                AND old_duplicates.n = 1
                AND same_path_count.n = 0 THEN 'renamed'
              WHEN stable.id IS NOT NULL
                AND stable.content_hash = oldv.content_hash THEN 'ambiguous'
              WHEN stable.id IS NOT NULL THEN 'changed'
              WHEN named_count.n > 1 THEN 'ambiguous'
              WHEN named_count.n = 1 THEN 'changed'
              ELSE 'deleted'
            END AS continuity,
            COALESCE(location.id, stable.id,
                CASE WHEN named_count.n = 1 THEN named.id END
            ) AS new_symbol_version_id
    ) AS kind
    WHERE l.tenant_id = NEW.tenant_id
      AND item.status = 'active'
      AND oldv.snapshot_id IN (SELECT id FROM ancestors)
      AND kind.continuity <> 'unchanged'
    ON CONFLICT (link_id, new_snapshot_id) DO NOTHING;

    RETURN NEW;
END;
$$;

CREATE TRIGGER memory_code_revalidate_on_activation
AFTER UPDATE OF status ON code_repository_snapshots
FOR EACH ROW
WHEN (NEW.status = 'active' AND OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION record_memory_code_revalidation();

COMMIT;
