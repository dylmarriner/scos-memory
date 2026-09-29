BEGIN;

-- The consolidation worker wrote memory_versions.structured_content with
-- only {title, evidence}. memory-intelligence decodes every fact's network,
-- source, scope and observed_at from structured_content (store.go), so each
-- consolidated memory recalled with those fields blank and was invisible to
-- its network/scope-keyed contradiction checks. The worker now writes the
-- full contract (workers/consolidation promoter.structured_content); this
-- repairs the versions it wrote before.
--
-- memory_versions is append-only (memory_versions_append_only trigger), so
-- the repair appends a successor version carrying the same content and the
-- completed structured_content, and repoints current_version_id at it; the
-- original version stays in history untouched.
--
-- This migration runs as the table owner, which FORCE ROW LEVEL SECURITY
-- (policies/000012) subjects to tenant policies it has no context for: the
-- statements below would silently match no rows. Forcing is lifted only
-- inside this transaction -- the ALTERs take ACCESS EXCLUSIVE locks held to
-- COMMIT, so no other session ever observes the relaxed state -- and each
-- table is returned to exactly the state it was found in (on a fresh
-- install migrations run before policies/000012 has forced anything).
DO $$
DECLARE
    items_forced boolean;
    versions_forced boolean;
BEGIN
    SELECT relforcerowsecurity INTO items_forced FROM pg_class WHERE oid = 'memory_items'::regclass;
    SELECT relforcerowsecurity INTO versions_forced FROM pg_class WHERE oid = 'memory_versions'::regclass;
    ALTER TABLE memory_items NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE memory_versions NO FORCE ROW LEVEL SECURITY;

WITH targets AS (
    SELECT mi.id AS memory_id, mi.type, mi.valid_from, mi.valid_until, mi.created_at,
           mi.owner_principal_id, mv.id AS version_id, mv.version_number,
           mv.content, mv.content_hash, mv.structured_content
      FROM memory_items mi
      JOIN memory_versions mv ON mv.id = mi.current_version_id
     WHERE mv.change_reason = 'initial consolidation'
       AND NOT (COALESCE(mv.structured_content, '{}'::jsonb) ? 'network')
), successors AS (
    INSERT INTO memory_versions
        (memory_id, version_number, content, structured_content, content_hash,
         change_reason, previous_version_id, created_by)
    SELECT memory_id,
           version_number + 1,
           content,
           COALESCE(structured_content, '{}'::jsonb) || jsonb_build_object(
               -- inverse of memory-intelligence's factNetworkToMemoryType
               'network', CASE type
                   WHEN 'episodic' THEN 'experience'
                   WHEN 'working' THEN 'experience'
                   WHEN 'procedural' THEN 'procedure'
                   WHEN 'revocation' THEN 'negative'
                   WHEN 'artifact_reference' THEN 'entity'
                   ELSE 'world'
               END,
               'source', 'observed',
               'scope', 'global',
               'observed_at', COALESCE(to_json(COALESCE(valid_from, created_at)) #>> '{}', ''),
               'valid_from', COALESCE(to_json(valid_from) #>> '{}', ''),
               'valid_until', COALESCE(to_json(valid_until) #>> '{}', ''),
               'inferred', false,
               'derivation', 'consolidated from a conversation turn'
           ),
           content_hash,
           'metadata backfill: structured_content contract (000041)',
           version_id,
           owner_principal_id
      FROM targets
    RETURNING id, memory_id
)
UPDATE memory_items mi
   SET current_version_id = s.id
  FROM successors s
 WHERE mi.id = s.memory_id;

    IF items_forced THEN
        ALTER TABLE memory_items FORCE ROW LEVEL SECURITY;
    END IF;
    IF versions_forced THEN
        ALTER TABLE memory_versions FORCE ROW LEVEL SECURITY;
    END IF;
END
$$;

COMMIT;
