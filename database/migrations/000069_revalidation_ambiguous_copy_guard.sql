-- P7: A copied successor must not masquerade as a safe rename.
-- Replacing the activation trigger function leaves prior immutable events intact.
BEGIN;

CREATE OR REPLACE FUNCTION record_memory_code_revalidation()
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
    -- A removed original with two identical successor copies is ambiguous,
    -- even when the indexer reuses the old stable symbol ID for one copy.
    CROSS JOIN LATERAL (
        SELECT count(*) AS n
        FROM code_symbol_versions v
        WHERE v.snapshot_id = NEW.id
          AND v.kind = oldv.kind AND v.content_hash = oldv.content_hash
    ) AS new_duplicates
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
                AND new_duplicates.n = 1
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

COMMIT;
